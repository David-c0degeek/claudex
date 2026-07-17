package engine

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

// Evaluate rejects an event kind that the current phase does not accept.
func TestEvaluateWrongKindRejected(t *testing.T) {
	cur := state.RunState{Phase: state.PhasePlanCritique, Revision: 5, EffectivePolicy: config.DefaultRunPolicy()}
	if _, err := Evaluate(cur, Event{Kind: EvStepImplemented, Source: src()}); !errors.Is(err, ErrPhaseMismatch) {
		t.Fatalf("wrong kind err = %v, want ErrPhaseMismatch", err)
	}
}

// A malformed Decision is rejected before any write: next is byte-for-byte unchanged.
func TestApplyNoPartialWriteOnReject(t *testing.T) {
	freeze := func(n *state.RunState) []byte { return mustJSON(t, n) }
	ck := &state.CheckSetRef{Keys: []string{}, Digest: hx()}

	cases := map[string]struct {
		next *state.RunState
		dec  Decision
		ids  Ids
	}{
		"unknown route": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhasePlanCritique, Route: Route(99)}, assign("n")},
		"gate route without gate": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhaseAwaitGuidance, Route: RouteGate}, gate("g")},
		"promote without candidate": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhaseImplementStep, Route: RoutePromote, Checks: ck}, assign("n")},
		"revise without checks": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhasePlanRevise, Route: RouteToRevise}, assign("n")},
		"retry with stray payload": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhasePlanCritique, Route: RouteRetryReview, Plan: &state.PlanRef{}}, assign("n")},
		"wrong from-phase edge": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhasePlanCritique, Route: RouteDraftAccepted}, assign("n")},
		"running with a gate id": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhasePlanCritique, Route: RouteRetryReview}, Ids{AssignmentTurnID: "n", GateID: "g"}},
		"reissue consumed turn": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhasePlanCritique, Route: RouteRetryReview}, assign("t")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			before := freeze(tc.next)
			if err := Apply(tc.dec, tc.dec.Source, tc.ids, tc.next.Revision, tc.next); err == nil {
				t.Fatalf("malformed decision was accepted")
			}
			if after := freeze(tc.next); !bytes.Equal(before, after) {
				t.Fatalf("rejected Apply partially mutated next:\n%s\n%s", before, after)
			}
		})
	}
}

// A nil cursor for RouteNextStep is rejected, not a panic.
func TestApplyNilCursorRejected(t *testing.T) {
	next := &state.RunState{Phase: state.PhaseCheckpoint, Revision: 6, Lifecycle: state.LifecycleRunning,
		Assignment: &state.Ref{ID: "t", IssuedRevision: 5}, Counters: state.Counters{StepFixes: []int{}}, AcceptedTurns: map[string]state.AcceptedTurn{}}
	dec := Decision{FromPhase: state.PhaseCheckpoint, ExpectedStateRevision: 5, Source: src(), Next: state.PhaseImplementStep, Route: RouteNextStep}
	before := mustJSON(t, next)
	if err := Apply(dec, src(), assign("n"), 6, next); err == nil {
		t.Fatal("nil cursor should be rejected")
	}
	if !bytes.Equal(before, mustJSON(t, next)) {
		t.Fatal("nil-cursor reject mutated next")
	}
}

