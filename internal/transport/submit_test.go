package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

// --- fakes ---

type memSink struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemSink() *memSink { return &memSink{m: map[string][]byte{}} }

func skey(turnID, digest string) string { return turnID + "\x00" + digest }

func (s *memSink) Put(turnID, digest string, canonical []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := skey(turnID, digest)
	if existing, ok := s.m[key]; ok {
		if !bytes.Equal(existing, canonical) {
			return errors.New("sink: digest collision")
		}
		return nil
	}
	s.m[key] = append([]byte(nil), canonical...)
	return nil
}

func (s *memSink) get(turnID, digest string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[skey(turnID, digest)]
}

func (s *memSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func ownerAuth(owner string) Authorizer {
	return func(_ state.RunState, sessionID, _ string) error {
		if sessionID != owner {
			return errors.New("transport: session does not own this turn")
		}
		return nil
	}
}

type advancer struct{ calls atomic.Int64 }

func (a *advancer) fn(_ PreparedSubmit, gen uint64, next *state.RunState) error {
	a.calls.Add(1)
	next.Phase = state.PhaseCheckpoint
	next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	return nil
}

// --- state fixture ---

func initValid(n *state.RunState) {
	n.RunID = "run-a"
	n.Lifecycle = state.LifecycleRunning
	n.Phase = state.PhaseInit
	n.CreatedUnix = 1000
	n.TaskSnapshot = state.SnapshotRef{RelPath: "inputs/task.json", Digest: strings.Repeat("a", 64)}
	n.PolicySnapshot = state.SnapshotRef{RelPath: "inputs/policy.json", Digest: strings.Repeat("b", 64)}
	pol := config.DefaultRunPolicy()
	pol.TestGate = config.TestGate{Disabled: true}
	n.EffectivePolicy = pol
	n.FS = state.FSResult{Class: "supported-local", Reason: "local fixed drive"}
	n.Base = pol.BaseBranch
	n.BaseCommit = strings.Repeat("a", 40)
	n.WorktreeRelPath = ".claudex/runs/run-a/worktree"
	n.RunBranch = "claudex/run-a"
}

func newRunWithActiveTurn(t *testing.T) (*state.Store, uint64) {
	t.Helper()
	dir := t.TempDir()
	store := state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
	r1, err := store.Mutate(0, func(_ uint64, n *state.RunState) error { initValid(n); return nil })
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	implRev := driveAgreedImplement(t, store, r1.Revision)
	r2, err := store.Mutate(implRev, func(gen uint64, n *state.RunState) error {
		n.Assignment = &state.Ref{ID: "turn-1", IssuedRevision: gen}
		return nil
	})
	if err != nil {
		t.Fatalf("assign turn-1: %v", err)
	}
	initRegistry(t, store)
	return store, r2.Revision
}

func report(turnID string, rev uint64, notes string) []byte {
	return []byte(fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":%q,"state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"files_changed":["a.go"],"deviations_from_plan":[],"notes":%q}`, turnID, rev, notes))
}

// submit is a legacy-shaped shim: it builds the locked-Submit dependencies (the
// fixture registry + a terminal journal) and adapts the transition, so existing
// tests keep their call shape. Authorization is definitive via the registry, so the
// passed Authorizer is dropped (it was never definitive); a nil sink or transition
// still surfaces ErrMissingSeam.
func submit(store *state.Store, sink ArtifactSink, sess string, raw []byte, _ Authorizer, adv Transition) (SubmitResult, error) {
	var prep Prepare
	if adv != nil {
		prep = adaptTransition(adv)
	}
	return Submit(context.Background(), submitDeps(store, sink, prep), canonSess(sess), raw)
}

func TestSubmitAccepts(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	res, err := submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Idempotent || res.Receipt.TurnID != "turn-1" || res.Receipt.Revision != rev+1 {
		t.Fatalf("receipt = %+v idem=%v", res.Receipt, res.Idempotent)
	}
	stored := sink.get("turn-1", res.Receipt.ArtifactDigest)
	if stored == nil {
		t.Fatalf("artifact not persisted")
	}
	sum := sha256.Sum256(stored)
	if res.Receipt.ArtifactDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest is not over the stored canonical bytes")
	}
	if adv.calls.Load() != 1 {
		t.Fatalf("transition ran %d times, want 1", adv.calls.Load())
	}
	loaded, _, _ := store.Load()
	if loaded.Phase != state.PhaseCheckpoint || loaded.Assignment.ID != "turn-2" {
		t.Fatalf("transition did not advance: %s / %+v", loaded.Phase, loaded.Assignment)
	}
}

func TestSubmitIdempotent(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	first, err := submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	afterFirst, _, _ := store.Load()
	second, err := submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Idempotent || second.Receipt != first.Receipt {
		t.Fatalf("replay not idempotent")
	}
	afterSecond, _, _ := store.Load()
	if afterSecond.Revision != afterFirst.Revision || adv.calls.Load() != 1 {
		t.Fatalf("replay wrote a generation or reran the transition")
	}
}

func TestSubmitSecretOnlyDifferenceCollapses(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	a := report("turn-1", rev, "used token=sk-ant-aaaaaaaaaaaaaaaaaaaaaaaa here")
	b := report("turn-1", rev, "used token=sk-ant-bbbbbbbbbbbbbbbbbbbbbbbb here")
	ra, err := submit(store, sink, "sess-1", a, ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("submit a: %v", err)
	}
	if strings.Contains(string(sink.get("turn-1", ra.Receipt.ArtifactDigest)), "sk-ant-") {
		t.Fatalf("secret survived redaction in the sink")
	}
	rb, err := submit(store, sink, "sess-1", b, ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("submit b: %v", err)
	}
	if !rb.Idempotent || rb.Receipt != ra.Receipt {
		t.Fatalf("secret-only difference did not collapse")
	}
}

func TestSubmitConflictDoesNotRerunTransition(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	if _, err := submit(store, sink, "sess-1", report("turn-1", rev, "first"), ownerAuth("sess-1"), adv.fn); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := submit(store, sink, "sess-1", report("turn-1", rev, "different"), ownerAuth("sess-1"), adv.fn); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if adv.calls.Load() != 1 {
		t.Fatalf("transition reran on conflict (%d)", adv.calls.Load())
	}
}

func TestSubmitStaleCarriesCurrentStatus(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	adv := &advancer{}
	_, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev-1, "x"), ownerAuth("sess-1"), adv.fn)
	var se *StaleError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StaleError", err)
	}
	if se.CurrentRevision != rev || se.CurrentTurnID != "turn-1" || adv.calls.Load() != 0 {
		t.Fatalf("stale error wrong or transition ran: %+v calls=%d", se, adv.calls.Load())
	}
}

// After a reissue that does not accept this turn, revision staleness must win
// over the turn-identity check, so the caller still gets typed current status.
func TestStaleBeatsWrongTurnOnReissue(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	// Reissue turn-2 at rev+1 without accepting turn-1 (staying at IMPLEMENT_STEP).
	if _, err := store.Mutate(rev, func(gen uint64, n *state.RunState) error {
		n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
		return nil
	}); err != nil {
		t.Fatalf("reissue: %v", err)
	}
	_, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), (&advancer{}).fn)
	var se *StaleError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StaleError (revision must be checked before turn)", err)
	}
	if se.CurrentTurnID != "turn-2" {
		t.Fatalf("stale error current turn = %q, want turn-2", se.CurrentTurnID)
	}
}

func TestSubmitWrongTurn(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	if _, err := submit(store, newMemSink(), "sess-1", report("turn-999", rev, "x"), ownerAuth("sess-1"), (&advancer{}).fn); !errors.Is(err, ErrWrongTurn) {
		t.Fatalf("err = %v, want ErrWrongTurn", err)
	}
}

func TestSubmitSchemaInvalid(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	bad := fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":"turn-1","state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"deviations_from_plan":[],"notes":"n"}`, rev)
	if _, err := submit(store, newMemSink(), "sess-1", []byte(bad), ownerAuth("sess-1"), (&advancer{}).fn); err == nil {
		t.Fatalf("schema-invalid submission should be rejected")
	}
	wrong := fmt.Sprintf(`{"protocol_version":1,"message_type":"plan","turn_id":"turn-1","state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"plan_markdown":"x","steps":[],"risks":[],"open_questions":[]}`, rev)
	if _, err := submit(store, newMemSink(), "sess-1", []byte(wrong), ownerAuth("sess-1"), (&advancer{}).fn); err == nil {
		t.Fatalf("wrong message_type should be rejected")
	}
}

