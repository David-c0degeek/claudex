package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
)

func hx() string          { return strings.Repeat("a", 64) }
func src() state.EventRef { return state.EventRef{TurnID: "t", Digest: hx()} }

func planPol(planRounds int) config.RunPolicy {
	pol := config.DefaultRunPolicy()
	pol.Budgets.PlanRounds = planRounds
	return pol
}

// --- budget boundary (pure Evaluate) ---

func TestEvaluatePlanBudgetBoundary(t *testing.T) {
	pol := planPol(1)
	mk := func(revs int) state.RunState {
		return state.RunState{Phase: state.PhasePlanCritique, Revision: 5, EffectivePolicy: pol, Counters: state.Counters{PlanRevisions: revs}}
	}
	ev := Event{Kind: EvPlanCritiqued, Source: src(), Verdict: "REVISE", Actionable: true, ResultingChecks: state.CheckSetRef{Keys: []string{}, Digest: hx()}}

	d, err := Evaluate(mk(0), ev) // under the limit -> revise, counter increments in Apply
	if err != nil || d.Route != RouteToRevise || d.Next != state.PhasePlanRevise {
		t.Fatalf("under: route=%v next=%v err=%v", d.Route, d.Next, err)
	}
	d, err = Evaluate(mk(1), ev) // at the limit -> plan quality gate
	if err != nil || d.Route != RouteGate || d.Gate == nil || d.Gate.Budget != state.BudgetPlan || d.Gate.ResumePhase != state.PhasePlanRevise {
		t.Fatalf("at limit: route=%v gate=%+v err=%v", d.Route, d.Gate, err)
	}
	if _, err := Evaluate(mk(2), ev); !errors.Is(err, ErrBudgetCorrupt) { // above the limit -> fail closed
		t.Fatalf("over limit err = %v, want ErrBudgetCorrupt", err)
	}
}

func TestEvaluateCheckpointBudgetBoundary(t *testing.T) {
	pol := config.DefaultRunPolicy()
	pol.Budgets.CheckpointRounds = 2
	mk := func(fixes int) state.RunState {
		idx := 0
		return state.RunState{Phase: state.PhaseCheckpoint, Revision: 5, EffectivePolicy: pol,
			StepIndex: &idx, AgreedPlan: &state.PlanAgreement{Plan: state.PlanRef{StepCount: 2}},
			Counters: state.Counters{StepFixes: []int{fixes, 0}}}
	}
	ev := Event{Kind: EvStepCheckpointed, Source: src(), Verdict: "REVISE", Actionable: true, TestsAdequate: true}

	d, err := Evaluate(mk(1), ev) // under -> FIX
	if err != nil || d.Route != RouteToFix || d.Next != state.PhaseFix {
		t.Fatalf("under: route=%v err=%v", d.Route, err)
	}
	d, err = Evaluate(mk(2), ev) // at -> checkpoint quality gate, resume FIX
	if err != nil || d.Route != RouteGate || d.Gate.Budget != state.BudgetCheckpoint || d.Gate.ResumePhase != state.PhaseFix || d.Gate.FixReturn != state.PhaseCheckpoint {
		t.Fatalf("at limit: gate=%+v err=%v", d.Gate, err)
	}
	if _, err := Evaluate(mk(3), ev); !errors.Is(err, ErrBudgetCorrupt) {
		t.Fatalf("over limit err = %v, want ErrBudgetCorrupt", err)
	}
}

// --- reviewer retry (pure Evaluate) ---

func TestEvaluateReviewerRetry(t *testing.T) {
	// PLAN_CRITIQUE: withheld verdict with no actionable defect (missing evidence
	// only, or REVISE with only minor findings) retries with a fresh pair turn.
	cur := state.RunState{Phase: state.PhasePlanCritique, Revision: 5, EffectivePolicy: config.DefaultRunPolicy()}
	for _, ev := range []Event{
		{Kind: EvPlanCritiqued, Source: src(), Verdict: "AGREE", MissingEvidence: true},
		{Kind: EvPlanCritiqued, Source: src(), Verdict: "REVISE", Actionable: false},
	} {
		d, err := Evaluate(cur, ev)
		if err != nil || d.Route != RouteRetryReview || d.Next != state.PhasePlanCritique {
			t.Fatalf("critique retry: route=%v next=%v err=%v", d.Route, d.Next, err)
		}
	}
	// CHECKPOINT retry likewise, no counter.
	idx := 0
	cp := state.RunState{Phase: state.PhaseCheckpoint, Revision: 5, EffectivePolicy: config.DefaultRunPolicy(),
		StepIndex: &idx, AgreedPlan: &state.PlanAgreement{Plan: state.PlanRef{StepCount: 2}}, Counters: state.Counters{StepFixes: []int{0, 0}}}
	d, err := Evaluate(cp, Event{Kind: EvStepCheckpointed, Source: src(), Verdict: "AGREE", MissingEvidence: true, TestsAdequate: true})
	if err != nil || d.Route != RouteRetryReview || d.Next != state.PhaseCheckpoint {
		t.Fatalf("checkpoint retry: route=%v err=%v", d.Route, err)
	}
}

