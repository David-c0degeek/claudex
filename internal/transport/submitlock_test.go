package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/redact"
	"github.com/David-c0degeek/claudex/internal/state"
)

// advApply is the standard running transition body: advance to CHECKPOINT, issue
// turn-2. advTransition is the legacy-Transition-shaped wrapper for the shim.
func advApply(gen uint64, next *state.RunState) error {
	next.Phase = state.PhaseCheckpoint
	next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	return nil
}

func advTransition(_ PreparedSubmit, gen uint64, next *state.RunState) error {
	return advApply(gen, next)
}

func prepareIssuing(turnID, gateID string, apply func(gen uint64, next *state.RunState) error) Prepare {
	return func(state.RunState, PreparedSubmit) (PreparedTransition, error) {
		return NewPreparedTransition(turnID, gateID, apply), nil
	}
}

func prepareErr(err error) Prepare {
	return func(state.RunState, PreparedSubmit) (PreparedTransition, error) { return PreparedTransition{}, err }
}

func depsWith(store *state.Store, sink ArtifactSink, journal JournalReader, reg *state.RegistryStore, prepare Prepare) SubmitDeps {
	return SubmitDeps{Store: store, Registry: reg, Journal: journal, Sink: sink, Prepare: prepare}
}

// redactedDigest reproduces the digest a submit computes over the redacted canonical.
func redactedDigest(t *testing.T, raw []byte) ([]byte, string) {
	t.Helper()
	canon, err := canonjson.Canonicalize(raw)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	canonR, err := canonjson.Canonicalize(redact.Bytes(canon))
	if err != nil {
		t.Fatalf("canon redacted: %v", err)
	}
	sum := sha256.Sum256(canonR)
	return canonR, hex.EncodeToString(sum[:])
}

// registryWithRunID builds a registry (sharing the store lock) whose RunID differs.
func registryWithRunID(t *testing.T, store *state.Store, runID string) *state.RegistryStore {
	t.Helper()
	reg := state.OpenRegistry(filepath.Join(runDir(store), "registry-"+runID), store.LockPath())
	if _, err := reg.Mutate(0, func(gen uint64, n *state.Registry) error {
		n.RunID = runID
		n.Lead = &state.RoleSlot{Agent: state.AgentClaude, CurrentSessionID: leadSess, Sessions: []state.SessionRecord{{SessionID: leadSess, Generation: 1, IssuedRegistryRevision: gen}}}
		return nil
	}); err != nil {
		t.Fatalf("registry %s: %v", runID, err)
	}
	return reg
}

// --- journal policy ---

func TestSubmitNonterminalJournalRecoveryRequired(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	journal := fakeJournal{lockPath: store.LockPath(), head: JournalNonterminal}
	deps := depsWith(store, sink, journal, openRunRegistry(store), adaptTransition(advTransition))
	if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("nonterminal journal err = %v, want ErrRecoveryRequired", err)
	}
	if sink.count() != 0 {
		t.Fatalf("sink written despite recovery-required")
	}
}

func TestSubmitJournalReadErrorRecoveryRequired(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	journal := fakeJournal{lockPath: store.LockPath(), err: errors.New("corrupt journal")}
	deps := depsWith(store, sink, journal, openRunRegistry(store), adaptTransition(advTransition))
	if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("journal read error = %v, want ErrRecoveryRequired", err)
	}
	if sink.count() != 0 {
		t.Fatalf("sink written despite a journal read error")
	}
}

// --- run identity ---

func TestSubmitRunIDMismatch(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	reg := registryWithRunID(t, store, "run-b") // state is run-a
	deps := depsWith(store, sink, terminalJournal(store), reg, adaptTransition(advTransition))
	if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRunMismatch) {
		t.Fatalf("run id mismatch err = %v, want ErrRunMismatch", err)
	}
	if sink.count() != 0 {
		t.Fatalf("sink written despite a run id mismatch")
	}
}

// --- replacement linearization ---

func TestSubmitReplacedSessionRejected(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	reg := openRunRegistry(store)
	// A replacement wins before the submit: the lead session is superseded.
	r, _, _ := reg.Load()
	newLead := "sess-" + strings.Repeat("c", 32)
	if _, err := reg.Mutate(r.Revision, func(gen uint64, n *state.Registry) error {
		n.Lead.Sessions = append(n.Lead.Sessions, state.SessionRecord{SessionID: newLead, Generation: 2, IssuedRegistryRevision: gen})
		n.Lead.CurrentSessionID = newLead
		return nil
	}); err != nil {
		t.Fatalf("replace lead: %v", err)
	}
	deps := depsWith(store, sink, terminalJournal(store), reg, adaptTransition(advTransition))
	if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, "x")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replaced session err = %v, want ErrUnauthorized", err)
	}
	if sink.count() != 0 {
		t.Fatalf("a replaced session's submit wrote to the sink")
	}
}