func TestSubmitDecisionInconsistent(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	trueNull := fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":"turn-1","state_revision":%d,"human_context":null,"requires_human_decision":true,"decision_question":null,"files_changed":["a.go"],"deviations_from_plan":[],"notes":"n"}`, rev)
	if _, err := submit(store, newMemSink(), "sess-1", []byte(trueNull), ownerAuth("sess-1"), (&advancer{}).fn); !errors.Is(err, ErrDecisionInconsistent) {
		t.Fatalf("true+null want ErrDecisionInconsistent, got %v", err)
	}
	falseEmpty := fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":"turn-1","state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":"","files_changed":["a.go"],"deviations_from_plan":[],"notes":"n"}`, rev)
	if _, err := submit(store, newMemSink(), "sess-1", []byte(falseEmpty), ownerAuth("sess-1"), (&advancer{}).fn); !errors.Is(err, ErrDecisionInconsistent) {
		t.Fatalf("false+empty want ErrDecisionInconsistent, got %v", err)
	}
}

func TestSubmitUnauthorizedFresh(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	before, _, _ := store.Load()
	if _, err := submit(store, sink, "intruder", report("turn-1", rev, "x"), ownerAuth("sess-1"), adv.fn); err == nil {
		t.Fatalf("unauthorized submit should be rejected")
	}
	after, _, _ := store.Load()
	if sink.count() != 0 || adv.calls.Load() != 0 || after.Revision != before.Revision {
		t.Fatalf("unauthorized submit had an effect")
	}
}

