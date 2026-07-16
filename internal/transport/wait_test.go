package transport

import (
	"context"
	"crypto/sha256"
	"errors"
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

// fakeClock lets a test fire the poll and timeout channels deterministically and
// observe when Wait requests each, so no test sleeps.
type fakeClock struct {
	timeoutCh    chan time.Time
	pollCh       chan time.Time
	timeoutAsked chan struct{}
	pollAsked    chan struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		timeoutCh:    make(chan time.Time, 1),
		pollCh:       make(chan time.Time, 1),
		timeoutAsked: make(chan struct{}, 16),
		pollAsked:    make(chan struct{}, 16),
	}
}

func (c *fakeClock) timeout(time.Duration) <-chan time.Time {
	select {
	case c.timeoutAsked <- struct{}{}:
	default:
	}
	return c.timeoutCh
}

func (c *fakeClock) poll(time.Duration) <-chan time.Time {
	select {
	case c.pollAsked <- struct{}{}:
	default:
	}
	return c.pollCh
}

func viewForRole(role Role, replaced map[string]uint64) SessionViewer {
	return func(rs state.RunState, sessionID string) SessionView {
		v := SessionView{}
		if g, ok := replaced[sessionID]; ok {
			v.Replaced = true
			v.ReplacementGeneration = g
		}
		if rs.Assignment != nil {
			if spec, ok := TurnSpec(rs.Phase); ok && spec.Role == role {
				v.OwnsActiveTurn = true
			}
		}
		return v
	}
}

// --- immediate-return wake tests (state already shows the event) ---

func TestWaitWakesOnMyTurn(t *testing.T) {
	store, rev := newRunWithActiveTurn(t) // lead turn at rev
	next := mutate(t, store, rev, func(gen uint64, n *state.RunState) {
		n.Phase = state.PhaseCheckpoint
		n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	})
	ev, err := Wait(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitAssignment || ev.TurnID == nil || *ev.TurnID != "turn-2" || ev.Revision != next {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestWaitIgnoresOtherRoleTurn(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(gen uint64, n *state.RunState) {
		n.Phase = state.PhasePlanDraft // a lead turn
		n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	})
	// A pair session must not wake; with a fake clock, fire the timeout.
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
		{"failed", func(rev uint64, n *state.RunState) {
			n.Lifecycle = state.LifecycleFailedRetryable
			n.Assignment = nil
			n.Failure = &state.Projection{Code: "boom", Reason: "x", NextAction: "retry", AtRevision: rev}
		}, WaitFailed},
		{"paused-budget gate", func(gen uint64, n *state.RunState) {
			n.Phase = state.PhaseAwaitGuidance
			n.Lifecycle = state.LifecyclePausedBudget
			n.Assignment = nil
			n.Gate = &state.Ref{ID: "gate-1", IssuedRevision: gen}
		}, WaitGate},
		{"recovery required", func(rev uint64, n *state.RunState) {
			n.Recovery = &state.Projection{Code: "torn", Reason: "x", NextAction: "recover", AtRevision: rev}
		}, WaitRecoveryRequired},
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
		})
	}
}

// A replaced session learns of its replacement even while it is the turn owner.
func TestWaitReplacementDominatesOwnTurn(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	next := mutate(t, store, rev, func(gen uint64, n *state.RunState) {
		n.Phase = state.PhaseCheckpoint
		n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	})
	replaced := map[string]uint64{"old-pair": 9}
	ev, err := Wait(context.Background(), store, "old-pair", rev, time.Second, viewForRole(RolePair, replaced))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitSessionReplaced || ev.ReplacementGeneration == nil || *ev.ReplacementGeneration != 9 {
		t.Fatalf("ev = %+v, want session_replaced gen 9", ev)
	}
	_ = next
}

// --- clock-driven tests ---

func TestWaitUnchangedOnTimeout(t *testing.T) {
	store, rev := newRunWithActiveTurn(t) // lead turn; pair sees nothing
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

// An event committed at the timeout boundary is caught by the final read.
func TestWaitCatchesEventAtBoundary(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	clk := newFakeClock()
	ch := make(chan WaitEvent, 1)
	go func() {
		ev, _ := waitWithClock(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil), clk)
		ch <- ev
	}()
	<-clk.timeoutAsked // immediate read done, deadline armed
	// Commit a pair turn right at the boundary, then fire the timeout.
	mutate(t, store, rev, func(gen uint64, n *state.RunState) {
		n.Phase = state.PhaseCheckpoint
		n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	})
	clk.timeoutCh <- time.Time{}
	if ev := <-ch; ev.Kind != WaitAssignment {
		t.Fatalf("ev = %+v, want the boundary assignment", ev)
	}
}

func TestWaitClientAhead(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	_, err := Wait(context.Background(), store, "pair", rev+5, time.Second, viewForRole(RolePair, nil))
	var ca *ClientAheadError
	if !errors.As(err, &ca) {
		t.Fatalf("err = %v, want *ClientAheadError", err)
	}
	if ca.CurrentRevision != rev {
		t.Fatalf("client-ahead current = %d, want %d", ca.CurrentRevision, rev)
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

// Wait never acquires the mutation lock: it returns promptly with the lock held.
func TestWaitDoesNotHoldLock(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	next := mutate(t, store, rev, func(gen uint64, n *state.RunState) {
		n.Phase = state.PhaseCheckpoint
		n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	})
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

// --- WaitEvent as a protocol message ---

func TestWaitEventMarshals(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	next := mutate(t, store, rev, func(gen uint64, n *state.RunState) {
		n.Phase = state.PhaseCheckpoint
		n.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	})
	ev, _ := Wait(context.Background(), store, "pair", rev, time.Second, viewForRole(RolePair, nil))
	canon, err := ev.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := protocol.Validate("wait_event", canon); err != nil {
		t.Fatalf("wire bytes fail the schema: %v", err)
	}
	// The canonical bytes are stable (digest matches a re-marshal).
	again, _ := ev.Marshal()
	if sha256.Sum256(canon) != sha256.Sum256(again) {
		t.Fatalf("marshal is not stable")
	}
	_ = next
}

// An event with the wrong per-kind discriminants is rejected before the wire.
func TestWaitEventDiscriminantValidation(t *testing.T) {
	gid := "g"
	bad := WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: WaitAssignment, Revision: 1, Phase: "CHECKPOINT", Lifecycle: "running", GateID: &gid}
	if err := bad.Validate(); err == nil {
		t.Fatalf("an assignment carrying a gate_id should be rejected")
	}
	tid := "t"
	bad2 := WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: WaitCancelled, Revision: 1, Phase: "IMPLEMENT_STEP", Lifecycle: "cancelled", TurnID: &tid}
	if err := bad2.Validate(); err == nil {
		t.Fatalf("a cancelled event carrying a turn_id should be rejected")
	}
}
