package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/David-c0degeek/claudex/internal/state"
)

// runAtOwnerlessVerify puts the run at a running ownerless VERIFY (no verifier
// turn issued) holding the fresh-session threshold.
func runAtOwnerlessVerify(t *testing.T, requiredGen uint64) (*state.Store, uint64) {
	return newStoreAt(t, func(gen uint64, n *state.RunState) {
		*n.StepIndex = n.AgreedPlan.Plan.StepCount
		n.Phase = state.PhaseVerify
		n.Verify = &state.VerifyRequirement{RequiredGeneration: requiredGen}
		n.Assignment = nil
		n.Evidence = nil // the binding is consumed with the turn it authorized
	})
}

// runAtOwnerlessVerifyRecovering is an ownerless VERIFY that also needs recovery.
func runAtOwnerlessVerifyRecovering(t *testing.T, requiredGen uint64) (*state.Store, uint64) {
	return newStoreAt(t, func(gen uint64, n *state.RunState) {
		*n.StepIndex = n.AgreedPlan.Plan.StepCount
		n.Phase = state.PhaseVerify
		n.Verify = &state.VerifyRequirement{RequiredGeneration: requiredGen}
		n.Assignment = nil
		n.Evidence = nil // the binding is consumed with the turn it authorized
		n.Recovery = &state.Projection{Code: "torn_generation", Reason: "torn", NextAction: "recover", AtRevision: gen}
	})
}

func viewCurrentPair(gen uint64) SessionViewer {
	return func(SessionInput, string) (SessionView, error) {
		return SessionView{IsCurrentPair: true, PairGeneration: gen}, nil
	}
}

func viewNotPair() SessionViewer {
	return func(SessionInput, string) (SessionView, error) { return SessionView{}, nil }
}

// viewCurrentPairOwner is the current pair session; it owns the active turn when one
// is assigned (an active VERIFY).
func viewCurrentPairOwner(gen uint64) SessionViewer {
	return func(in SessionInput, _ string) (SessionView, error) {
		v := SessionView{IsCurrentPair: true, PairGeneration: gen}
		if in.ActiveTurnID != "" {
			v.OwnsActiveTurn = true
		}
		return v, nil
	}
}

// --- wait: the fresh-generation boundary ---

// The incumbent pair session (below threshold) is told to bring a fresh generation,
// and it is a standing condition: it wakes even at the same revision.
func TestWaitFreshSessionRequiredForIncumbent(t *testing.T) {
	store, rev := runAtOwnerlessVerify(t, 3)
	ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, sessPair, viewCurrentPair(2)))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitFreshSessionRequired || ev.RequiredGeneration == nil || *ev.RequiredGeneration != 3 {
		t.Fatalf("ev = %+v, want fresh_session_required gen 3", ev)
	}
	if ev.Phase != state.PhaseVerify || ev.Lifecycle != state.LifecycleRunning {
		t.Fatalf("ev shape wrong: %+v", ev)
	}
	if ev.TurnID != nil || ev.GateID != nil || ev.ReplacementGeneration != nil {
		t.Fatalf("fresh_session carries only required_generation: %+v", ev)
	}
}

// A lead (or unknown) session at ownerless VERIFY gets no pair signal.
func TestWaitFreshSessionLeadUnchanged(t *testing.T) {
	store, rev := runAtOwnerlessVerify(t, 3)
	clk := newFakeClock()
	ch := make(chan WaitEvent, 1)
	go func() {
		ev, _ := waitWithClock(context.Background(), rev, time.Second, pollFor(store, sessLead, viewNotPair()), clk)
		ch <- ev
	}()
	<-clk.timeoutAsked
	clk.timeoutCh <- time.Time{}
	if ev := <-ch; ev.Kind != WaitUnchanged {
		t.Fatalf("lead at ownerless VERIFY ev = %+v, want unchanged", ev)
	}
}

// A replacement of the incumbent outranks the fresh-session wait.
func TestWaitFreshSessionReplacementPriority(t *testing.T) {
	store, rev := runAtOwnerlessVerify(t, 3)
	repl := func(SessionInput, string) (SessionView, error) {
		return SessionView{Replaced: true, ReplacementGeneration: 4}, nil
	}
	ev, err := Wait(context.Background(), rev, time.Second, pollFor(store, sessPair, repl))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitSessionReplaced || ev.ReplacementGeneration == nil || *ev.ReplacementGeneration != 4 {
		t.Fatalf("ev = %+v, want session_replaced (priority over fresh-session)", ev)
	}
}