// Authorization guards the idempotent replay path too.
func TestSubmitUnauthorizedIdempotent(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	if _, err := submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), (&advancer{}).fn); err != nil {
		t.Fatalf("accept: %v", err)
	}
	// An intruder who knows the accepted bytes must not get a receipt.
	if _, err := submit(store, sink, "intruder", report("turn-1", rev, "did it"), ownerAuth("sess-1"), (&advancer{}).fn); err == nil {
		t.Fatalf("unauthorized replay should be rejected")
	}
}

func TestNoOpTransitionRejected(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	noop := func(_ PreparedSubmit, _ uint64, _ *state.RunState) error { return nil }
	if _, err := submit(store, sink, "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), noop); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("err = %v, want ErrTransitionInvalid", err)
	}
	loaded, _, _ := store.Load()
	if _, ok := loaded.AcceptedTurns["turn-1"]; ok {
		t.Fatalf("a no-op transition still recorded acceptance")
	}
}

func TestTransitionReissuingSameTurnRejected(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sameTurn := func(_ PreparedSubmit, gen uint64, next *state.RunState) error {
		next.Assignment = &state.Ref{ID: "turn-1", IssuedRevision: gen}
		return nil
	}
	if _, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), sameTurn); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("err = %v, want ErrTransitionInvalid", err)
	}
}

// A transition that tries to delete the acceptance cannot: it is installed last.
func TestTransitionCannotEraseAcceptance(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	malicious := func(_ PreparedSubmit, gen uint64, next *state.RunState) error {
		delete(next.AcceptedTurns, "turn-1")
		next.Phase = state.PhaseCheckpoint
		next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
		return nil
	}
	res, err := submit(store, sink, "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), malicious)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	loaded, _, _ := store.Load()
	if at, ok := loaded.AcceptedTurns["turn-1"]; !ok || at.Receipt != res.Receipt {
		t.Fatalf("acceptance was erased by the transition")
	}
}

func TestTransitionReceivesPreparedSubmit(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	var got PreparedSubmit
	capture := func(p PreparedSubmit, gen uint64, next *state.RunState) error {
		got = p
		next.Phase = state.PhaseCheckpoint
		next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
		return nil
	}
	if _, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), capture); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got.MessageType != "implementation_report" || got.TurnID != "turn-1" || got.Revision != rev || got.CanonicalJSON == "" {
		t.Fatalf("prepared submit not populated: %+v", got)
	}
	// The immutable canonical string matches the digest handed to the callback.
	sum := sha256.Sum256([]byte(got.CanonicalJSON))
	if hex.EncodeToString(sum[:]) != got.Digest {
		t.Fatalf("prepared canonical/digest mismatch")
	}
}

// Clearing the assignment without leaving an agent phase strands the run and
// must be rejected.
func TestClearWithoutAdvanceRejected(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	clearOnly := func(_ PreparedSubmit, _ uint64, next *state.RunState) error {
		next.Assignment = nil // phase stays IMPLEMENT_STEP (actionable)
		return nil
	}
	if _, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), clearOnly); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("err = %v, want ErrTransitionInvalid", err)
	}
}

// Parking at a human gate requires a paused lifecycle and a gate issued now.
func TestClearToHumanGateAccepted(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	toGate := func(p PreparedSubmit, gen uint64, next *state.RunState) error {
		// The just-accepted turn-1 is the event that requested the decision; Submit
		// records it after this callback, so the source references it by digest.
		humanGate(next, gen, state.PhaseImplementStep, p.TurnID, p.Digest, "gate-1")
		return nil
	}
	if _, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), toGate); err != nil {
		t.Fatalf("parking at a valid human gate should be accepted: %v", err)
	}
	loaded, _, _ := store.Load()
	if loaded.Phase != state.PhaseAwaitGuidance || loaded.Gate == nil || loaded.Lifecycle != state.LifecyclePaused {
		t.Fatalf("run not parked at the gate: %+v", loaded)
	}
}

