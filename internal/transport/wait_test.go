package transport

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

func mutate(t *testing.T, store *state.Store, expected uint64, fn func(gen uint64, n *state.RunState)) uint64 {
	t.Helper()
	r, err := store.Mutate(expected, func(gen uint64, n *state.RunState) error { fn(gen, n); return nil })
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	return r.Revision
}

// SessionInput / SessionViewer are TEST-LOCAL mirrors of the pre-seam viewer: the
// production seam is now a Poller supplying a coherent (State, View) pair, but the
// classification tests are still cleanest expressed as a role/replacement view over
// loaded state. pollFor adapts such a viewer into a Poller by loading the store and
// building the SessionInput from it — the exact plumbing the coordinator does inside
// its readCoherent bracket.
type SessionInput struct {
	Revision     uint64
	Phase        state.Phase
	Lifecycle    state.Lifecycle
	ActiveTurnID string // "" when no turn is assigned
}

type SessionViewer func(in SessionInput, sessionID string) (SessionView, error)

func pollFor(store *state.Store, sessionID string, view SessionViewer) Poller {
	return func() (PollObservation, error) {
		rs, ok, err := store.Load()
		if err != nil {
			return PollObservation{}, err
		}
		if !ok {
			return PollObservation{}, ErrNoRun
		}
		in := SessionInput{Revision: rs.Revision, Phase: rs.Phase, Lifecycle: rs.Lifecycle}
		if rs.Assignment != nil {
			in.ActiveTurnID = rs.Assignment.ID
		}
		v, verr := view(in, sessionID)
		if verr != nil {
			return PollObservation{}, verr
		}
		return PollObservation{State: rs, View: v}, nil
	}
}

// fakeClock lets a test fire the poll/timeout channels deterministically and
// observe when Wait requests each, so no test sleeps.
type fakeClock struct {
	timeoutCh    chan time.Time
	pollCh       chan time.Time
	timeoutAsked chan struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{timeoutCh: make(chan time.Time, 1), pollCh: make(chan time.Time, 1), timeoutAsked: make(chan struct{}, 16)}
}

func (c *fakeClock) timeout(time.Duration) <-chan time.Time {
	select {
	case c.timeoutAsked <- struct{}{}:
	default:
	}
	return c.timeoutCh
}
func (c *fakeClock) poll(time.Duration) <-chan time.Time { return c.pollCh }

func viewForRole(role Role, replaced map[string]uint64) SessionViewer {
	return func(in SessionInput, sessionID string) (SessionView, error) {
		if g, ok := replaced[sessionID]; ok {
			return SessionView{Replaced: true, ReplacementGeneration: g}, nil
		}
		v := SessionView{}
		if in.ActiveTurnID != "" {
			if spec, ok := TurnSpec(in.Phase); ok && spec.Role == role {
				v.OwnsActiveTurn = true
			}
		}
		return v, nil
	}
}

func toPairTurn(gen uint64, n *state.RunState) {
	n.Phase = state.PhaseCheckpoint
	n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	bindEvidence(n, gen)
}

// --- immediate-return wake tests ---

func TestWaitWakesOnMyTurn(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	next := mutate(t, store, rev, toPairTurn)
	ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitAssignment || ev.TurnID == nil || *ev.TurnID != "turn-2" || ev.Revision != next {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestWaitStopPriority(t *testing.T) {
	cases := []struct {
		name string
		mut  func(gen uint64, n *state.RunState)
		want WaitKind
	}{
		{"cancelled", func(_ uint64, n *state.RunState) {
			n.Lifecycle = state.LifecycleCancelled
			n.Assignment, n.Evidence = nil, nil // the binding is consumed with the turn it authorized
		}, WaitCancelled},
		{"completed", func(_ uint64, n *state.RunState) {
			n.Lifecycle = state.LifecycleCompleted
			n.Phase = state.PhaseDone
			idx := 1 // the single step is done
			n.StepIndex = &idx
			n.Assignment = nil
			n.Evidence = nil // the binding is consumed with the turn it authorized
		}, WaitCompleted},
		{"gate", func(gen uint64, n *state.RunState) {
			acceptAndHumanGate(n, gen, state.PhaseCheckpoint, "turn-1", dig("7"), "gate-1")
		}, WaitGate},
		{"paused_budget", func(_ uint64, n *state.RunState) { n.Lifecycle = state.LifecyclePausedBudget }, WaitPausedBudget},
		{"rate_limited", func(_ uint64, n *state.RunState) { n.Lifecycle = state.LifecycleRateLimited }, WaitRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, rev := newRunWithActiveTurn(t)
			mutate(t, store, rev, tc.mut)
			ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)))
			if err != nil {
				t.Fatalf("wait: %v", err)
			}
			if ev.Kind != tc.want {
				t.Fatalf("%s: kind = %s, want %s", tc.name, ev.Kind, tc.want)
			}
			if tc.want == WaitGate && (ev.GateID == nil || *ev.GateID != "gate-1") {
				t.Fatalf("gate missing gate_id: %+v", ev)
			}
		})
	}
}