// A current pair session whose generation already qualifies while VERIFY is still
// ownerless is a seam/state contradiction, not unchanged or a downgrade.
func TestWaitFreshSessionQualifyingContradiction(t *testing.T) {
	store, rev := runAtOwnerlessVerify(t, 3)
	if _, err := Wait(context.Background(), rev, time.Second, pollFor(store, sessPair, viewCurrentPair(3))); !errors.Is(err, ErrSessionView) {
		t.Fatalf("qualifying incumbent at ownerless VERIFY err = %v, want ErrSessionView", err)
	}
	if _, err := Wait(context.Background(), rev, time.Second, pollFor(store, sessPair, viewCurrentPair(9))); !errors.Is(err, ErrSessionView) {
		t.Fatalf("above-threshold incumbent at ownerless VERIFY err = %v, want ErrSessionView", err)
	}
}

// An ACTIVE VERIFY is an ordinary pair-owned turn for the current qualifying pair.
func TestWaitActiveVerifyIsAssignment(t *testing.T) {
	store, rev := runAtActiveVerify(t, 3)
	ev, err := Wait(context.Background(), rev-1, time.Second, pollFor(store, sessPair, viewCurrentPairOwner(3)))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitAssignment || ev.TurnID == nil || *ev.TurnID != "verify-turn" {
		t.Fatalf("active VERIFY ev = %+v, want assignment", ev)
	}
}

// At active VERIFY, ownership and current-pair identity must agree, and the owner must
// meet the threshold. Every mismatch is a fail-closed contradiction — it would
// otherwise leak a stale turn or strand the qualifying verifier.
func TestWaitActiveVerifyOwnershipContradictions(t *testing.T) {
	store, rev := runAtActiveVerify(t, 3)
	cases := map[string]SessionViewer{
		"owner below threshold":                      viewCurrentPairOwner(2), // both true, gen < 3
		"owner without current-pair facts":           viewForRole(RolePair, nil),
		"qualifying current pair without ownership":  viewCurrentPair(3), // current pair, gen ok, not owning
		"below-threshold current pair without owner": viewCurrentPair(2),
	}
	for name, v := range cases {
		if _, err := Wait(context.Background(), rev-1, time.Second, pollFor(store, sessPair, v)); !errors.Is(err, ErrSessionView) {
			t.Fatalf("%s err = %v, want ErrSessionView", name, err)
		}
	}
}

// A lead/other-role waiter at active VERIFY (neither owner nor current pair) is
// legitimate and stays unchanged.
func TestWaitActiveVerifyLeadUnchanged(t *testing.T) {
	store, rev := runAtActiveVerify(t, 3)
	clk := newFakeClock()
	ch := make(chan WaitEvent, 1)
	go func() {
		ev, _ := waitWithClock(context.Background(), rev-1, time.Second, pollFor(store, sessLead, viewNotPair()), clk)
		ch <- ev
	}()
	<-clk.timeoutAsked
	clk.timeoutCh <- time.Time{}
	if ev := <-ch; ev.Kind != WaitUnchanged {
		t.Fatalf("lead at active VERIFY ev = %+v, want unchanged", ev)
	}
}

// A required recovery outranks the fresh-session wait.
func TestWaitFreshSessionRecoveryDominates(t *testing.T) {
	store, rev := runAtOwnerlessVerifyRecovering(t, 3)
	ev, err := Wait(context.Background(), rev-1, time.Second, pollFor(store, sessPair, viewCurrentPair(2)))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != WaitRecoveryRequired {
		t.Fatalf("ev = %+v, want recovery to dominate the fresh-session wait", ev)
	}
}

