package engine

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
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
		"non-canonical assignment id (path)": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhasePlanCritique, Route: RouteRetryReview}, assign("bad/id")},
		"non-canonical assignment id (reserved)": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhasePlanCritique, Route: RouteRetryReview}, assign("..")},
		"quality gate origin != from-phase": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhaseAwaitGuidance, Route: RouteGate,
				Gate: &GateSpec{Kind: state.PauseQualityBudget, OriginPhase: state.PhaseCheckpoint, ResumePhase: state.PhaseFix, FixReturn: state.PhaseCheckpoint, Budget: state.BudgetCheckpoint}}, gate("g")},
		"non-hex submitted digest": {baseNext(6),
			Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: state.EventRef{TurnID: "t", Digest: "not-hex"}, Next: state.PhasePlanCritique, Route: RouteRetryReview}, assign("n")},
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

	// Second critique REVISE at the limit, adding a check -> plan quality gate that
	// applies the actionable projection (a gate id is minted).
	rs = mustStep(t, store, facts, critiqueArtifact(t, "t-crit2", rs.Revision, "REVISE", false, []string{"blocking"}, nil, []map[string]any{addCheck("chk-9", "step one")}), gate("plan-gate-1"))
	if rs.Phase != state.PhaseAwaitGuidance || rs.Pause == nil || rs.Pause.Kind != state.PauseQualityBudget || rs.Pause.Budget == nil || rs.Pause.Budget.Kind != state.BudgetPlan {
		t.Fatalf("not parked at a plan quality gate: %+v", rs.Pause)
	}
	if rs.Pause.ResumePhase != state.PhasePlanRevise || rs.PendingFindings == nil {
		t.Fatalf("plan gate did not carry the findings: %+v", rs)
	}
	if rs.CandidateChecks == nil || len(rs.CandidateChecks.Keys) != 1 || rs.CandidateChecks.Keys[0] != "chk-9" {
		t.Fatalf("plan gate did not apply the check projection: %+v", rs.CandidateChecks)
	}
	if rs.Counters.PlanRevisions != 1 {
		t.Fatalf("plan gate charged the counter: %d", rs.Counters.PlanRevisions)
	}
}

// A forged running edge chosen at the frozen budget limit cannot persist an
// over-limit counter — Apply rejects it before any write.
func TestForgedOverLimitEdgeRejected(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	bootstrapPlanDraft(t, store, "t-plan")
	rs, _, _ := store.Load()
	rs = mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", rs.Revision, false, f), assign("t-crit1"))
	facts := ProjectionFacts{CandidatePlan: f.canonicalPlan()}
	// One revise cycle brings PlanRevisions to the default limit of 1.
	rs = mustStep(t, store, facts, critiqueArtifact(t, "t-crit1", rs.Revision, "REVISE", false, []string{"blocking"}, nil, nil), assign("t-rev1"))
	rs = mustStep(t, store, facts, revisionArtifact(t, "t-rev1", rs.Revision, f.digest(t), rs.PendingFindings.Keys), assign("t-crit2"))
	// Now at PLAN_CRITIQUE with PlanRevisions==1==limit and t-crit2 outstanding.
	cur, _, _ := store.Load()
	empty, _ := materializeChecks(nil)
	forged := Decision{
		FromPhase: state.PhasePlanCritique, ExpectedStateRevision: cur.Revision,
		Source: state.EventRef{Digest: hx(), TurnID: "t-crit2"}, Next: state.PhasePlanRevise, Route: RouteToRevise,
		Checks: &empty, Findings: &state.FindingObligations{Source: state.EventRef{Digest: hx(), TurnID: "t-crit2"}, Keys: []string{"finding-a"}},
	}
	_, err := store.Mutate(cur.Revision, func(gen uint64, next *state.RunState) error {
		return Apply(forged, forged.Source, assign("t-forged"), gen, next)
	})
	if !errors.Is(err, ErrBadDecision) {
		t.Fatalf("forged over-limit revise err = %v, want ErrBadDecision", err)
	}
	if after, _, _ := store.Load(); after.Revision != cur.Revision || after.Counters.PlanRevisions != 1 {
		t.Fatalf("forged edge advanced the run: rev %d revs %d", after.Revision, after.Counters.PlanRevisions)
	}
}

// checkpointNext builds a CHECKPOINT run with StepFixes[0]==fixes and CheckpointRounds==limit.
func checkpointNext(fixes, limit int) *state.RunState {
	pol := config.DefaultRunPolicy()
	pol.Budgets.CheckpointRounds = limit
	idx := 0
	return &state.RunState{
		Phase: state.PhaseCheckpoint, Revision: 6, Lifecycle: state.LifecycleRunning, EffectivePolicy: pol,
		Assignment: &state.Ref{ID: "t", IssuedRevision: 5}, StepIndex: &idx,
		AgreedPlan: &state.PlanAgreement{Plan: state.PlanRef{StepCount: 2}},
		Counters:   state.Counters{StepFixes: []int{fixes, 0}}, AcceptedTurns: map[string]state.AcceptedTurn{},
	}
}