func TestWaitFailedCarriesProjection(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(r uint64, n *state.RunState) {
		n.Lifecycle = state.LifecycleFailedRetryable
		n.Assignment = nil
		n.Evidence = nil // the binding is consumed with the turn it authorized
		n.Failure = &state.Projection{Code: "test_gate_failed", Reason: "tests failed", NextAction: "fix and resubmit", AtRevision: r}
	})
	ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitFailed || ev.Code == nil || *ev.Code != "test_gate_failed" || ev.NextAction == nil || *ev.NextAction != "fix and resubmit" {
		t.Fatalf("failed event missing projection: %+v", ev)
	}
}

func TestWaitRecoveryCarriesRedactedProjection(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(r uint64, n *state.RunState) {
		n.Recovery = &state.Projection{Code: "torn_generation", Reason: "torn at token=sk-ant-abcdefghijklmnopqrstuvwx", NextAction: "run recover", AtRevision: r}
	})
	ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitRecoveryRequired || ev.Code == nil || *ev.Code != "torn_generation" {
		t.Fatalf("recovery event wrong: %+v", ev)
	}
	if ev.Reason == nil || strings.Contains(*ev.Reason, "sk-ant-") {
		t.Fatalf("recovery reason not redacted: %v", ev.Reason)
	}
	if ev.NextAction == nil || *ev.NextAction != "run recover" {
		t.Fatalf("recovery next action missing: %+v", ev)
	}
}

func TestWaitReplacementDominatesOwnTurn(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, toPairTurn)
	ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, "old-pair", viewForRole(RolePair, map[string]uint64{"old-pair": 9})))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitSessionReplaced || ev.ReplacementGeneration == nil || *ev.ReplacementGeneration != 9 {
		t.Fatalf("ev = %+v, want session_replaced gen 9", ev)
	}
}

// A newer terminal run state wins over a concurrent valid replacement.
func TestWaitTerminalBeatsReplacement(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(_ uint64, n *state.RunState) {
		n.Lifecycle = state.LifecycleCancelled
		n.Assignment = nil
		n.Evidence = nil // the binding is consumed with the turn it authorized
	})
	ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, "old-pair", viewForRole(RolePair, map[string]uint64{"old-pair": 9})))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitCancelled {
		t.Fatalf("ev = %+v, want cancelled to win over the replacement", ev)
	}
}

// --- adversarial / trust tests ---

// Wait no longer runs the viewer, so it no longer sanitizes an arbitrary viewer
// error: the Poller owns error hygiene and Wait surfaces its error unchanged.
func TestWaitPropagatesPollError(t *testing.T) {
	sentinel := errors.New("boom")
	_, err := Wait(context.Background(), 0, time.Second, func() (PollObservation, error) { return PollObservation{}, sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the poll error propagated", err)
	}
}

// The value-free guarantee now lives in ResolveSessionView: an unknown session
// fails closed with the fixed ErrSessionView sentinel, never echoing the (possibly
// secret-bearing) session id.
func TestResolveSessionViewUnknownIsValueFree(t *testing.T) {
	_, err := ResolveSessionView(state.Registry{}, state.PhaseCheckpoint, "turn-2", "sk-ant-abcdefghijklmnopqrstuvwx")
	if !errors.Is(err, ErrSessionView) {
		t.Fatalf("unknown session err = %v, want ErrSessionView", err)
	}
	if strings.Contains(err.Error(), "sk-ant-") {
		t.Fatalf("ResolveSessionView leaked the session id: %v", err)
	}
}

func TestWaitRejectsContradictoryView(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, toPairTurn)
	// Replaced with generation 0.
	bad := func(SessionInput, string) (SessionView, error) { return SessionView{Replaced: true}, nil }
	if _, err := Wait(context.Background(), rev, time.Second, pollFor(store, "pair", bad)); !errors.Is(err, ErrSessionView) {
		t.Fatalf("replaced+gen0 err = %v, want ErrSessionView", err)
	}
	// Not replaced but a generation set.
	bad2 := func(SessionInput, string) (SessionView, error) { return SessionView{ReplacementGeneration: 5}, nil }
	if _, err := Wait(context.Background(), rev, time.Second, pollFor(store, "pair", bad2)); !errors.Is(err, ErrSessionView) {
		t.Fatalf("!replaced+gen5 err = %v, want ErrSessionView", err)
	}
}