// The pair-generation seam facts are validated: they appear only for the current
// pair session and never contradict replacement.
func TestWaitFreshSessionViewContradictions(t *testing.T) {
	store, rev := runAtOwnerlessVerify(t, 3)
	cases := map[string]SessionViewer{
		"current pair with zero generation": func(SessionInput, string) (SessionView, error) {
			return SessionView{IsCurrentPair: true, PairGeneration: 0}, nil
		},
		"generation without current pair": func(SessionInput, string) (SessionView, error) {
			return SessionView{PairGeneration: 2}, nil
		},
		"current pair and replaced": func(SessionInput, string) (SessionView, error) {
			return SessionView{IsCurrentPair: true, PairGeneration: 2, Replaced: true, ReplacementGeneration: 4}, nil
		},
	}
	for name, v := range cases {
		if _, err := Wait(context.Background(), rev, time.Second, pollFor(store, sessPair, v)); !errors.Is(err, ErrSessionView) {
			t.Fatalf("%s err = %v, want ErrSessionView", name, err)
		}
	}
}

// The shared VERIFY coherence: the live requirement is present exactly at VERIFY
// with a nonzero threshold.
func TestVerifyCoherence(t *testing.T) {
	ok := runFacts{phase: state.PhaseVerify, lifecycle: state.LifecycleRunning, hasVerify: true, verifyGen: 3}
	if err := coherenceCheck(ok); err != nil {
		t.Fatalf("valid VERIFY rejected: %v", err)
	}
	bad := map[string]runFacts{
		"verify missing at VERIFY": {phase: state.PhaseVerify, lifecycle: state.LifecycleRunning, hasVerify: false},
		"verify zero at VERIFY":    {phase: state.PhaseVerify, lifecycle: state.LifecycleRunning, hasVerify: true, verifyGen: 0},
		"verify outside VERIFY":    {phase: state.PhaseImplementStep, lifecycle: state.LifecycleRunning, hasVerify: true, verifyGen: 2},
	}
	for name, f := range bad {
		if err := coherenceCheck(f); !errors.Is(err, ErrCorruptState) {
			t.Fatalf("%s err = %v, want ErrCorruptState", name, err)
		}
	}
}

// The fresh_session_required wire event's discriminant invariants.
func TestFreshSessionEventValidation(t *testing.T) {
	gen := uint64(3)
	mk := func() WaitEvent {
		return WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: WaitFreshSessionRequired, Revision: 5, Phase: state.PhaseVerify, Lifecycle: state.LifecycleRunning, RequiredGeneration: &gen}
	}
	if err := mk().semanticValidate(); err != nil {
		t.Fatalf("valid fresh_session rejected: %v", err)
	}
	if _, err := mk().Marshal(); err != nil {
		t.Fatalf("valid fresh_session marshal: %v", err)
	}
	bad := map[string]func(*WaitEvent){
		"missing required_generation": func(e *WaitEvent) { e.RequiredGeneration = nil },
		"zero required_generation":    func(e *WaitEvent) { z := uint64(0); e.RequiredGeneration = &z },
		"not VERIFY":                  func(e *WaitEvent) { e.Phase = state.PhaseTests },
		"not running":                 func(e *WaitEvent) { e.Lifecycle = state.LifecyclePaused },
		"also a turn_id":              func(e *WaitEvent) { id := "t"; e.TurnID = &id },
		"also a replacement gen":      func(e *WaitEvent) { g := uint64(2); e.ReplacementGeneration = &g },
	}
	for name, mut := range bad {
		e := mk()
		mut(&e)
		if err := e.semanticValidate(); err == nil {
			t.Fatalf("%s should be rejected", name)
		}
	}
	// required_generation must not appear on any other kind.
	other := WaitEvent{ProtocolVersion: 1, MessageType: "wait_event", Kind: WaitUnchanged, Revision: 5, Phase: state.PhaseTests, Lifecycle: state.LifecycleRunning, RequiredGeneration: &gen}
	if err := other.semanticValidate(); err == nil {
		t.Fatalf("required_generation on unchanged should be rejected")
	}
}

// --- status: the ownerless VERIFY wait projection + BYO honesty ---

func TestStatusOwnerlessVerifyWaits(t *testing.T) {
	store, _ := runAtOwnerlessVerify(t, 3)
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.WhoseTurn != nil || s.TurnID != nil {
		t.Fatalf("ownerless VERIFY must fabricate no owner: %+v", s)
	}
	if s.Waiting == nil || s.Waiting.Kind != "fresh_session" || s.Waiting.RequiredGeneration != 3 {
		t.Fatalf("ownerless VERIFY waiting projection wrong: %+v", s.Waiting)
	}
	if _, err := s.Marshal(); err != nil {
		t.Fatalf("ownerless VERIFY status fails its schema: %v", err)
	}
}

