package transport

import (
	"bytes"
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
	"github.com/David-c0degeek/claudex/internal/state"
)

// --- fakes for the injected seams ---

type memSink struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemSink() *memSink { return &memSink{m: map[string][]byte{}} }

func (s *memSink) Put(turnID, digest string, canonical []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := turnID + "\x00" + digest
	if existing, ok := s.m[key]; ok {
		if !bytes.Equal(existing, canonical) {
			return errors.New("sink: digest collision")
		}
		return nil
	}
	s.m[key] = append([]byte(nil), canonical...)
	return nil
}

func (s *memSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func ownerAuth(owner string) Authorizer {
	return func(_ state.RunState, sessionID, _ string, _ TurnSpecEntry) error {
		if sessionID != owner {
			return errors.New("transport: session does not own this turn")
		}
		return nil
	}
}

type advancer struct{ calls atomic.Int64 }

func (a *advancer) fn(gen uint64, next *state.RunState) error {
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
}

func newRunWithActiveTurn(t *testing.T) (*state.Store, uint64) {
	t.Helper()
	dir := t.TempDir()
	store := state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
	r1, err := store.Mutate(0, func(_ uint64, n *state.RunState) error { initValid(n); return nil })
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	r2, err := store.Mutate(r1.Revision, func(gen uint64, n *state.RunState) error {
		n.Phase = state.PhaseImplementStep
		n.Assignment = &state.Ref{ID: "turn-1", IssuedRevision: gen}
		return nil
	})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	return store, r2.Revision
}

func report(turnID string, rev uint64, notes string) []byte {
	return []byte(fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":%q,"state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"files_changed":["a.go"],"deviations_from_plan":[],"notes":%q}`, turnID, rev, notes))
}

func TestSubmitAccepts(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	res, err := Submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Idempotent || res.Receipt.TurnID != "turn-1" || res.Receipt.Revision != rev+1 {
		t.Fatalf("receipt = %+v idem=%v", res.Receipt, res.Idempotent)
	}
	sum := sha256.Sum256(res.CanonicalArtifact)
	if res.Receipt.ArtifactDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest is not over the canonical artifact bytes")
	}
	if sink.count() != 1 {
		t.Fatalf("artifact not persisted; sink has %d", sink.count())
	}
	if adv.calls.Load() != 1 {
		t.Fatalf("transition ran %d times, want 1", adv.calls.Load())
	}
	loaded, _, _ := store.Load()
	if loaded.Phase != state.PhaseCheckpoint || loaded.Assignment.ID != "turn-2" {
		t.Fatalf("transition did not advance the run: %s / %+v", loaded.Phase, loaded.Assignment)
	}
}

func TestSubmitIdempotent(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	first, err := Submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	afterFirst, _, _ := store.Load()
	second, err := Submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Idempotent || second.Receipt != first.Receipt {
		t.Fatalf("replay not idempotent: %+v vs %+v", second, first)
	}
	afterSecond, _, _ := store.Load()
	if afterSecond.Revision != afterFirst.Revision {
		t.Fatalf("replay wrote a new generation")
	}
	if adv.calls.Load() != 1 {
		t.Fatalf("transition ran on an idempotent replay (%d)", adv.calls.Load())
	}
}

func TestSubmitSecretOnlyDifferenceCollapses(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	a := report("turn-1", rev, "used token=sk-ant-aaaaaaaaaaaaaaaaaaaaaaaa here")
	b := report("turn-1", rev, "used token=sk-ant-bbbbbbbbbbbbbbbbbbbbbbbb here")
	ra, err := Submit(store, sink, "sess-1", a, ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("submit a: %v", err)
	}
	if strings.Contains(string(ra.CanonicalArtifact), "sk-ant-") {
		t.Fatalf("secret survived redaction")
	}
	rb, err := Submit(store, sink, "sess-1", b, ownerAuth("sess-1"), adv.fn)
	if err != nil {
		t.Fatalf("submit b: %v", err)
	}
	if !rb.Idempotent || rb.Receipt != ra.Receipt {
		t.Fatalf("secret-only difference did not collapse")
	}
}

func TestSubmitConflict(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	if _, err := Submit(store, sink, "sess-1", report("turn-1", rev, "first"), ownerAuth("sess-1"), adv.fn); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := Submit(store, sink, "sess-1", report("turn-1", rev, "different"), ownerAuth("sess-1"), adv.fn); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestSubmitStaleCarriesCurrentStatus(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	_, err := Submit(store, sink, "sess-1", report("turn-1", rev-1, "x"), ownerAuth("sess-1"), adv.fn)
	var se *StaleError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StaleError", err)
	}
	if se.CurrentRevision != rev || se.Phase != state.PhaseImplementStep || se.CurrentTurnID != "turn-1" {
		t.Fatalf("stale error missing current status: %+v", se)
	}
}

func TestSubmitWrongTurn(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	if _, err := Submit(store, newMemSink(), "sess-1", report("turn-999", rev, "x"), ownerAuth("sess-1"), (&advancer{}).fn); !errors.Is(err, ErrWrongTurn) {
		t.Fatalf("err = %v, want ErrWrongTurn", err)
	}
}

func TestSubmitSchemaInvalid(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	bad := fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":"turn-1","state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"deviations_from_plan":[],"notes":"n"}`, rev)
	if _, err := Submit(store, newMemSink(), "sess-1", []byte(bad), ownerAuth("sess-1"), (&advancer{}).fn); err == nil {
		t.Fatalf("a schema-invalid submission should be rejected")
	}
	wrong := fmt.Sprintf(`{"protocol_version":1,"message_type":"plan","turn_id":"turn-1","state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"plan_markdown":"x","steps":[],"risks":[],"open_questions":[]}`, rev)
	if _, err := Submit(store, newMemSink(), "sess-1", []byte(wrong), ownerAuth("sess-1"), (&advancer{}).fn); err == nil {
		t.Fatalf("a wrong message_type should be rejected")
	}
}