// Note: the gate-coherence corruption cases that formerly lived here
// (gateless AWAIT_GUIDANCE, paused-without-gate, gate-outside-AWAIT) are now
// unrepresentable — the state layer enforces the four-way gate equivalence, so such
// a generation can never be persisted or loaded. See state's gate-coherence tests.

// The view is resolved on every poll, so an unknown/failed session fails
// immediately even at the caller's current revision, and a registration-only
// replacement wakes.
func TestWaitSeamAtSameRevision(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	// Session-view failure at since == current: immediate failure, not a timeout.
	failing := func(SessionInput, string) (SessionView, error) { return SessionView{}, ErrSessionView }
	if _, err := Wait(context.Background(), rev, time.Second, pollFor(store, "lead", failing)); !errors.Is(err, ErrSessionView) {
		t.Fatalf("view failure at same revision err = %v, want ErrSessionView", err)
	}
	// Replacement at since == current (no run mutation): immediate wake.
	repl := viewForRole(RoleLead, map[string]uint64{"lead": 7})
	ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, "lead", repl))
	if err != nil || ev.Kind != WaitSessionReplaced || ev.ReplacementGeneration == nil || *ev.ReplacementGeneration != 7 {
		t.Fatalf("replacement at same revision ev=%+v err=%v", ev, err)
	}
}

func TestWaitSeamContradictions(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	next := mutate(t, store, rev, toPairTurn) // pair turn active
	cases := map[string]SessionViewer{
		"owns and replaced": func(SessionInput, string) (SessionView, error) {
			return SessionView{OwnsActiveTurn: true, Replaced: true, ReplacementGeneration: 3}, nil
		},
		"owns with no turn": func(in SessionInput, _ string) (SessionView, error) { return SessionView{OwnsActiveTurn: true}, nil },
	}
	// "owns with no turn" needs a state with no active turn; use a coherent TESTS phase.
	mutate(t, store, next, func(_ uint64, n *state.RunState) {
		n.Phase = state.PhaseTests
		idx := 1 // TESTS sits at the plan end
		n.StepIndex = &idx
		n.Assignment = nil
		n.Evidence = nil // the binding is consumed with the turn it authorized
	})
	after, _, _ := store.Load()
	for name, view := range cases {
		if _, err := Wait(context.Background(), after.Revision-1, time.Second, pollFor(store, "pair", view)); !errors.Is(err, ErrSessionView) {
			t.Fatalf("%s err = %v, want ErrSessionView", name, err)
		}
	}
}

// --- aggregate recovery: ride-over transient, surface persistent ---

// A transient nonterminal aggregate (a healthy peer submit's brief window) is
// ridden over: wait keeps polling and returns the real event once it clears.
func TestWaitRidesOverTransientRecovery(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	next := mutate(t, store, rev, toPairTurn)
	clk := newFakeClock()

	// The first two polls report recovery-required; the third is coherent and wakes.
	calls := 0
	poll := func() (PollObservation, error) {
		calls++
		if calls <= 2 {
			return PollObservation{RecoveryRequired: true}, nil
		}
		return pollFor(store, "pair", viewForRole(RolePair, nil))()
	}
	ch := make(chan WaitEvent, 1)
	errCh := make(chan error, 1)
	go func() {
		ev, err := waitWithClock(context.Background(), rev, time.Second, poll, clk)
		ch <- ev
		errCh <- err
	}()
	<-clk.timeoutAsked        // first (recovery) poll ran; we are in the loop
	clk.pollCh <- time.Time{} // second poll: recovery, ridden over
	clk.pollCh <- time.Time{} // third poll: coherent wake
	if err := <-errCh; err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev := <-ch; ev.Kind != WaitAssignment || ev.Revision != next {
		t.Fatalf("ev = %+v, want the assignment after riding over recovery", ev)
	}
}