// The at-limit plan gate parks a valid v5 state: PendingFindings present, checks
// updated, and the counter unchanged at the frozen limit.
func TestPlanBudgetGatePersists(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	bootstrapPlanDraft(t, store, "t-plan")

	rs, _, _ := store.Load()
	rs = mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", rs.Revision, false, f), assign("t-crit1"))
	facts := ProjectionFacts{CandidatePlan: f.canonicalPlan()}

	// First critique REVISE (blocking) -> PLAN_REVISE, PlanRevisions -> 1 (== the
	// default PlanRounds limit).
	rs = mustStep(t, store, facts, critiqueArtifact(t, "t-crit1", rs.Revision, "REVISE", false, []string{"blocking"}, nil, nil), assign("t-rev1"))
	if rs.Counters.PlanRevisions != 1 {
		t.Fatalf("plan revisions = %d, want 1", rs.Counters.PlanRevisions)
	}
	// Revise answers the finding -> PLAN_CRITIQUE.
	rs = mustStep(t, store, facts, revisionArtifact(t, "t-rev1", rs.Revision, f.digest(t), rs.PendingFindings.Keys), assign("t-crit2"))

	// Second critique REVISE at the limit -> plan quality gate (a gate id is minted).
	rs = mustStep(t, store, facts, critiqueArtifact(t, "t-crit2", rs.Revision, "REVISE", false, []string{"blocking"}, nil, nil), gate("plan-gate-1"))
	if rs.Phase != state.PhaseAwaitGuidance || rs.Pause == nil || rs.Pause.Kind != state.PauseQualityBudget || rs.Pause.Budget == nil || rs.Pause.Budget.Kind != state.BudgetPlan {
		t.Fatalf("not parked at a plan quality gate: %+v", rs.Pause)
	}
	if rs.Pause.ResumePhase != state.PhasePlanRevise || rs.PendingFindings == nil || rs.CandidateChecks == nil {
		t.Fatalf("plan gate did not carry the actionable projection: %+v", rs)
	}
	if rs.Counters.PlanRevisions != 1 {
		t.Fatalf("plan gate charged the counter: %d", rs.Counters.PlanRevisions)
	}
}

// --- projector semantic validators ---

func TestProjectDuplicateStepTitlesRejected(t *testing.T) {
	store := newStore(t)
	draft := bootstrapPlanDraft(t, store, "t-plan")
	dup := planFixture{Markdown: "m", Steps: []PlanStep{{Title: "same", Description: "d1"}, {Title: "same", Description: "d2"}}, Risks: []string{}, OpenQ: []string{}}
	if _, err := step(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, dup), assign("x")); !errors.Is(err, ErrSemantic) {
		t.Fatalf("duplicate titles err = %v, want ErrSemantic", err)
	}
}

func TestProjectFactsMismatchRejected(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	draft := bootstrapPlanDraft(t, store, "t-plan")
	mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, f), assign("t-crit1"))
	rs, _, _ := store.Load()
	// Facts whose plan does not materialize to the durable candidate digest.
	wrong := ProjectionFacts{CandidatePlan: CanonicalPlan{Markdown: "different", Steps: f.Steps, Risks: f.Risks, OpenQuestions: f.OpenQ}}
	if _, err := step(t, store, wrong, critiqueArtifact(t, "t-crit1", rs.Revision, "AGREE", false, nil, nil, nil), assign("x")); !errors.Is(err, ErrSemantic) {
		t.Fatalf("facts mismatch err = %v, want ErrSemantic", err)
	}
}

func TestProjectDuplicateCheckOpKeyRejected(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	draft := bootstrapPlanDraft(t, store, "t-plan")
	mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, f), assign("t-crit1"))
	rs, _, _ := store.Load()
	dupOps := []map[string]any{addCheck("chk-1", "step one"), addCheck("chk-1", "step two")}
	if _, err := step(t, store, ProjectionFacts{CandidatePlan: f.canonicalPlan()},
		critiqueArtifact(t, "t-crit1", rs.Revision, "REVISE", false, []string{"blocking"}, nil, dupOps), assign("x")); !errors.Is(err, ErrSemantic) {
		t.Fatalf("duplicate check-op key err = %v, want ErrSemantic", err)
	}
}

// The cumulative materialized check set is bounded by the shared item ceiling.
func TestMaterializeChecksCountBound(t *testing.T) {
	checks := make([]MaterializedCheck, protocol.MaxKeySetItems+1)
	for i := range checks {
		checks[i] = MaterializedCheck{Key: fmt.Sprintf("k-%05d", i)}
	}
	if _, err := materializeChecks(checks); !errors.Is(err, ErrSemantic) {
		t.Fatalf("over-count err = %v, want ErrSemantic", err)
	}
}