// --- semantic failure after locked preparation ---

func TestSubmitSemanticFailBeforePut(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), prepareErr(errors.New("semantic reject")))
	if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, "x")); err == nil {
		t.Fatalf("a semantic Prepare failure should reject the submit")
	}
	if sink.count() != 0 {
		t.Fatalf("sink written despite a semantic reject")
	}
	if loaded, _, _ := store.Load(); loaded.Revision != rev {
		t.Fatalf("state advanced despite a semantic reject")
	}
}

// --- locked issued-id recheck ---

func TestSubmitIssuedIDCollisionRecheck(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	// "plan-turn" was accepted reaching the agreed plan; issuing it again collides.
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), prepareIssuing("plan-turn", "", advApply))
	if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, "x")); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("id collision err = %v, want ErrTransitionInvalid", err)
	}
	if sink.count() != 0 {
		t.Fatalf("sink written despite an id collision (recheck must precede Put)")
	}
}

// --- Prepare isolation ---

func TestSubmitPrepareMutationCannotAffectState(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	prep := func(snap state.RunState, _ PreparedSubmit) (PreparedTransition, error) {
		snap.Assignment = nil // mutate the snapshot; it is a copy
		snap.AgreedPlan = nil
		return NewPreparedTransition("turn-2", "", advApply), nil
	}
	deps := depsWith(store, newMemSink(), terminalJournal(store), openRunRegistry(store), prep)
	if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, "x")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	loaded, _, _ := store.Load()
	if loaded.Phase != state.PhaseCheckpoint || loaded.Assignment == nil || loaded.Assignment.ID != "turn-2" || loaded.AgreedPlan == nil {
		t.Fatalf("a Prepare snapshot mutation affected committed state: %+v", loaded)
	}
}

// --- orphan re-confirmation on a fresh submit ---

func TestSubmitReconfirmsOrphanArtifact(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	raw := report("turn-1", rev, "did it")
	canonR, dg := redactedDigest(t, raw)
	// A prior submit published the artifact but crashed before accepting it.
	if err := sink.Put("turn-1", dg, canonR); err != nil {
		t.Fatalf("orphan put: %v", err)
	}
	// A fresh submit re-confirms the durable artifact (idempotent Put) and accepts.
	res, err := submit(store, sink, "sess-1", raw, ownerAuth("sess-1"), advTransition)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if res.Idempotent {
		t.Fatalf("the orphan was never accepted, so this is a new acceptance")
	}
	if sink.count() != 1 {
		t.Fatalf("re-confirming the orphan created a duplicate, sink has %d", sink.count())
	}
	loaded, _, _ := store.Load()
	if _, ok := loaded.AcceptedTurns["turn-1"]; !ok {
		t.Fatalf("the orphan was not accepted on resubmit")
	}
}

// --- gap-skipped generation binding ---

func TestSubmitGapGenerationBinding(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	stateDir := filepath.Join(runDir(store), "state")
	if err := os.WriteFile(filepath.Join(stateDir, fmt.Sprintf("%012d.gen", rev+1)), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("occupy slot: %v", err)
	}
	res, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), advTransition)
	if err != nil {
		t.Fatalf("submit across gap: %v", err)
	}
	if res.Receipt.Revision != rev+2 {
		t.Fatalf("receipt revision = %d, want the skipped-to generation %d", res.Receipt.Revision, rev+2)
	}
	loaded, _, _ := store.Load()
	if loaded.Assignment == nil || loaded.Assignment.IssuedRevision != rev+2 {
		t.Fatalf("issued assignment not bound to the gap generation: %+v", loaded.Assignment)
	}
}

// --- context cancellation after Put is ignored ---

type cancelOnPutSink struct {
	inner  *memSink
	cancel context.CancelFunc
}

func (s *cancelOnPutSink) Put(turnID, digest string, canonical []byte) error {
	s.cancel() // cancel the submit's context at the moment of publish
	return s.inner.Put(turnID, digest, canonical)
}

func TestSubmitCancellationAfterPutIgnored(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	ctx, cancel := context.WithCancel(context.Background())
	sink := &cancelOnPutSink{inner: newMemSink(), cancel: cancel}
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), adaptTransition(advTransition))
	// The context is cancelled during Put, but the append completes exactly once.
	res, err := Submit(ctx, deps, leadSess, report("turn-1", rev, "x"))
	if err != nil {
		t.Fatalf("submit cancelled after Put: %v", err)
	}
	loaded, _, _ := store.Load()
	if at, ok := loaded.AcceptedTurns["turn-1"]; !ok || at.Receipt != res.Receipt {
		t.Fatalf("acceptance did not complete after a post-Put cancellation")
	}
}