func TestStatusActiveVerifyHasOwner(t *testing.T) {
	store, _ := runAtActiveVerify(t, 3)
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.WhoseTurn == nil || *s.WhoseTurn != RolePair || s.TurnID == nil || *s.TurnID != "verify-turn" {
		t.Fatalf("active VERIFY owner wrong: %+v", s)
	}
	if s.Waiting != nil {
		t.Fatalf("active VERIFY must carry no waiting projection: %+v", s.Waiting)
	}
}

func TestStatusOwnerlessVerifyRecoverySuppressesWait(t *testing.T) {
	store, _ := runAtOwnerlessVerifyRecovering(t, 3)
	s, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.Stop == nil || s.Stop.Kind != "recovery" {
		t.Fatalf("recovery stop missing: %+v", s.Stop)
	}
	if s.Waiting != nil {
		t.Fatalf("a recovery stop suppresses the fresh-session wait: %+v", s.Waiting)
	}
	if s.WhoseTurn != nil {
		t.Fatalf("recovery suppresses the owner: %+v", s)
	}
}

// A running ownerless agent phase that is NOT the VERIFY fresh-session wait still
// fails closed (a TESTS phase is coordinator-authored, not an agent phase, so use a
// forged ownerless PLAN_CRITIQUE via the marshal-level invariant).
func TestStatusWaitingContradictions(t *testing.T) {
	store, _ := runAtOwnerlessVerify(t, 3)
	good, err := Status(store, byoHonesty())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	mutations := map[string]func(s *StatusReport){
		"waiting with owner": func(s *StatusReport) {
			r := RolePair
			id := "x"
			s.WhoseTurn, s.TurnID = &r, &id
		},
		"waiting kind unknown":            func(s *StatusReport) { s.Waiting = &WaitingProjection{Kind: "bogus", RequiredGeneration: 3} },
		"waiting zero generation":         func(s *StatusReport) { s.Waiting = &WaitingProjection{Kind: "fresh_session", RequiredGeneration: 0} },
		"waiting outside VERIFY":          func(s *StatusReport) { s.Phase = state.PhaseTests },
		"ownerless VERIFY without a wait": func(s *StatusReport) { s.Waiting = nil },
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

// The fresh-generation enforcement is labeled honestly per tier: BYO declares a
// session-generation mechanism and must never claim process freshness.
func TestStatusFreshSessionHonestyLabel(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	cap := func(mech string) []Capability {
		return []Capability{{Name: "verify-fresh-session", Status: "enforced", Mechanism: mech}}
	}
	// BYO declaring fresh-session-declared is honest.
	byoFresh := func(StatusInput) (HonestyLabels, error) {
		return HonestyLabels{Tier: TierProtocolOnly, TierMechanism: "durable-byo-registration", Capabilities: cap("fresh-session-declared")}, nil
	}
	if _, err := Status(store, byoFresh); err != nil {
		t.Fatalf("BYO fresh-session-declared rejected: %v", err)
	}
	// BYO forging fresh-process is rejected.
	byoForged := func(StatusInput) (HonestyLabels, error) {
		return HonestyLabels{Tier: TierProtocolOnly, TierMechanism: "durable-byo-registration", Capabilities: cap("fresh-process")}, nil
	}
	if _, err := Status(store, byoForged); !errors.Is(err, ErrHonestySource) {
		t.Fatalf("BYO fresh-process forgery err = %v, want ErrHonestySource", err)
	}
	// Managed may legitimately claim fresh-process.
	managedFresh := func(StatusInput) (HonestyLabels, error) {
		return HonestyLabels{Tier: TierManaged, TierMechanism: "managed-launch-record", Capabilities: cap("fresh-process")}, nil
	}
	if _, err := Status(store, managedFresh); err != nil {
		t.Fatalf("managed fresh-process rejected: %v", err)
	}
}

// The protocol-only generation-enforcement label cannot be omitted, downgraded, or
// laundered.
func TestStatusBYOHonestyNegatives(t *testing.T) {
	store, _ := newRunWithActiveTurn(t)
	byo := func(caps []Capability) HonestySource {
		return func(StatusInput) (HonestyLabels, error) {
			return HonestyLabels{Tier: TierProtocolOnly, TierMechanism: "durable-byo-registration", Capabilities: caps}, nil
		}
	}
	cases := map[string][]Capability{
		"missing verify-fresh-session": {{Name: "repo-read-only", Status: "unavailable", Mechanism: "byo-attach"}},
		"wrong status":                 {{Name: "verify-fresh-session", Status: "unavailable", Mechanism: "fresh-session-declared"}},
		"wrong mechanism":              {{Name: "verify-fresh-session", Status: "enforced", Mechanism: "byo-attach"}},
		"fresh-process on BYO":         {{Name: "verify-fresh-session", Status: "enforced", Mechanism: "fresh-process"}},
		"laundered onto another cap": {
			{Name: "verify-fresh-session", Status: "enforced", Mechanism: "fresh-session-declared"},
			{Name: "other-cap", Status: "enforced", Mechanism: "fresh-session-declared"},
		},
	}
	for name, caps := range cases {
		if _, err := Status(store, byo(caps)); !errors.Is(err, ErrHonestySource) {
			t.Fatalf("%s err = %v, want ErrHonestySource", name, err)
		}
	}
	// The exact valid shape passes.
	if _, err := Status(store, byo([]Capability{{Name: "verify-fresh-session", Status: "enforced", Mechanism: "fresh-session-declared"}})); err != nil {
		t.Fatalf("valid BYO label rejected: %v", err)
	}
}

// --- pull: the active-VERIFY fresh-session gate ---

func TestPullVerifyFreshSessionGate(t *testing.T) {
	rs := runStateAt(state.PhaseVerify) // active VERIFY, threshold 3, turn-1
	mk := func(gen uint64) PullInputs {
		in := evidenceInputs(RolePair)
		in.CurrentPairGeneration = gen
		return in
	}
	if _, err := BuildAssignment(rs, mk(2)); !errors.Is(err, ErrFreshSessionRequired) {
		t.Fatalf("below threshold err = %v, want ErrFreshSessionRequired", err)
	}
	if _, err := BuildAssignment(rs, mk(0)); !errors.Is(err, ErrFreshSessionRequired) {
		t.Fatalf("zero/missing generation err = %v, want ErrFreshSessionRequired", err)
	}
	if a, err := BuildAssignment(rs, mk(3)); err != nil || a.Phase != state.PhaseVerify || a.Role != RolePair {
		t.Fatalf("at threshold: a=%+v err=%v", a, err)
	}
	if _, err := BuildAssignment(rs, mk(4)); err != nil {
		t.Fatalf("above threshold err = %v", err)
	}
}

// Ownerless VERIFY has no assignment to pull, and pull never mints one — even for a
// qualifying generation.
func TestPullOwnerlessVerifyNoTurn(t *testing.T) {
	rs := runStateAt(state.PhaseVerify)
	rs.Assignment = nil
	rs.Evidence = nil // the binding is consumed with the turn it authorized
	in := evidenceInputs(RolePair)
	in.CurrentPairGeneration = 4
	if _, err := BuildAssignment(rs, in); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("ownerless VERIFY err = %v, want ErrNoActiveTurn", err)
	}
}

// A VERIFY assignment with no retained requirement — nil or a zero threshold — fails
// closed, so a zero+zero pair cannot bypass the below-threshold comparison.
func TestPullVerifyMissingRequirement(t *testing.T) {
	for name, verify := range map[string]*state.VerifyRequirement{
		"nil requirement":  nil,
		"zero requirement": {RequiredGeneration: 0},
	} {
		rs := runStateAt(state.PhaseVerify)
		rs.Verify = verify
		in := evidenceInputs(RolePair)
		in.CurrentPairGeneration = 0
		if _, err := BuildAssignment(rs, in); !errors.Is(err, ErrAssignmentInvalid) {
			t.Fatalf("%s err = %v, want ErrAssignmentInvalid", name, err)
		}
	}
}
