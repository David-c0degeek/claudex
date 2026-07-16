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
		v := SessionView{}
		if g, ok := replaced[sessionID]; ok {
			v.Replaced = true
			v.ReplacementGeneration = g
		}
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
}

// --- immediate-return wake tests ---

func TestWaitWakesOnMyTurn(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	next := mutate(t, store, rev, toPairTurn)
	ev, err := Wait(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil))
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
		{"cancelled", func(_ uint64, n *state.RunState) { n.Lifecycle = state.LifecycleCancelled; n.Assignment = nil }, WaitCancelled},
		{"completed", func(_ uint64, n *state.RunState) {
			n.Lifecycle = state.LifecycleCompleted
			n.Phase = state.PhaseDone
			n.Assignment = nil
		}, WaitCompleted},
		{"gate", func(gen uint64, n *state.RunState) {
			n.Phase = state.PhaseAwaitGuidance
			n.Lifecycle = state.LifecyclePaused
			n.Assignment = nil
			n.Gate = &state.Ref{ID: "gate-1", IssuedRevision: gen}
		}, WaitGate},
		{"paused_budget", func(_ uint64, n *state.RunState) { n.Lifecycle = state.LifecyclePausedBudget }, WaitPausedBudget},
		{"rate_limited", func(_ uint64, n *state.RunState) { n.Lifecycle = state.LifecycleRateLimited }, WaitRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, rev := newRunWithActiveTurn(t)
			mutate(t, store, rev, tc.mut)
			ev, err := Wait(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil))
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
		n.Failure = &state.Projection{Code: "test_gate_failed", Reason: "tests failed", NextAction: "fix and resubmit", AtRevision: r}
	})
	ev, err := Wait(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil))
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
	ev, err := Wait(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil))
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
	ev, err := Wait(context.Background(), store, "old-pair", rev, time.Second, viewForRole(RolePair, map[string]uint64{"old-pair": 9}))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitSessionReplaced || ev.ReplacementGeneration == nil || *ev.ReplacementGeneration != 9 {
		t.Fatalf("ev = %+v, want session_replaced gen 9", ev)
	}
}

// --- adversarial / trust tests ---

func TestWaitViewerError(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, toPairTurn)
	failing := func(SessionInput, string) (SessionView, error) {
		return SessionView{}, errors.New("registration lookup failed for token=sk-ant-abcdefghijklmnopqrstuvwx")
	}
	_, err := Wait(context.Background(), store, "pair", rev, time.Second, failing)
	if !errors.Is(err, ErrSessionView) {
		t.Fatalf("err = %v, want ErrSessionView", err)
	}
	if strings.Contains(err.Error(), "sk-ant-") {
		t.Fatalf("viewer error leaked a secret: %v", err)
	}
}

func TestWaitViewerInvalidReplacement(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, toPairTurn)
	// Replaced with generation 0.
	bad := func(SessionInput, string) (SessionView, error) { return SessionView{Replaced: true}, nil }
	if _, err := Wait(context.Background(), store, "pair", rev, time.Second, bad); !errors.Is(err, ErrSessionView) {
		t.Fatalf("replaced+gen0 err = %v, want ErrSessionView", err)
	}
	// Not replaced but a generation set.
	bad2 := func(SessionInput, string) (SessionView, error) { return SessionView{ReplacementGeneration: 5}, nil }
	if _, err := Wait(context.Background(), store, "pair", rev, time.Second, bad2); !errors.Is(err, ErrSessionView) {
		t.Fatalf("!replaced+gen5 err = %v, want ErrSessionView", err)
	}
}

func TestWaitGatelessAwaitGuidanceFailsClosed(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(_ uint64, n *state.RunState) {
		n.Phase = state.PhaseAwaitGuidance
		n.Lifecycle = state.LifecyclePaused
		n.Assignment = nil // no gate
	})
	if _, err := Wait(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil)); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("err = %v, want ErrCorruptState", err)
	}
}

// --- clock-driven tests ---

func TestWaitUnchangedOnTimeout(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	clk := newFakeClock()
	ch := make(chan WaitEvent, 1)
	go func() {
		ev, _ := waitWithClock(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil), clk)
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
		ev, _ := waitWithClock(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil), clk)
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
		n.Phase = state.PhasePlanDraft // a lead turn
		n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	})
	clk := newFakeClock()
	ch := make(chan WaitEvent, 1)
	go func() {
		ev, _ := waitWithClock(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil), clk)
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
	_, err := Wait(context.Background(), store, "pair", rev+5, time.Second, viewForRole(RolePair, nil))
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
		_, err := waitWithClock(ctx, store, "pair", rev, time.Second, viewForRole(RolePair, nil), clk)
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
		if _, err := Wait(context.Background(), store, "pair", rev, to, viewForRole(RolePair, nil)); !errors.Is(err, ErrInvalidTimeout) {
			t.Fatalf("timeout %v err = %v, want ErrInvalidTimeout", to, err)
		}
	}
}

func TestWaitMissingSeamAndNoRun(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	if _, err := Wait(context.Background(), store, "pair", rev, time.Second, nil); !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("nil view want ErrMissingSeam, got %v", err)
	}
	dir := t.TempDir()
	empty := state.Open(dir+"/state", dir+"/run.lock")
	if _, err := Wait(context.Background(), empty, "pair", 0, time.Second, viewForRole(RolePair, nil)); !errors.Is(err, ErrNoRun) {
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
	ev, err := Wait(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil))
	if err != nil || ev.Kind != WaitAssignment || ev.Revision != next {
		t.Fatalf("wait under held lock: ev=%+v err=%v", ev, err)
	}
}

// --- WaitEvent protocol message ---

func TestWaitEventMarshalsAndValidates(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, toPairTurn)
	ev, _ := Wait(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil))
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
	bad := WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: WaitAssignment, Revision: 1, Phase: "CHECKPOINT", Lifecycle: "running", GateID: &gid}
	if err := bad.semanticValidate(); err == nil {
		t.Fatalf("assignment carrying a gate_id should be rejected")
	}
	code := "c"
	bad2 := WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: WaitCancelled, Revision: 1, Phase: "IMPLEMENT_STEP", Lifecycle: "cancelled", Code: &code}
	if err := bad2.semanticValidate(); err == nil {
		t.Fatalf("cancelled carrying a code should be rejected")
	}
}
