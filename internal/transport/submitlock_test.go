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
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/redact"
	"github.com/David-c0degeek/claudex/internal/state"
)

// advApply is a running transition body that issues turn-2 (used by the id-collision
// and prepare-isolation tests, which declare the id explicitly).
func advApply(gen uint64, next *state.RunState) error {
	next.Phase = state.PhaseCheckpoint
	next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	bindEvidence(next, gen)
	return nil
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
	deps := depsWith(store, sink, journal, openRunRegistry(store), checkpointPrep())
	if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRecoveryRequired) {
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
	deps := depsWith(store, sink, journal, openRunRegistry(store), checkpointPrep())
	if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRecoveryRequired) {
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
	deps := depsWith(store, sink, terminalJournal(store), reg, checkpointPrep())
	if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRunMismatch) {
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
	deps := depsWith(store, sink, terminalJournal(store), reg, checkpointPrep())
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
	if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); err == nil {
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
	if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrTransitionInvalid) {
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
	if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); err != nil {
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
	res, err := submit(store, sink, "sess-2", raw, ownerAuth("sess-2"), checkpointPrep())
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
	res, err := submit(store, newMemSink(), "sess-2", report("turn-1", rev, "did it"), ownerAuth("sess-2"), checkpointPrep())
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
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
	// The context is cancelled during Put, but the append completes exactly once.
	res, err := Submit(ctx, deps, pairSess, report("turn-1", rev, "x"))
	if err != nil {
		t.Fatalf("submit cancelled after Put: %v", err)
	}
	loaded, _, _ := store.Load()
	if at, ok := loaded.AcceptedTurns["turn-1"]; !ok || at.Receipt != res.Receipt {
		t.Fatalf("acceptance did not complete after a post-Put cancellation")
	}
}

// --- role authorization (fresh + replay) ---

// --- the git-participant requirement (schema v6, pin C) ---

