package transport

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

// visibleUnconfirmedWrite publishes the record (making it visible) then reports a
// post-commit sync failure, so the append is committed-but-durability-unconfirmed — the
// exact condition the inline re-confirmation must handle.
func visibleUnconfirmedWrite(path string, data []byte, perm os.FileMode) error {
	_ = atomicfile.Write(path, data, perm)
	return &atomicfile.PostCommitSyncError{Path: path, Err: errors.New("dir sync failed")}
}

func failingSync(string) error { return errors.New("dir sync persistently fails") }
func passingSync(string) error { return nil }

// --- TESTS-outcome confirmation path ---

// A visible-but-unconfirmed outcome append that the inline re-confirm rescues commits
// durably: the result carries the authoritative revision with no error.
func TestTestOutcomeDurabilityReconfirmed(t *testing.T) {
	store, rev := runAtTests(t, 2)
	store.WithWrite(visibleUnconfirmedWrite).WithSyncDir(passingSync)
	res, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, passToVerify()), rev, evDigest)
	if err != nil {
		t.Fatalf("inline re-confirm should rescue a visible append: %v", err)
	}
	if res.Revision != rev+1 {
		t.Fatalf("revision = %d, want %d", res.Revision, rev+1)
	}
	if rs, _, _ := store.Load(); rs.Phase != state.PhaseVerify {
		t.Fatalf("outcome not applied: %+v", rs)
	}
}

// When the inline re-confirm persistently fails, the primitive preserves the
// committed/visible revision with the durability error — never a proven-uncommitted zero
// that would invite a retry of an already-applied ownerless transition.
func TestTestOutcomeDurabilityPersistentFailurePreservesRevision(t *testing.T) {
	store, rev := runAtTests(t, 2)
	store.WithWrite(visibleUnconfirmedWrite).WithSyncDir(failingSync)
	res, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, passToVerify()), rev, evDigest)
	if !genstore.IsDurabilityUnconfirmed(err) {
		t.Fatalf("persistent-fail err = %v, want IsDurabilityUnconfirmed", err)
	}
	if res.Revision != rev+1 {
		t.Fatalf("committed revision not preserved: got %d, want %d", res.Revision, rev+1)
	}
	if rs, _, _ := store.Load(); rs.Phase != state.PhaseVerify {
		t.Fatalf("outcome not visible after a durability-unconfirmed commit: %+v", rs)
	}
}

// --- agent-submit confirmation path ---

// A visible-but-unconfirmed accepting append that the inline re-confirm rescues is a clean
// acceptance: the receipt carries the committed revision with no error.
func TestSubmitDurabilityReconfirmed(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	store.WithWrite(visibleUnconfirmedWrite).WithSyncDir(passingSync)
	res, err := submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.prep())
	if err != nil {
		t.Fatalf("inline re-confirm should rescue a visible submit: %v", err)
	}
	if res.Idempotent || res.Receipt.Revision != rev+1 {
		t.Fatalf("receipt = %+v idem=%v", res.Receipt, res.Idempotent)
	}
}

// A persistent re-confirm failure preserves the committed acceptance (receipt + durability
// error), never a proven-uncommitted reject that would orphan the artifact and let an
// identical retry double-apply.
func TestSubmitDurabilityPersistentFailurePreservesReceipt(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	store.WithWrite(visibleUnconfirmedWrite).WithSyncDir(failingSync)
	res, err := submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.prep())
	if !genstore.IsDurabilityUnconfirmed(err) {
		t.Fatalf("persistent-fail err = %v, want IsDurabilityUnconfirmed", err)
	}
	if res.Receipt.Revision != rev+1 {
		t.Fatalf("committed receipt not preserved: %+v", res.Receipt)
	}
	if loaded, _, _ := store.Load(); loaded.Assignment == nil || loaded.Assignment.ID != "turn-2" {
		t.Fatalf("submit not visible after a durability-unconfirmed commit: %+v", loaded)
	}
}

// A replay after a visible-but-unconfirmed accepting append must RE-CONFIRM the run-state
// durability, not report a durable idempotent receipt whose state entry was never synced.
// The prior append was durable here; injecting a failing sync proves the replay path now
// runs the state barrier and surfaces its failure rather than a false durable success.
func TestSubmitReplayReconfirmsState(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	first, err := submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.prep())
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	store.WithSyncDir(failingSync)
	res, err := submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.prep())
	// The replay ran the state barrier and surfaced its failure (a raw ConfirmDurable error,
	// not a PostCommitSyncError — no append happened on a replay); without the re-confirm it
	// would have reported a false durable idempotent success (nil error).
	if err == nil {
		t.Fatal("replay must re-confirm the run-state durability and surface the confirm failure")
	}
	if !res.Idempotent || res.Receipt != first.Receipt {
		t.Fatalf("replay receipt = %+v idem=%v, want the preserved receipt %+v", res.Receipt, res.Idempotent, first.Receipt)
	}
	if adv.calls.Load() != 1 {
		t.Fatalf("replay re-ran the transition (%d)", adv.calls.Load())
	}
}
