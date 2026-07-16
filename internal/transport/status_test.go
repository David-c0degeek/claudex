package transport

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

func byoHonesty() HonestySource {
	return func(StatusInput) (HonestyLabels, error) {
		return HonestyLabels{
			Tier:         TierProtocolOnly,
			Capabilities: []Capability{{Name: "repo-read-only", Status: "unavailable", Mechanism: "byo-attach"}},
		}, nil
	}
}

func TestStatusProjectsLiveTurn(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.RunID != "run-a" || s.Phase != state.PhaseImplementStep || s.Lifecycle != state.LifecycleRunning {
		t.Fatalf("status header wrong: %+v", s)
	}
	if s.WhoseTurn == nil || *s.WhoseTurn != RoleLead || s.TurnID == nil || *s.TurnID != "turn-1" {
		t.Fatalf("whose_turn/turn_id wrong: %+v", s)
	}
	if s.Honesty.Tier != TierProtocolOnly || len(s.Honesty.Capabilities) != 1 {
		t.Fatalf("honesty wrong: %+v", s.Honesty)
	}
	pol := config.DefaultRunPolicy()
	if s.Caps.RunTurns.Used != 0 || s.Caps.RunTurns.Limit != pol.Limits.MaxRunTurns {
		t.Fatalf("run_turns cap wrong: %+v", s.Caps.RunTurns)
	}
	if s.Caps.PlanRounds.Remaining != pol.Budgets.PlanRounds || s.Caps.RunTurns.Mechanism != "durable-state-counter" {
		t.Fatalf("caps wrong: %+v", s.Caps)
	}
}

// A terminal run may keep an actionable phase but must report no owner.
func TestStatusTerminalHasNoOwner(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(_ uint64, n *state.RunState) {
		n.Phase = state.PhaseCheckpoint
		n.Lifecycle = state.LifecycleCancelled
		n.Assignment = nil
	})
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.WhoseTurn != nil || s.TurnID != nil {
		t.Fatalf("a cancelled run must report no owner: %+v", s)
	}
}

func TestStatusOwnershipFailsClosed(t *testing.T) {
	// Assignment under a non-running lifecycle.
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(_ uint64, n *state.RunState) { n.Lifecycle = state.LifecycleCancelled }) // keeps the assignment
	if _, err := Status(store, byoHonesty()); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("assignment under cancelled err = %v, want ErrCorruptState", err)
	}
	// Running agent phase with no assignment.
	store2, rev2 := newRunWithActiveTurn(t)
	mutate(t, store2, rev2, func(_ uint64, n *state.RunState) { n.Assignment = nil }) // IMPLEMENT_STEP, running, no assignment
	if _, err := Status(store2, byoHonesty()); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("running agent phase without assignment err = %v, want ErrCorruptState", err)
	}
}

func TestStatusCapsClamp(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(_ uint64, n *state.RunState) { n.Counters.PlanRevisions = 999 })
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	pol := config.DefaultRunPolicy()
	if s.Caps.PlanRounds.Remaining != 0 || s.Caps.PlanRounds.ExceededBy != 999-pol.Budgets.PlanRounds {
		t.Fatalf("over-limit cap not clamped: %+v", s.Caps.PlanRounds)
	}
}

func TestStatusStopProjection(t *testing.T) {
	// failure lifecycle -> failure stop.
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(r uint64, n *state.RunState) {
		n.Lifecycle = state.LifecycleFailedTerminal
		n.Assignment = nil
		n.Failure = &state.Projection{Code: "boom", Reason: "leaked token=sk-ant-abcdefghijklmnopqrstuvwx", NextAction: "inspect", AtRevision: r}
	})
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.Stop == nil || s.Stop.Kind != "failure" || s.Stop.Code != "boom" {
		t.Fatalf("failure stop wrong: %+v", s.Stop)
	}
	if strings.Contains(s.Stop.Reason, "sk-ant-") {
		t.Fatalf("stop reason not redacted: %q", s.Stop.Reason)
	}
	// recovery projection -> recovery stop.
	store2, rev2 := newRunWithActiveTurn(t)
	mutate(t, store2, rev2, func(r uint64, n *state.RunState) {
		n.Recovery = &state.Projection{Code: "torn", Reason: "torn gen", NextAction: "recover", AtRevision: r}
	})
	s2, _ := Status(store2, byoHonesty())
	if s2.Stop == nil || s2.Stop.Kind != "recovery" {
		t.Fatalf("recovery stop wrong: %+v", s2.Stop)
	}
}

func TestStatusHonestyFailsClosed(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	// Nil source.
	if _, err := Status(store, nil); !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("nil source want ErrMissingSeam, got %v", err)
	}
	// Source error (value-free sentinel).
	failing := func(StatusInput) (HonestyLabels, error) {
		return HonestyLabels{}, errors.New("lookup failed for /home/dave secret")
	}
	_, err := Status(store, failing)
	if !errors.Is(err, ErrHonestySource) || strings.Contains(err.Error(), "/home/") {
		t.Fatalf("source error leaked or wrong: %v", err)
	}
	// Invalid labels (unknown tier / bad capability status).
	badTier := func(StatusInput) (HonestyLabels, error) { return HonestyLabels{Tier: "super"}, nil }
	if _, err := Status(store, badTier); !errors.Is(err, ErrHonestySource) {
		t.Fatalf("bad tier want ErrHonestySource, got %v", err)
	}
	badCap := func(StatusInput) (HonestyLabels, error) {
		return HonestyLabels{Tier: TierProtocolOnly, Capabilities: []Capability{{Name: "x", Status: "totally-enforced", Mechanism: "m"}}}, nil
	}
	if _, err := Status(store, badCap); !errors.Is(err, ErrHonestySource) {
		t.Fatalf("bad capability status want ErrHonestySource, got %v", err)
	}
}

// Status never acquires the mutation lock.
func TestStatusDoesNotHoldLock(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	g, ok, err := genstore.Acquire(store.LockPath())
	if err != nil || !ok {
		t.Fatalf("hold lock: %v", err)
	}
	defer g.Release()
	if _, err := Status(store, byoHonesty()); err != nil {
		t.Fatalf("status under held lock: %v", err)
	}
}

func TestStatusMarshalsAndRoundTrips(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	canon, err := s.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := protocol.Validate("status", canon); err != nil {
		t.Fatalf("wire bytes fail the schema: %v", err)
	}
	var back StatusReport
	if err := json.Unmarshal(canon, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(s, back) {
		t.Fatalf("round-trip differs:\n%+v\n%+v", s, back)
	}
}

func TestStatusCoherenceFailsClosed(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	mutate(t, store, rev, func(gen uint64, n *state.RunState) { n.Gate = &state.Ref{ID: "g", IssuedRevision: gen} }) // gate outside AWAIT_GUIDANCE
	if _, err := Status(store, byoHonesty()); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("err = %v, want ErrCorruptState", err)
	}
}