// A nonterminal aggregate that persists to the deadline surfaces as recovery-required.
func TestWaitPersistentRecoveryAtTimeout(t *testing.T) {
	_, rev := newRunWithActiveTurn(t)
	clk := newFakeClock()
	poll := func() (PollObservation, error) { return PollObservation{RecoveryRequired: true}, nil }
	errCh := make(chan error, 1)
	go func() {
		_, err := waitWithClock(context.Background(), rev, time.Second, poll, clk)
		errCh <- err
	}()
	<-clk.timeoutAsked
	clk.timeoutCh <- time.Time{} // deadline: final poll still recovery
	if err := <-errCh; !errors.Is(err, ErrWaitRecoveryRequired) {
		t.Fatalf("err = %v, want ErrWaitRecoveryRequired", err)
	}
}

// A recovery observed only on the initial poll, cleared by the deadline, returns
// the coherent event rather than recovery-required.
func TestWaitInitialRecoveryClearsByDeadline(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	clk := newFakeClock()
	calls := 0
	poll := func() (PollObservation, error) {
		calls++
		if calls == 1 {
			return PollObservation{RecoveryRequired: true}, nil
		}
		return pollFor(store, "pair", viewForRole(RolePair, nil))()
	}
	ch := make(chan WaitEvent, 1)
	errCh := make(chan error, 1)
	go func() {
		ev, err := waitWithClock(context.Background(), rev, time.Second, poll, clk)
		ch <- ev
		errCh <- err
	}()
	<-clk.timeoutAsked           // initial recovery poll done; in the loop
	clk.timeoutCh <- time.Time{} // deadline: final poll is coherent-unchanged
	if err := <-errCh; err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev := <-ch; ev.Kind != WaitUnchanged || ev.Revision != rev {
		t.Fatalf("ev = %+v, want unchanged after the transient cleared", ev)
	}
}

// --- clock-driven tests ---

func TestWaitUnchangedOnTimeout(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	clk := newFakeClock()
	ch := make(chan WaitEvent, 1)
	go func() {
		ev, _ := waitWithClock(context.Background(), rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)), clk)
		ch <- ev
	}()
	<-clk.timeoutAsked
	clk.timeoutCh <- time.Time{}
	if ev := <-ch; ev.Kind != WaitUnchanged || ev.Revision != rev {
		t.Fatalf("ev = %+v, want unchanged at %d", ev, rev)
	}
}

func TestWaitCatchesEventAtBoundary(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	clk := newFakeClock()
	ch := make(chan WaitEvent, 1)
	go func() {
		ev, _ := waitWithClock(context.Background(), rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)), clk)
		ch <- ev
	}()
	<-clk.timeoutAsked
	mutate(t, store, rev, toPairTurn)
	clk.timeoutCh <- time.Time{}
	if ev := <-ch; ev.Kind != WaitAssignment {
		t.Fatalf("ev = %+v, want the boundary assignment", ev)
	}
}

func TestWaitIgnoresOtherRoleTurn(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(gen uint64, n *state.RunState) {
		// Advance to IMPLEMENT_STEP (a lead turn) so the pair waiter has nothing.
		n.Phase = state.PhaseImplementStep
		n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
		bindEvidence(n, gen)
	})
	clk := newFakeClock()
	ch := make(chan WaitEvent, 1)
	go func() {
		ev, _ := waitWithClock(context.Background(), rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)), clk)
		ch <- ev
	}()
	<-clk.timeoutAsked
	clk.timeoutCh <- time.Time{}
	if ev := <-ch; ev.Kind != WaitUnchanged {
		t.Fatalf("ev = %+v, want unchanged", ev)
	}
}

func TestWaitClientAhead(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	_, err := Wait(context.Background(), rev+5, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)))
	var ca *ClientAheadError
	if !errors.As(err, &ca) || ca.CurrentRevision != rev {
		t.Fatalf("err = %v, want *ClientAheadError at %d", err, rev)
	}
}

func TestWaitContextCancelled(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	ctx, cancel := context.WithCancel(context.Background())
	clk := newFakeClock()
	ch := make(chan error, 1)
	go func() {
		_, err := waitWithClock(ctx, rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)), clk)
		ch <- err
	}()
	<-clk.timeoutAsked
	cancel()
	if err := <-ch; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestWaitInvalidTimeout(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	for _, to := range []time.Duration{0, -time.Second, 2 * time.Hour} {
		if _, err := Wait(context.Background(), rev, to, pollFor(store, "pair", viewForRole(RolePair, nil))); !errors.Is(err, ErrInvalidTimeout) {
			t.Fatalf("timeout %v err = %v, want ErrInvalidTimeout", to, err)
		}
	}
}