// A forged RouteToFix at the checkpoint budget limit is rejected with no write.
func TestForgedCheckpointFixOverLimit(t *testing.T) {
	next := checkpointNext(2, 2) // StepFixes[0]==2==CheckpointRounds
	dec := Decision{FromPhase: state.PhaseCheckpoint, ExpectedStateRevision: 5, Source: src(), Next: state.PhaseFix, Route: RouteToFix}
	before := mustJSON(t, next)
	if err := Apply(dec, src(), assign("n"), 6, next); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("forged fix at limit err = %v, want ErrBadDecision", err)
	}
	if !bytes.Equal(before, mustJSON(t, next)) {
		t.Fatal("rejected fix mutated next")
	}
}

// A checkpoint quality gate raised below the budget limit is rejected with no write.
func TestQualityGateUnderLimitRejected(t *testing.T) {
	next := checkpointNext(0, 2) // StepFixes[0]==0 < CheckpointRounds==2
	dec := Decision{FromPhase: state.PhaseCheckpoint, ExpectedStateRevision: 5, Source: src(), Next: state.PhaseAwaitGuidance, Route: RouteGate,
		Gate: &GateSpec{Kind: state.PauseQualityBudget, OriginPhase: state.PhaseCheckpoint, ResumePhase: state.PhaseFix, FixReturn: state.PhaseCheckpoint, Budget: state.BudgetCheckpoint}}
	before := mustJSON(t, next)
	if err := Apply(dec, src(), gate("g"), 6, next); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("under-limit gate err = %v, want ErrBadDecision", err)
	}
	if !bytes.Equal(before, mustJSON(t, next)) {
		t.Fatal("rejected gate mutated next")
	}
}

// A revision that would materialize empty plan markdown is rejected by the schema.
func TestRevisionEmptyMarkdownRejected(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	draft := bootstrapPlanDraft(t, store, "t-plan")
	mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, f), assign("t-crit1"))
	facts := ProjectionFacts{CandidatePlan: f.canonicalPlan()}
	crs, _, _ := store.Load()
	rs := mustStep(t, store, facts, critiqueArtifact(t, "t-crit1", crs.Revision, "REVISE", false, []string{"blocking"}, nil, nil), assign("t-rev1"))
	bad := mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "plan_revision", "turn_id": "t-rev1", "state_revision": rs.Revision,
		"human_context": nil, "requires_human_decision": false, "decision_question": nil,
		"base_plan_sha256": f.digest(t), "plan_markdown": "", "steps": nil, "risks": nil, "open_questions": nil,
		"responses": []any{map[string]any{"finding_key": rs.PendingFindings.Keys[0], "finding": "f", "action": "accepted", "rationale": "r"}},
	})
	if _, err := step(t, store, facts, bad, assign("x")); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("empty markdown err = %v, want a schema rejection", err)
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

// With a non-empty durable check set, facts that change only a check's content are
// rejected (the durable digest carries description/evidence).
func TestProjectCheckContentMismatchRejected(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	draft := bootstrapPlanDraft(t, store, "t-plan")
	mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, f), assign("t-crit1"))
	facts := ProjectionFacts{CandidatePlan: f.canonicalPlan()}
	// Critique adds chk-1 -> PLAN_REVISE (durable check set now {chk-1}).
	rs, _, _ := store.Load()
	rs = mustStep(t, store, facts, critiqueArtifact(t, "t-crit1", rs.Revision, "REVISE", false, []string{"blocking"}, nil, []map[string]any{addCheck("chk-1", "step one")}), assign("t-rev1"))
	correct := []MaterializedCheck{{Key: "chk-1", Description: "check chk-1", Evidence: "ev chk-1", TargetStep: strptr("step one")}}
	// Revise back to PLAN_CRITIQUE, preserving the check.
	rs = mustStep(t, store, ProjectionFacts{CandidatePlan: f.canonicalPlan(), CandidateChecks: correct},
		revisionArtifact(t, "t-rev1", rs.Revision, f.digest(t), rs.PendingFindings.Keys), assign("t-crit2"))
	// Now feed facts whose only difference is the check description.
	wrong := []MaterializedCheck{{Key: "chk-1", Description: "TAMPERED", Evidence: "ev chk-1", TargetStep: strptr("step one")}}
	if _, err := step(t, store, ProjectionFacts{CandidatePlan: f.canonicalPlan(), CandidateChecks: wrong},
		critiqueArtifact(t, "t-crit2", rs.Revision, "AGREE", false, nil, nil, nil), assign("x")); !errors.Is(err, ErrSemantic) {
		t.Fatalf("check-content mismatch err = %v, want ErrSemantic", err)
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
