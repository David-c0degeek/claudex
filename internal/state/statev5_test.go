package state

import "testing"

// Deterministic identities for the plan-negotiation flow the builders drive.
const (
	planTurnID = "plan-turn"
	critTurnID = "crit-turn"
)

func planSrcDigest() string { return hex64("1") }
func planDocDigest() string { return hex64("2") }
func critSrcDigest() string { return hex64("3") }
func candidateChecks() *CheckSetRef {
	return &CheckSetRef{Keys: []string{}, Digest: emptyCheckSetDigest}
}

// issueFirstTurn drives a pristine INIT to PLAN_DRAFT, issuing the first turn
// (FirstTurn == Assignment) and setting the run clock.
func issueFirstTurn(t *testing.T, s *Store, init RunState, turnID string) RunState {
	t.Helper()
	rs, err := s.Mutate(init.Revision, func(rev uint64, next *RunState) error {
		ref := &Ref{ID: turnID, IssuedRevision: rev}
		next.Phase = PhasePlanDraft
		next.Assignment = ref
		next.FirstTurn = ref
		next.StartedUnix = next.CreatedUnix
		next.DeadlineUnix = next.CreatedUnix + next.EffectivePolicy.Limits.MaxWallSeconds
		return nil
	})
	if err != nil {
		t.Fatalf("issue first turn: %v", err)
	}
	return rs
}

// assignAt issues turnID as the outstanding assignment at the run's current agent
// phase (no phase change).
func assignAt(t *testing.T, s *Store, prev RunState, turnID string) RunState {
	t.Helper()
	rs, err := s.Mutate(prev.Revision, func(rev uint64, next *RunState) error {
		next.Assignment = &Ref{ID: turnID, IssuedRevision: rev}
		return nil
	})
	if err != nil {
		t.Fatalf("assign %s: %v", turnID, err)
	}
	return rs
}

// acceptTurnAdvance accepts the currently-assigned turnID (recording it at the
// pre-transition phase) and applies adv to advance the run in the same generation.
func acceptTurnAdvance(t *testing.T, s *Store, prev RunState, turnID, digest string, adv func(rev uint64, next *RunState)) RunState {
	t.Helper()
	rs, err := s.Mutate(prev.Revision, func(rev uint64, next *RunState) error {
		next.AcceptedTurns[turnID] = AcceptedTurn{
			ArtifactDigest: digest,
			Receipt:        Receipt{TurnID: turnID, Revision: rev, ArtifactDigest: digest},
			Phase:          prev.Phase,
			GitCommit:      evidenceFor(prev.Phase, next),
		}
		next.Assignment = nil
		adv(rev, next)
		return nil
	})
	if err != nil {
		t.Fatalf("accept %s: %v", turnID, err)
	}
	return rs
}

// candidatePlan builds the 1-step candidate plan ref sourced by the plan turn.
func candidatePlanN(stepCount int) *PlanRef {
	return &PlanRef{
		Source:    EventRef{Digest: planSrcDigest(), TurnID: planTurnID},
		Digest:    planDocDigest(),
		StepCount: stepCount,
	}
}

func candidatePlan() *PlanRef { return candidatePlanN(1) }

// driveToCritique drives a pristine INIT to PLAN_CRITIQUE with a stepCount-step
// candidate plan and the canonical empty candidate check set (no assignment).
func driveToCritiqueN(t *testing.T, s *Store, init RunState, stepCount int) RunState {
	t.Helper()
	draft := issueFirstTurn(t, s, init, planTurnID)
	return acceptTurnAdvance(t, s, draft, planTurnID, planSrcDigest(), func(_ uint64, next *RunState) {
		next.Phase = PhasePlanCritique
		next.CandidatePlan = candidatePlanN(stepCount)
		next.CandidateChecks = candidateChecks()
	})
}

func driveToCritique(t *testing.T, s *Store, init RunState) RunState {
	return driveToCritiqueN(t, s, init, 1)
}

// driveToAgreedImplementN drives a pristine INIT through the real negotiation to a
// stepCount-step agreed plan at IMPLEMENT_STEP with cursor 0 and no assignment.
func driveToAgreedImplementN(t *testing.T, s *Store, init RunState, stepCount int) RunState {
	t.Helper()
	critique := driveToCritiqueN(t, s, init, stepCount)
	critAssigned := assignAt(t, s, critique, critTurnID)
	return acceptTurnAdvance(t, s, critAssigned, critTurnID, critSrcDigest(), func(rev uint64, next *RunState) {
		plan := *next.CandidatePlan
		checks := *next.CandidateChecks
		next.AgreedPlan = &PlanAgreement{
			Plan:           plan,
			Critique:       EventRef{Digest: critSrcDigest(), TurnID: critTurnID},
			Checks:         checks,
			AgreedRevision: rev,
		}
		next.CandidatePlan = nil
		next.CandidateChecks = nil
		next.PendingFindings = nil
		next.Counters.StepFixes = make([]int, stepCount)
		idx := 0
		next.StepIndex = &idx
		next.Phase = PhaseImplementStep
	})
}

func driveToAgreedImplement(t *testing.T, s *Store, init RunState) RunState {
	return driveToAgreedImplementN(t, s, init, 1)
}

// mustAgreedImplement drives a fresh store to a 1-step agreed plan at
// IMPLEMENT_STEP (cursor 0, no assignment).
func mustAgreedImplement(t *testing.T, s *Store) RunState {
	t.Helper()
	return driveToAgreedImplement(t, s, mustInit(t, s))
}