func TestWaitMissingSeamAndNoRun(t *testing.T) {
	_, rev := newRunWithActiveTurn(t)
	if _, err := Wait(context.Background(), rev, time.Second, nil); !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("nil poll want ErrMissingSeam, got %v", err)
	}
	dir := t.TempDir()
	empty := state.Open(dir+"/state", dir+"/run.lock")
	if _, err := Wait(context.Background(), 0, time.Second, pollFor(empty, "pair", viewForRole(RolePair, nil))); !errors.Is(err, ErrNoRun) {
		t.Fatalf("empty store want ErrNoRun, got %v", err)
	}
}

func TestWaitDoesNotHoldLock(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	next := mutate(t, store, rev, toPairTurn)
	g, ok, err := genstore.Acquire(store.LockPath())
	if err != nil || !ok {
		t.Fatalf("hold lock: %v", err)
	}
	defer g.Release()
	ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)))
	if err != nil || ev.Kind != WaitAssignment || ev.Revision != next {
		t.Fatalf("wait under held lock: ev=%+v err=%v", ev, err)
	}
}

// --- WaitEvent protocol message ---

func TestWaitEventMarshalsAndValidates(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, toPairTurn)
	ev, _ := Wait(context.Background(), rev, time.Second, pollFor(store, "pair", viewForRole(RolePair, nil)))
	canon, err := ev.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := protocol.Validate("wait_event", canon); err != nil {
		t.Fatalf("wire bytes fail the schema: %v", err)
	}
	again, _ := ev.Marshal()
	if sha256.Sum256(canon) != sha256.Sum256(again) {
		t.Fatalf("marshal is not stable")
	}
}

func TestWaitEventDiscriminantValidation(t *testing.T) {
	gid := "g"
	code := "c"
	base := func(k WaitKind, phase state.Phase, lc state.Lifecycle) WaitEvent {
		return WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: k, Revision: 1, Phase: phase, Lifecycle: lc}
	}
	cases := map[string]WaitEvent{
		"assignment with gate_id":     func() WaitEvent { e := base(WaitAssignment, "CHECKPOINT", "running"); e.GateID = &gid; return e }(),
		"cancelled with code":         func() WaitEvent { e := base(WaitCancelled, "DONE", "cancelled"); e.Code = &code; return e }(),
		"cancelled but running":       base(WaitCancelled, "IMPLEMENT_STEP", "running"),
		"gate in TESTS":               func() WaitEvent { e := base(WaitGate, "TESTS", "paused"); e.GateID = &gid; return e }(),
		"paused_budget but completed": base(WaitPausedBudget, "IMPLEMENT_STEP", "completed"),
		"assignment not agent phase":  func() WaitEvent { e := base(WaitAssignment, "DONE", "running"); id := "t"; e.TurnID = &id; return e }(),
	}
	for name, ev := range cases {
		if err := ev.semanticValidate(); err == nil {
			t.Fatalf("%s should be rejected", name)
		}
	}
	// A well-formed assignment passes.
	good := base(WaitAssignment, "CHECKPOINT", "running")
	id := "turn-2"
	good.TurnID = &id
	if err := good.semanticValidate(); err != nil {
		t.Fatalf("a valid assignment should pass: %v", err)
	}
}

// unchanged and session_replaced legitimately allow many states, but the schema
// enums still reject an unknown or empty phase/lifecycle at Marshal.
func TestWaitEventMarshalRejectsUnknownStatus(t *testing.T) {
	unknownPhase := WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: WaitUnchanged, Revision: 1, Phase: "BOGUS", Lifecycle: "running"}
	if _, err := unknownPhase.Marshal(); err == nil {
		t.Fatalf("an unknown phase should fail Marshal")
	}
	emptyLifecycle := WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: WaitUnchanged, Revision: 1, Phase: "IMPLEMENT_STEP", Lifecycle: ""}
	if _, err := emptyLifecycle.Marshal(); err == nil {
		t.Fatalf("an empty lifecycle should fail Marshal")
	}
	gen := uint64(3)
	replacedBogus := WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: WaitSessionReplaced, Revision: 1, Phase: "nope", Lifecycle: "running", ReplacementGeneration: &gen}
	if _, err := replacedBogus.Marshal(); err == nil {
		t.Fatalf("session_replaced with an unknown phase should fail Marshal")
	}
}