// implRaw builds an implementation_report artifact (the IMPLEMENT_STEP/FIX turn type).
func implRaw(turnID string, rev uint64) []byte {
	return []byte(fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":%q,"state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"files_changed":["a.go"],"deviations_from_plan":[],"notes":"n"}`, turnID, rev))
}

// The STANDALONE transport path refuses an IMPLEMENT_STEP or FIX submit outright: the
// acceptance requires git-commit evidence only the coordinator's git transaction
// supplies, so the low-level bypass fails closed after authorization and before
// Prepare, the sink, or any state mutation.
func TestSubmitImplementationRequiresGitParticipant(t *testing.T) {
	fixture := func(t *testing.T, toFix bool) (*state.Store, uint64) {
		t.Helper()
		dir := t.TempDir()
		store := state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
		r1, err := store.Mutate(0, func(_ uint64, n *state.RunState) error { initValid(n); return nil })
		if err != nil {
			t.Fatalf("init: %v", err)
		}
		implRev := driveAgreedImplement(t, store, r1.Revision)
		if toFix {
			implRev = mutate(t, store, implRev, func(_ uint64, n *state.RunState) {
				n.Phase = state.PhaseFix
				n.FixReturn = state.PhaseCheckpoint
				n.Counters.StepFixes[0] = 1
			})
		}
		r2, err := store.Mutate(implRev, func(gen uint64, n *state.RunState) error {
			n.Assignment = &state.Ref{ID: "turn-1", IssuedRevision: gen}
			bindEvidence(n, gen)
			return nil
		})
		if err != nil {
			t.Fatalf("assign: %v", err)
		}
		initRegistry(t, store)
		return store, r2.Revision
	}
	for _, tc := range []struct {
		name  string
		toFix bool
	}{{"IMPLEMENT_STEP", false}, {"FIX", true}} {
		t.Run(tc.name, func(t *testing.T) {
			store, rev := fixture(t, tc.toFix)
			sink := &countingSink{}
			called := false
			prep := func(state.RunState, PreparedSubmit) (PreparedTransition, error) {
				called = true
				return checkpointPrep()(state.RunState{}, PreparedSubmit{})
			}
			deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), prep)
			_, err := Submit(context.Background(), deps, leadSess, implRaw("turn-1", rev))
			if !errors.Is(err, ErrGitParticipantRequired) {
				t.Fatalf("%s bypass err = %v, want ErrGitParticipantRequired", tc.name, err)
			}
			if called {
				t.Fatalf("%s bypass reached Prepare", tc.name)
			}
			if sink.calls != 0 {
				t.Fatalf("%s bypass wrote to the sink", tc.name)
			}
			if loaded, _, _ := store.Load(); loaded.Revision != rev {
				t.Fatalf("%s bypass advanced the run", tc.name)
			}
		})
	}
}

// The current lead session cannot submit the pair's CHECKPOINT turn.
func TestSubmitWrongRoleFresh(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
	if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, "x")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong-role fresh err = %v, want ErrUnauthorized", err)
	}
	if sink.count() != 0 {
		t.Fatalf("a wrong-role fresh submit wrote to the sink")
	}
}

// After the pair's review is accepted, a session in the other role cannot replay the
// accepted turn to steal its receipt.
// spySink counts Put CALLS (memSink.count() cannot distinguish an idempotent
// re-confirm from an untouched sink).
type spySink struct {
	puts  int
	inner *memSink
}

func (s *spySink) Put(turnID, digest string, canonical []byte) error {
	s.puts++
	return s.inner.Put(turnID, digest, canonical)
}

func TestSubmitWrongRoleReplay(t *testing.T) {
	// Role authorization precedes both same-digest replay and different-digest
	// conflict, so a wrong-slot session never touches the sink either way.
	for _, tc := range []struct{ name, body string }{
		{"same digest", "did it"},
		{"different digest", "tampered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, rev := newRunWithActiveTurn(t)
			sink := &spySink{inner: newMemSink()}
			if _, err := submit(store, sink, "sess-2", report("turn-1", rev, "did it"), ownerAuth("sess-2"), checkpointPrep()); err != nil {
				t.Fatalf("pair accept: %v", err)
			}
			putsAfterAccept := sink.puts
			deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
			if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, tc.body)); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("wrong-role replay err = %v, want ErrUnauthorized", err)
			}
			if sink.puts != putsAfterAccept {
				t.Fatalf("a wrong-role replay touched the sink (%d -> %d)", putsAfterAccept, sink.puts)
			}
		})
	}
}

// A transition that nils the accepted-turns map and returns nil must be rejected
// before the receipt insertion (no panic), leave an orphan publication, and release
// the guard so a subsequent valid submit re-confirms and accepts.
func TestSubmitTransitionNilsAcceptedMap(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	nilMap := prepareIssuing("turn-2", "", func(gen uint64, next *state.RunState) error {
		next.AcceptedTurns = nil
		next.Phase = state.PhaseCheckpoint
		next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
		bindEvidence(next, gen)
		return nil
	})
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), nilMap)
	if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("nil-map transition err = %v, want ErrTransitionInvalid", err)
	}
	if loaded, _, _ := store.Load(); loaded.Revision != rev {
		t.Fatalf("a rejected nil-map transition still advanced the run")
	}
	// The guard was released: a fresh valid submit re-confirms the orphan and accepts.
	res, err := submit(store, sink, "sess-2", report("turn-1", rev, "x"), ownerAuth("sess-2"), checkpointPrep())
	if err != nil {
		t.Fatalf("fresh submit after the nil-map orphan: %v", err)
	}
	if res.Receipt.Revision != rev+1 {
		t.Fatalf("the fresh submit did not accept at rev+1: %+v", res.Receipt)
	}
}

// --- journal fail-closed on zero / invalid enum ---

func TestSubmitJournalZeroAndInvalidRecoveryRequired(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	for _, head := range []JournalHead{JournalUnknown, JournalHead(99)} {
		sink := newMemSink()
		deps := depsWith(store, sink, fakeJournal{lockPath: store.LockPath(), head: head}, openRunRegistry(store), checkpointPrep())
		if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("journal head %d err = %v, want ErrRecoveryRequired", head, err)
		}
		if sink.count() != 0 {
			t.Fatalf("journal head %d wrote to the sink", head)
		}
	}
}

// --- cancellation between Prepare and Put ---

func TestSubmitCancelBeforePut(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	ctx, cancel := context.WithCancel(context.Background())
	sink := newMemSink()
	// Prepare cancels the context just before returning; the pre-Put recheck catches it.
	prep := func(state.RunState, PreparedSubmit) (PreparedTransition, error) {
		cancel()
		return NewPreparedTransition("turn-2", "", advApply), nil
	}
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), prep)
	if _, err := Submit(ctx, deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel-before-put err = %v, want context.Canceled", err)
	}
	if sink.count() != 0 {
		t.Fatalf("a cancelled-before-put submit published, count = %d", sink.count())
	}
	if loaded, _, _ := store.Load(); loaded.Revision != rev {
		t.Fatalf("a cancelled-before-put submit advanced the run")
	}
}

// --- submit-wins linearization ---

type blockingSink struct {
	inner   *memSink
	entered chan struct{}
	release chan struct{}
}

func (s *blockingSink) Put(turnID, digest string, canonical []byte) error {
	close(s.entered)
	<-s.release
	return s.inner.Put(turnID, digest, canonical)
}

// While a submit holds the run guard (blocked inside Put), a concurrent registry
// replacement cannot acquire the lock; once the submit completes and releases, the
// replacement proceeds — the two are totally ordered by the run guard.
func TestSubmitWinsThenReplacementProceeds(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	reg := openRunRegistry(store)
	sink := &blockingSink{inner: newMemSink(), entered: make(chan struct{}), release: make(chan struct{})}
	deps := depsWith(store, sink, terminalJournal(store), reg, checkpointPrep())

	done := make(chan error, 1)
	go func() {
		_, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x"))
		done <- err
	}()
	<-sink.entered // the submit is inside Put, holding the run guard

	regBefore, _, _ := reg.Load()
	newLead := "sess-" + strings.Repeat("d", 32)
	replace := func(gen uint64, n *state.Registry) error {
		n.Lead.Sessions = append(n.Lead.Sessions, state.SessionRecord{SessionID: newLead, Generation: 2, IssuedRegistryRevision: gen})
		n.Lead.CurrentSessionID = newLead
		return nil
	}
	if _, err := reg.Mutate(regBefore.Revision, replace); !errors.Is(err, genstore.ErrBusy) {
		t.Fatalf("replacement while the submit holds the guard err = %v, want ErrBusy", err)
	}
	if after, _, _ := reg.Load(); after.Revision != regBefore.Revision {
		t.Fatalf("a busy replacement still mutated the registry")
	}

	close(sink.release) // let the submit publish + accept
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}
	if loaded, _, _ := store.Load(); loaded.Revision != rev+1 {
		t.Fatalf("the submit did not accept")
	}
	// The replacement now succeeds after the submit released the guard.
	if _, err := reg.Mutate(regBefore.Revision, replace); err != nil {
		t.Fatalf("replacement after the submit released: %v", err)
	}
}

// emptyRegistry opens an uninitialized registry handle sharing the store's run lock.
func emptyRegistry(store *state.Store) *state.RegistryStore {
	return state.OpenRegistry(filepath.Join(runDir(store), "empty-registry"), store.LockPath())
}

// The journal is classified BEFORE the registry, so a crash that left the run state
// present, the attach journal nonterminal/unreadable, and the registry not yet
// created routes to recovery rather than a permanent run mismatch.
func TestSubmitJournalPrecedesRegistry(t *testing.T) {
	t.Run("nonterminal journal + empty registry", func(t *testing.T) {
		store, rev := newRunWithActiveTurn(t)
		sink := newMemSink()
		deps := depsWith(store, sink, fakeJournal{lockPath: store.LockPath(), head: JournalNonterminal}, emptyRegistry(store), checkpointPrep())
		if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("err = %v, want ErrRecoveryRequired", err)
		}
		if sink.count() != 0 {
			t.Fatalf("sink touched")
		}
	})
	t.Run("journal read error + empty registry", func(t *testing.T) {
		store, rev := newRunWithActiveTurn(t)
		sink := newMemSink()
		deps := depsWith(store, sink, fakeJournal{lockPath: store.LockPath(), err: errors.New("corrupt")}, emptyRegistry(store), checkpointPrep())
		if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("err = %v, want ErrRecoveryRequired", err)
		}
		if sink.count() != 0 {
			t.Fatalf("sink touched")
		}
	})
	t.Run("terminal journal + empty registry", func(t *testing.T) {
		store, rev := newRunWithActiveTurn(t)
		sink := newMemSink()
		deps := depsWith(store, sink, terminalJournal(store), emptyRegistry(store), checkpointPrep())
		if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRunMismatch) {
			t.Fatalf("err = %v, want ErrRunMismatch", err)
		}
		if sink.count() != 0 {
			t.Fatalf("sink touched")
		}
	})
}

// A declared gate id that collides with an accepted turn is rejected before Put
// (turn and gate ids share one canonical namespace).
func TestSubmitIssuedGateCollisionRecheck(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	// "plan-turn" is an accepted turn; declaring it as the issued gate id collides.
	prep := prepareIssuing("", "plan-turn", func(uint64, *state.RunState) error { return nil })
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), prep)
	if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("gate collision err = %v, want ErrTransitionInvalid", err)
	}
	if sink.count() != 0 {
		t.Fatalf("sink written despite a gate-id collision")
	}
}

// An Apply that directly returns an error after Put leaves exactly one orphan,
// releases the guard, and a later valid submit re-confirms and accepts.
func TestSubmitApplyErrorLeavesOneOrphan(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	raw := report("turn-1", rev, "did it")
	errApply := prepareIssuing("turn-2", "", func(uint64, *state.RunState) error {
		return errors.New("apply boom")
	})
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), errApply)
	if _, err := Submit(context.Background(), deps, pairSess, raw); err == nil {
		t.Fatalf("an apply error should reject the submit")
	}
	if sink.count() != 1 {
		t.Fatalf("apply error left %d artifacts, want exactly one orphan", sink.count())
	}
	if loaded, _, _ := store.Load(); loaded.Revision != rev {
		t.Fatalf("an apply error still advanced the run")
	}
	// The guard was released: a fresh valid submit re-confirms the orphan and accepts.
	res, err := submit(store, sink, "sess-2", raw, ownerAuth("sess-2"), checkpointPrep())
	if err != nil {
		t.Fatalf("fresh submit after the apply-error orphan: %v", err)
	}
	if res.Receipt.Revision != rev+1 || sink.count() != 1 {
		t.Fatalf("fresh submit did not re-confirm+accept: rev=%d sink=%d", res.Receipt.Revision, sink.count())
	}
}