// AGREE with only non-actionable findings converges (dangling target still blocks).
func TestEvaluateConvergenceTargets(t *testing.T) {
	cur := state.RunState{Phase: state.PhasePlanCritique, Revision: 5, EffectivePolicy: config.DefaultRunPolicy()}
	converged := Event{Kind: EvPlanCritiqued, Source: src(), Verdict: "AGREE", Actionable: false, ResultingChecks: state.CheckSetRef{Keys: []string{}, Digest: hx()}, TargetsValid: true}
	if d, err := Evaluate(cur, converged); err != nil || d.Route != RoutePromote {
		t.Fatalf("converged: route=%v err=%v", d.Route, err)
	}
	dangling := converged
	dangling.TargetsValid = false
	if _, err := Evaluate(cur, dangling); !errors.Is(err, ErrSemantic) {
		t.Fatalf("dangling target err = %v, want ErrSemantic", err)
	}
}

// --- human gate priority (pure Evaluate) ---

func TestEvaluateHumanGateFirst(t *testing.T) {
	cur := state.RunState{Phase: state.PhaseImplementStep, Revision: 5, EffectivePolicy: config.DefaultRunPolicy()}
	d, err := Evaluate(cur, Event{Kind: EvStepImplemented, Source: src(), Decision: true})
	if err != nil || d.Route != RouteGate || d.Gate.Kind != state.PauseHumanDecision || d.Gate.OriginPhase != state.PhaseImplementStep || d.Gate.ResumePhase != state.PhaseImplementStep {
		t.Fatalf("human gate: gate=%+v err=%v", d.Gate, err)
	}
	// A human gate from FIX carries the return target into the pause spec.
	fix := state.RunState{Phase: state.PhaseFix, Revision: 5, FixReturn: state.PhaseCheckpoint, EffectivePolicy: config.DefaultRunPolicy()}
	d, err = Evaluate(fix, Event{Kind: EvFixImplemented, Source: src(), Decision: true})
	if err != nil || d.Gate == nil || d.Gate.ResumePhase != state.PhaseFix || d.Gate.FixReturn != state.PhaseCheckpoint {
		t.Fatalf("fix human gate: gate=%+v err=%v", d.Gate, err)
	}
}

// --- Apply binding + id exactness (pure Apply) ---

func baseNext(rev uint64) *state.RunState {
	return &state.RunState{Phase: state.PhasePlanCritique, Revision: rev, Lifecycle: state.LifecycleRunning, Assignment: &state.Ref{ID: "t", IssuedRevision: 5}, Counters: state.Counters{StepFixes: []int{}}, AcceptedTurns: map[string]state.AcceptedTurn{}}
}

func retryDecision() Decision {
	return Decision{FromPhase: state.PhasePlanCritique, ExpectedStateRevision: 5, Source: src(), Next: state.PhasePlanCritique, Route: RouteRetryReview}
}

func TestApplyBindingChecks(t *testing.T) {
	// Happy path with a gap-skipped generation: the issued assignment binds to gen.
	next := baseNext(9) // gen gap-skipped from expected 5
	if err := Apply(retryDecision(), src(), assign("n1"), 9, next); err != nil {
		t.Fatalf("gap apply: %v", err)
	}
	if next.Assignment == nil || next.Assignment.ID != "n1" || next.Assignment.IssuedRevision != 9 {
		t.Fatalf("issued assignment not bound to the gap generation: %+v", next.Assignment)
	}

	// submitted != Source.
	if err := Apply(retryDecision(), state.EventRef{TurnID: "other", Digest: hx()}, assign("n1"), 6, baseNext(6)); err == nil {
		t.Fatal("mismatched submitted source should be rejected")
	}
	// gen != next.Revision.
	if err := Apply(retryDecision(), src(), assign("n1"), 7, baseNext(6)); err == nil {
		t.Fatal("gen != next.Revision should be rejected")
	}
	// wrong from-phase.
	badPhase := retryDecision()
	badPhase.FromPhase = state.PhaseFix
	if err := Apply(badPhase, src(), assign("n1"), 6, baseNext(6)); err == nil {
		t.Fatal("wrong from-phase should be rejected")
	}
	// consumed assignment id mismatch.
	nx := baseNext(6)
	nx.Assignment.ID = "x"
	if err := Apply(retryDecision(), src(), assign("n1"), 6, nx); err == nil {
		t.Fatal("wrong outstanding assignment should be rejected")
	}
	// assignment issued at the wrong revision.
	nx2 := baseNext(6)
	nx2.Assignment.IssuedRevision = 4
	if err := Apply(retryDecision(), src(), assign("n1"), 6, nx2); err == nil {
		t.Fatal("stale assignment issuance should be rejected")
	}
	// id exactness: a running edge must not carry a gate id.
	if err := Apply(retryDecision(), src(), Ids{AssignmentTurnID: "n1", GateID: "g"}, 6, baseNext(6)); err == nil {
		t.Fatal("extra gate id on a running edge should be rejected")
	}
	// id exactness: a running edge requires an assignment id.
	if err := Apply(retryDecision(), src(), Ids{}, 6, baseNext(6)); err == nil {
		t.Fatal("missing assignment id should be rejected")
	}
}