// A terminal (DONE/completed) shape with no assignment is a valid live owner.
func TestClearToTerminalAccepted(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	toDone := func(_ PreparedSubmit, _ uint64, next *state.RunState) error {
		next.Phase = state.PhaseDone
		next.Lifecycle = state.LifecycleCompleted
		idx := 1 // the single step is done
		next.StepIndex = &idx
		next.Assignment = nil
		return nil
	}
	if _, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), toDone); err != nil {
		t.Fatalf("completing the run should be accepted: %v", err)
	}
}

// Ownerless and inconsistent shapes are rejected.
func TestOwnerlessShapesRejected(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	cases := map[string]Transition{
		"init + nil": func(_ PreparedSubmit, _ uint64, next *state.RunState) error {
			next.Phase = state.PhaseInit
			next.Assignment = nil
			return nil
		},
		"await-guidance without a gate": func(_ PreparedSubmit, _ uint64, next *state.RunState) error {
			next.Phase = state.PhaseAwaitGuidance
			next.Assignment = nil // no gate, lifecycle still running
			return nil
		},
		"assignment under a terminal lifecycle": func(_ PreparedSubmit, gen uint64, next *state.RunState) error {
			next.Phase = state.PhaseCheckpoint
			next.Lifecycle = state.LifecycleCompleted
			next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
			return nil
		},
		"assignment in a non-agent phase": func(_ PreparedSubmit, gen uint64, next *state.RunState) error {
			next.Phase = state.PhaseDone
			next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
			return nil
		},
	}
	for name, adv := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), adv); !errors.Is(err, ErrTransitionInvalid) {
				t.Fatalf("%s err = %v, want ErrTransitionInvalid", name, err)
			}
		})
	}
}

// A raw parse error must not echo a secret-shaped duplicate key.
func TestSubmitRawParseErrorRedacted(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	secretKey := "sk-ant-abcdefghijklmnopqrstuvwx"
	// A duplicate key (canonjson rejects it) whose name is a secret.
	dup := fmt.Sprintf(`{"%s":1,"%s":2,"turn_id":"turn-1","state_revision":%d}`, secretKey, secretKey, rev)
	_, err := submit(store, newMemSink(), "sess-1", []byte(dup), ownerAuth("sess-1"), (&advancer{}).fn)
	if err == nil {
		t.Fatalf("a duplicate-key submission should be rejected")
	}
	if strings.Contains(err.Error(), "sk-ant-") {
		t.Fatalf("raw parse error leaked a secret: %v", err)
	}
}

// A trusted-but-fallible authorizer that mutates the state value it receives
// cannot fabricate an idempotent acceptance: authority is captured first.
func TestAuthorizerCannotInjectAcceptance(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	inject := func(rs state.RunState, _ string, turnID string) error {
		rs.AcceptedTurns[turnID] = state.AcceptedTurn{
			ArtifactDigest: "fabricated",
			Receipt:        state.Receipt{TurnID: turnID, Revision: 999, ArtifactDigest: "fabricated"},
		}
		return nil
	}
	res, err := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "x"), inject, (&advancer{}).fn)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Idempotent || res.Receipt.Revision != rev+1 || res.Receipt.ArtifactDigest == "fabricated" {
		t.Fatalf("authorizer injection influenced classification: %+v", res)
	}
}

func TestSubmitRequiresSeams(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	r := report("turn-1", rev, "x")
	// The store/registry/journal/sink/prepare are all required; the preflight
	// Authorizer is optional (never definitive), so a nil authorizer is not an error.
	if _, err := submit(store, nil, "s", r, ownerAuth("s"), (&advancer{}).fn); !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("nil sink")
	}
	if _, err := submit(store, newMemSink(), "s", r, ownerAuth("s"), nil); !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("nil transition")
	}
	// A lock-path mismatch across the dependencies fails closed.
	bad := SubmitDeps{Store: store, Registry: openRunRegistry(store), Journal: fakeJournal{lockPath: "/somewhere/else"}, Sink: newMemSink(), Prepare: adaptTransition((&advancer{}).fn)}
	if _, err := Submit(context.Background(), bad, leadSess, r); !errors.Is(err, ErrLockMismatch) {
		t.Fatalf("lock mismatch err = %v, want ErrLockMismatch", err)
	}
}

