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
			Tier:          TierProtocolOnly,
			TierMechanism: "durable-byo-registration",
			Capabilities: []Capability{
				{Name: "repo-read-only", Status: "unavailable", Mechanism: "byo-attach"},
				// A BYO run must positively declare the VERIFY generation enforcement.
				{Name: "verify-fresh-session", Status: "enforced", Mechanism: "fresh-session-declared"},
			},
		}, nil
	}
}

func TestStatusProjectsLiveTurn(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.RunID != "run-a" || s.Phase != state.PhaseCheckpoint || s.Lifecycle != state.LifecycleRunning {
		t.Fatalf("status header wrong: %+v", s)
	}
	if s.WhoseTurn == nil || *s.WhoseTurn != RolePair || s.TurnID == nil || *s.TurnID != "turn-1" {
		t.Fatalf("whose_turn/turn_id wrong: %+v", s)
	}
	if s.Honesty.Tier != TierProtocolOnly || len(s.Honesty.Capabilities) != 2 {
		t.Fatalf("honesty wrong: %+v", s.Honesty)
	}
	pol := config.DefaultRunPolicy()
	// Three turns (plan, critique, implement) were accepted reaching the checkpoint.
	if s.Caps.RunTurns.Used != 3 || s.Caps.RunTurns.Limit != pol.Limits.MaxRunTurns {
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
	// A recovery on a running run with an active turn suppresses the owner: the
	// recovery is the single next actor.
	store2, rev2 := newRunWithActiveTurn(t) // IMPLEMENT_STEP, lead turn-1, running
	mutate(t, store2, rev2, func(r uint64, n *state.RunState) {
		n.Recovery = &state.Projection{Code: "torn", Reason: "torn gen", NextAction: "recover", AtRevision: r}
	})
	s2, err := Status(store2, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s2.Stop == nil || s2.Stop.Kind != "recovery" {
		t.Fatalf("recovery stop wrong: %+v", s2.Stop)
	}
	if s2.WhoseTurn != nil || s2.TurnID != nil {
		t.Fatalf("recovery must suppress the owner: %+v", s2)
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
		return HonestyLabels{Tier: TierProtocolOnly, TierMechanism: "durable-byo-registration", Capabilities: []Capability{{Name: "x", Status: "totally-enforced", Mechanism: "m"}}}, nil
	}
	if _, err := Status(store, badCap); !errors.Is(err, ErrHonestySource) {
		t.Fatalf("bad capability status want ErrHonestySource, got %v", err)
	}
}

// The tier mechanism must actually back the tier.
func TestStatusTierMechanismPairing(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	mismatched := func(StatusInput) (HonestyLabels, error) {
		return HonestyLabels{Tier: TierManaged, TierMechanism: "durable-byo-registration"}, nil
	}
	if _, err := Status(store, mismatched); !errors.Is(err, ErrHonestySource) {
		t.Fatalf("mismatched tier/mechanism err = %v, want ErrHonestySource", err)
	}
	// The correct managed pairing validates (a fake managed source).
	managed := func(StatusInput) (HonestyLabels, error) {
		return HonestyLabels{Tier: TierManaged, TierMechanism: "managed-launch-record"}, nil
	}
	if _, err := Status(store, managed); err != nil {
		t.Fatalf("valid managed pairing rejected: %v", err)
	}
}

// Arbitrary non-secret-pattern text in tier/name must not cross the boundary.
func TestStatusHonestyValueFree(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	leaky := func(StatusInput) (HonestyLabels, error) {
		return HonestyLabels{Tier: "user@example.com /home/dave", TierMechanism: "durable-byo-registration"}, nil
	}
	_, err := Status(store, leaky)
	if !errors.Is(err, ErrHonestySource) || strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "/home/") {
		t.Fatalf("honesty value leaked: %v", err)
	}
}

// Status reflects a real Submit: the resulting revision and newly issued owner.
func TestStatusAfterSubmit(t *testing.T) {
	store, rev := newRunWithActiveTurn(t) // CHECKPOINT, pair turn-1
	adv := checkpointPrep()
	res, err := submit(store, newMemSink(), "sess-2", report("turn-1", rev, "done"), ownerAuth("sess-2"), adv)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.Revision != res.Receipt.Revision {
		t.Fatalf("status revision %d != receipt revision %d", s.Revision, res.Receipt.Revision)
	}
	if s.Phase != state.PhaseImplementStep || s.WhoseTurn == nil || *s.WhoseTurn != RoleLead || s.TurnID == nil || *s.TurnID != "turn-2" {
		t.Fatalf("status did not reflect the submit's transition: %+v", s)
	}
}

// A mutated public report cannot marshal contradictory wire data.
func TestStatusMarshalRejectsContradictions(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	good, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	mutations := map[string]func(s *StatusReport){
		"cap arithmetic":     func(s *StatusReport) { s.Caps.PlanRounds.Remaining = 99 },
		"whose without turn": func(s *StatusReport) { s.TurnID = nil },
		"gate outside gate":  func(s *StatusReport) { gid := "g"; s.GateID = &gid },
		"failure under running": func(s *StatusReport) {
			s.Stop = &StopProjection{Kind: "failure", Code: "c", Reason: "r", NextAction: "a", AtRevision: 1}
		},
		"recovery with owner": func(s *StatusReport) {
			s.Stop = &StopProjection{Kind: "recovery", Code: "c", Reason: "r", NextAction: "a", AtRevision: 1}
		},
		"secret in stop reason": func(s *StatusReport) {
			s.WhoseTurn, s.TurnID = nil, nil
			s.Stop = &StopProjection{Kind: "recovery", Code: "c", Reason: "token=sk-ant-abcdefghijklmnopqrstuvwx", NextAction: "a", AtRevision: 1}
		},
		"step out of order": func(s *StatusReport) {
			s.Caps.CheckpointRounds.Steps = []StepCap{{StepIndex: 5, Used: 0, Remaining: 0, ExceededBy: 0}}
		},
	}
	for name, mut := range mutations {
		t.Run(name, func(t *testing.T) {
			s := good
			mut(&s)
			if _, err := s.Marshal(); err == nil {
				t.Fatalf("%s should be rejected by Marshal", name)
			}
		})
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

// The gate-coherence corruption Status formerly guarded against (a gate outside
// AWAIT_GUIDANCE) is now unrepresentable: the state layer enforces the four-way
// gate equivalence, so such a generation can never be persisted. See state's
// gate-coherence tests.