func TestSubmitDecisionInconsistent(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	// requires true, question null.
	trueNull := fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":"turn-1","state_revision":%d,"human_context":null,"requires_human_decision":true,"decision_question":null,"files_changed":["a.go"],"deviations_from_plan":[],"notes":"n"}`, rev)
	if _, err := Submit(store, newMemSink(), "sess-1", []byte(trueNull), ownerAuth("sess-1"), (&advancer{}).fn); !errors.Is(err, ErrDecisionInconsistent) {
		t.Fatalf("true+null err = %v, want ErrDecisionInconsistent", err)
	}
	// requires false, empty-string question (not null).
	falseEmpty := fmt.Sprintf(`{"protocol_version":1,"message_type":"implementation_report","turn_id":"turn-1","state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":"","files_changed":["a.go"],"deviations_from_plan":[],"notes":"n"}`, rev)
	if _, err := Submit(store, newMemSink(), "sess-1", []byte(falseEmpty), ownerAuth("sess-1"), (&advancer{}).fn); !errors.Is(err, ErrDecisionInconsistent) {
		t.Fatalf("false+empty-string err = %v, want ErrDecisionInconsistent", err)
	}
}

func TestSubmitUnauthorizedDoesNotMutate(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	adv := &advancer{}
	before, _, _ := store.Load()
	if _, err := Submit(store, sink, "intruder", report("turn-1", rev, "x"), ownerAuth("sess-1"), adv.fn); err == nil {
		t.Fatalf("an unauthorized session should be rejected")
	}
	if sink.count() != 0 || adv.calls.Load() != 0 {
		t.Fatalf("unauthorized submit wrote an artifact or ran the transition")
	}
	after, _, _ := store.Load()
	if after.Revision != before.Revision {
		t.Fatalf("unauthorized submit mutated state")
	}
}

func TestSubmitRequiresSeams(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	r := report("turn-1", rev, "x")
	if _, err := Submit(store, nil, "s", r, ownerAuth("s"), (&advancer{}).fn); !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("nil sink should be ErrMissingSeam")
	}
	if _, err := Submit(store, newMemSink(), "s", r, nil, (&advancer{}).fn); !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("nil authorizer should be ErrMissingSeam")
	}
	if _, err := Submit(store, newMemSink(), "s", r, ownerAuth("s"), nil); !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("nil transition should be ErrMissingSeam")
	}
}

func TestSubmitNoRun(t *testing.T) {
	dir := t.TempDir()
	store := state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
	if _, err := Submit(store, newMemSink(), "s", report("turn-1", 1, "x"), ownerAuth("s"), (&advancer{}).fn); !errors.Is(err, ErrNoRun) {
		t.Fatalf("err = %v, want ErrNoRun", err)
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
			results[i], errs[i] = Submit(store, sink, "sess-1", report("turn-1", rev, "did it"), ownerAuth("sess-1"), adv.fn)
		}(i)
	}
	wg.Wait()

	accepted := 0
	var receipt state.Receipt
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d errored: %v", i, errs[i])
		}
		if !results[i].Idempotent {
			accepted++
			receipt = results[i].Receipt
		}
	}
	if accepted != 1 {
		t.Fatalf("expected exactly one fresh acceptance, got %d", accepted)
	}
	for i := 0; i < n; i++ {
		if results[i].Receipt != receipt {
			t.Fatalf("goroutine %d receipt %+v != %+v", i, results[i].Receipt, receipt)
		}
	}
	if adv.calls.Load() != 1 {
		t.Fatalf("transition ran %d times under concurrency, want 1", adv.calls.Load())
	}
	loaded, _, _ := store.Load()
	if loaded.Revision != rev+1 {
		t.Fatalf("expected one accepted generation (%d), got %d", rev+1, loaded.Revision)
	}
}

func TestSubmitConcurrentDifferentDigest(t *testing.T) {
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
			res[i], errs[i] = Submit(store, sink, "sess-1", report("turn-1", rev, bodies[i]), ownerAuth("sess-1"), adv.fn)
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
			t.Fatalf("goroutine %d: unexpected outcome res=%+v err=%v", i, res[i], errs[i])
		}
	}
	if accepted != 1 || conflicts != 1 {
		t.Fatalf("want 1 accepted + 1 conflict, got %d/%d", accepted, conflicts)
	}
	loaded, _, _ := store.Load()
	if loaded.Revision != rev+1 {
		t.Fatalf("expected one accepted generation, got revision %d", loaded.Revision)
	}
}