func TestSubmitNoRun(t *testing.T) {
	dir := t.TempDir()
	store := state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
	if _, err := submit(store, newMemSink(), "s", report("turn-1", 1, "x"), ownerAuth("s"), (&advancer{}).fn); !errors.Is(err, ErrNoRun) {
		t.Fatalf("err = %v, want ErrNoRun", err)
	}
}

// A permanently held lock exhausts retries as busy, never as stale.
func TestSubmitBusyExhaustionNotStale(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	g, ok, err := genstore.Acquire(store.LockPath())
	if err != nil || !ok {
		t.Fatalf("acquire lock: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	_, serr := submit(store, newMemSink(), "sess-1", report("turn-1", rev, "x"), ownerAuth("sess-1"), (&advancer{}).fn)
	if !errors.Is(serr, genstore.ErrBusy) {
		t.Fatalf("err = %v, want genstore.ErrBusy", serr)
	}
	var se *StaleError
	if errors.As(serr, &se) {
		t.Fatalf("busy exhaustion must not be reported as stale")
	}
}

func TestSubmitContextCancelled(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deps := submitDeps(store, newMemSink(), adaptTransition((&advancer{}).fn))
	if _, err := Submit(ctx, deps, leadSess, report("turn-1", rev, "x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// releaseOutcome classifies a lock-release result before vs after a committed append.
func TestReleaseOutcome(t *testing.T) {
	if err := releaseOutcome(false, 0, nil); err != nil {
		t.Fatalf("no release error should be nil, got %v", err)
	}
	relErr := errors.New("release failed")
	// Before acceptance: an ordinary error (never looks committed).
	if err := releaseOutcome(false, 0, relErr); err != relErr {
		t.Fatalf("pre-acceptance release should be the plain error, got %v", err)
	}
	var pce *genstore.PostCommitError
	if !errors.As(releaseOutcome(false, 0, relErr), &pce) {
		// (expected: not a PostCommitError)
	} else {
		t.Fatalf("pre-acceptance release must not be a PostCommitError")
	}
	// After a committed append: a PostCommitError over the committed revision.
	out := releaseOutcome(true, 7, relErr)
	if !errors.As(out, &pce) || pce.Generation != 7 {
		t.Fatalf("post-commit release = %v, want a PostCommitError over generation 7", out)
	}
}

func TestSubmitConcurrentSameDigest(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	const n = 6
	var wg sync.WaitGroup
	results := make([]SubmitResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.fn)
		}(i)
	}
	wg.Wait()

	accepted := 0
	var receipt state.Receipt
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if !results[i].Idempotent {
			accepted++
			receipt = results[i].Receipt
		}
	}
	if accepted != 1 || adv.calls.Load() != 1 {
		t.Fatalf("want one acceptance and one transition, got accepted=%d calls=%d", accepted, adv.calls.Load())
	}
	for i := 0; i < n; i++ {
		if results[i].Receipt != receipt {
			t.Fatalf("goroutine %d receipt mismatch", i)
		}
	}
	if loaded, _, _ := store.Load(); loaded.Revision != rev+1 {
		t.Fatalf("expected one accepted generation")
	}
}

// Two concurrent submits with different digests for the same turn: they serialize
// on the run guard, so exactly one accepts and the other is rejected as a conflict
// BEFORE it can publish — the locked ordering leaves no orphan artifact at all.
func TestSubmitConcurrentDifferentDigestNoOrphan(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	var wg sync.WaitGroup
	res := make([]SubmitResult, 2)
	errs := make([]error, 2)
	bodies := []string{"first", "second"}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], errs[i] = submit(store, sink, "sess-1", report("turn-1", rev, bodies[i]), ownerAuth("sess-1"), adv.fn)
		}(i)
	}
	wg.Wait()

	accepted, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		switch {
		case errs[i] == nil && !res[i].Idempotent:
			accepted++
		case errors.Is(errs[i], ErrConflict):
			conflicts++
		default:
			t.Fatalf("goroutine %d: res=%+v err=%v", i, res[i], errs[i])
		}
	}
	if accepted != 1 || conflicts != 1 || adv.calls.Load() != 1 {
		t.Fatalf("want 1 accept + 1 conflict + 1 transition; got %d/%d/%d", accepted, conflicts, adv.calls.Load())
	}
	// The conflicting submit was rejected before publishing, so the sink holds only
	// the single accepted artifact — no orphan.
	if sink.count() != 1 {
		t.Fatalf("expected exactly the accepted artifact, sink has %d", sink.count())
	}
}