func TestApplyGateIdExactness(t *testing.T) {
	next := &state.RunState{Phase: state.PhaseImplementStep, Revision: 6, Lifecycle: state.LifecycleRunning, Assignment: &state.Ref{ID: "t", IssuedRevision: 5}, Counters: state.Counters{StepFixes: []int{}}, AcceptedTurns: map[string]state.AcceptedTurn{}}
	dec := Decision{FromPhase: state.PhaseImplementStep, ExpectedStateRevision: 5, Source: src(), Next: state.PhaseAwaitGuidance, Route: RouteGate,
		Gate: &GateSpec{Kind: state.PauseHumanDecision, OriginPhase: state.PhaseImplementStep, ResumePhase: state.PhaseImplementStep}}
	// A gate requires a gate id and no assignment id.
	if err := Apply(dec, src(), assign("n1"), 6, next); err == nil {
		t.Fatal("assignment id on a gate edge should be rejected")
	}
	if err := Apply(dec, src(), gate("g1"), 6, next); err != nil {
		t.Fatalf("valid gate apply: %v", err)
	}
	if next.Pause == nil || next.Gate == nil || next.Assignment != nil || next.Lifecycle != state.LifecyclePaused || next.Phase != state.PhaseAwaitGuidance {
		t.Fatalf("gate not realized: %+v", next)
	}
}

// --- semantic rejects (pure Project via the adapter) ---

func TestProjectRevisionSemanticRejects(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	draft := bootstrapPlanDraft(t, store, "t-plan")
	mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, f), assign("t-crit1"))
	critFacts := ProjectionFacts{CandidatePlan: f.canonicalPlan()}
	crs, _, _ := store.Load()
	rs, err := step(t, store, critFacts, critiqueArtifact(t, "t-crit1", crs.Revision, "REVISE", false, []string{"blocking"}, nil, nil), assign("t-rev1"))
	if err != nil {
		t.Fatalf("to revise: %v", err)
	}
	reviseFacts := ProjectionFacts{CandidatePlan: f.canonicalPlan()}
	rev := rs.Revision

	// Wrong base digest -> semantic reject, no acceptance.
	bad := revisionArtifact(t, "t-rev1", rev, strings.Repeat("f", 64), rs.PendingFindings.Keys)
	if _, err := step(t, store, reviseFacts, bad, assign("x")); !errors.Is(err, ErrSemantic) {
		t.Fatalf("wrong base err = %v, want ErrSemantic", err)
	}
	// Extra (unbound) response key -> semantic reject.
	extra := revisionArtifact(t, "t-rev1", rev, f.digest(t), append(append([]string{}, rs.PendingFindings.Keys...), "finding-z"))
	if _, err := step(t, store, reviseFacts, extra, assign("x")); !errors.Is(err, ErrSemantic) {
		t.Fatalf("extra response err = %v, want ErrSemantic", err)
	}
	// Missing response key -> semantic reject.
	missing := revisionArtifact(t, "t-rev1", rev, f.digest(t), nil)
	if _, err := step(t, store, reviseFacts, missing, assign("x")); !errors.Is(err, ErrSemantic) {
		t.Fatalf("missing response err = %v, want ErrSemantic", err)
	}
	// The store never advanced past PLAN_REVISE.
	if cur, _, _ := store.Load(); cur.Phase != state.PhasePlanRevise {
		t.Fatalf("a semantic reject advanced the run to %s", cur.Phase)
	}
}

// A critique that removes a check key that is not present is a semantic reject.
func TestProjectCritiqueRemoveMissingRejected(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	draft := bootstrapPlanDraft(t, store, "t-plan")
	mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, f), assign("t-crit1"))
	critFacts := ProjectionFacts{CandidatePlan: f.canonicalPlan()}
	crs, _, _ := store.Load()
	remove := []map[string]any{{"key": "ghost", "description": "d", "evidence": "e", "action": "remove", "target_step": nil}}
	if _, err := step(t, store, critFacts, critiqueArtifact(t, "t-crit1", crs.Revision, "REVISE", false, []string{"blocking"}, nil, remove), assign("x")); !errors.Is(err, ErrSemantic) {
		t.Fatalf("remove-missing err = %v, want ErrSemantic", err)
	}
}
