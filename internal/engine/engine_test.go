package engine

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
)

// --- test adapter: Project -> Evaluate -> Apply, simulating transport's accept ---

// step drives one accepted submit through the pure engine and, inside the same
// mutation, appends the accepted turn exactly as transport would after Apply.
func step(t *testing.T, store *state.Store, facts ProjectionFacts, canonical []byte, ids Ids) (state.RunState, error) {
	t.Helper()
	cur, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	ev, err := Project(cur, canonical, facts)
	if err != nil {
		return state.RunState{}, err
	}
	dec, err := Evaluate(cur, ev)
	if err != nil {
		return state.RunState{}, err
	}
	return store.Mutate(cur.Revision, func(gen uint64, next *state.RunState) error {
		if err := Apply(dec, ev.Source, ids, gen, next); err != nil {
			return err
		}
		next.AcceptedTurns[ev.Source.TurnID] = state.AcceptedTurn{
			ArtifactDigest: ev.Source.Digest,
			Receipt:        state.Receipt{TurnID: ev.Source.TurnID, Revision: gen, ArtifactDigest: ev.Source.Digest},
			Phase:          cur.Phase,
		}
		return nil
	})
}

// --- fixtures ---

func newStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	return state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
}

// planFixture is a plan whose artifact and materialized facts agree by construction.
type planFixture struct {
	Markdown string
	Steps    []PlanStep
	Risks    []string
	OpenQ    []string
}

func twoStepPlan() planFixture {
	return planFixture{
		Markdown: "architecture prose",
		Steps: []PlanStep{
			{Title: "step one", Description: "do one", Files: []string{"a.go"}, Tests: []string{"a_test.go"}},
			{Title: "step two", Description: "do two", Files: []string{"b.go"}, Tests: []string{"b_test.go"}},
		},
		Risks: []string{"a risk"},
		OpenQ: []string{},
	}
}

func (f planFixture) canonicalPlan() CanonicalPlan {
	return CanonicalPlan{Markdown: f.Markdown, Steps: f.Steps, Risks: f.Risks, OpenQuestions: f.OpenQ}
}

func (f planFixture) digest(t *testing.T) string {
	d, err := planDocDigest(f.Markdown, f.Steps, f.Risks, f.OpenQ)
	if err != nil {
		t.Fatalf("plan digest: %v", err)
	}
	return d
}

func stepsJSON(steps []PlanStep) []any {
	out := make([]any, 0, len(steps))
	for _, s := range steps {
		out = append(out, map[string]any{"title": s.Title, "description": s.Description, "files": s.Files, "tests": s.Tests})
	}
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func planArtifact(t *testing.T, turnID string, rhd bool, f planFixture) []byte {
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "plan", "turn_id": turnID, "state_revision": 1,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": nil,
		"plan_markdown": f.Markdown, "steps": stepsJSON(f.Steps), "risks": f.Risks, "open_questions": f.OpenQ,
	})
}

func critiqueArtifact(t *testing.T, turnID, verdict string, rhd bool, severities []string, missing []string, checkOps []map[string]any) []byte {
	findings := make([]any, 0, len(severities))
	for i, sev := range severities {
		findings = append(findings, map[string]any{
			"key": "finding-" + string(rune('a'+i)), "kind": "new", "category": "safety",
			"severity": sev, "file": nil, "line": nil, "problem": "p", "evidence": "e", "suggested_fix": "s",
		})
	}
	checks := make([]any, 0, len(checkOps))
	for _, op := range checkOps {
		checks = append(checks, op)
	}
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "plan_critique", "turn_id": turnID, "state_revision": 1,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": nil,
		"verdict": verdict, "findings": findings, "implementation_checks": checks,
		"missing_evidence": missing, "simpler_alternative": nil, "notes": "n",
	})
}

func implReport(t *testing.T, turnID string, rhd bool) []byte {
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "implementation_report", "turn_id": turnID, "state_revision": 1,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": nil,
		"files_changed": []any{"a.go"}, "deviations_from_plan": []any{}, "notes": "n",
	})
}

func checkpointArtifact(t *testing.T, turnID, verdict string, rhd, testsAdequate bool, severities, missing []string) []byte {
	findings := make([]any, 0, len(severities))
	for _, sev := range severities {
		findings = append(findings, map[string]any{"severity": sev, "file": nil, "line": nil, "problem": "p", "evidence": "e", "suggested_fix": "s"})
	}
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "checkpoint_review", "turn_id": turnID, "state_revision": 1,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": nil,
		"verdict": verdict, "findings": findings, "missing_evidence": missing,
		"tests_adequate": testsAdequate, "tests_critique": "tc",
	})
}

// bootstrapPlanDraft creates a valid run at PLAN_DRAFT with turnID assigned, the
// shape attach leaves for the lead's first plan submit.
func bootstrapPlanDraft(t *testing.T, store *state.Store, turnID string) state.RunState {
	t.Helper()
	init, err := store.Mutate(0, func(_ uint64, n *state.RunState) error {
		n.RunID = "run-a"
		n.Lifecycle = state.LifecycleRunning
		n.Phase = state.PhaseInit
		n.CreatedUnix = 1000
		n.TaskSnapshot = state.SnapshotRef{RelPath: "inputs/task.json", Digest: strings.Repeat("a", 64)}
		n.PolicySnapshot = state.SnapshotRef{RelPath: "inputs/policy.json", Digest: strings.Repeat("b", 64)}
		pol := config.DefaultRunPolicy()
		pol.TestGate = config.TestGate{Disabled: true}
		n.EffectivePolicy = pol
		n.FS = state.FSResult{Class: "supported-local", Reason: "local fixed drive"}
		n.Base = pol.BaseBranch
		n.BaseCommit = strings.Repeat("a", 40)
		n.WorktreeRelPath = ".claudex/runs/run-a/worktree"
		n.RunBranch = "claudex/run-a"
		return nil
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	draft, err := store.Mutate(init.Revision, func(rev uint64, n *state.RunState) error {
		ref := &state.Ref{ID: turnID, IssuedRevision: rev}
		n.Phase = state.PhasePlanDraft
		n.Assignment = ref
		n.FirstTurn = ref
		n.StartedUnix = n.CreatedUnix
		n.DeadlineUnix = n.CreatedUnix + n.EffectivePolicy.Limits.MaxWallSeconds
		return nil
	})
	if err != nil {
		t.Fatalf("issue first turn: %v", err)
	}
	return draft
}

func assign(turn string) Ids { return Ids{AssignmentTurnID: turn} }
func gate(id string) Ids     { return Ids{GateID: id} }

// --- unit tests ---

// The engine's empty check-set digest must be exactly what state pins, so a promoted
// or set empty check set round-trips through state validation.
func TestEmptyCheckSetAcceptedByState(t *testing.T) {
	empty, err := materializeChecks(nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if empty.Keys == nil || len(empty.Keys) != 0 {
		t.Fatalf("empty keys not a non-nil empty slice: %+v", empty.Keys)
	}
	store := newStore(t)
	draft := bootstrapPlanDraft(t, store, "t1")
	// Accept the draft into PLAN_CRITIQUE, which persists the empty check set.
	facts := ProjectionFacts{}
	if _, err := step(t, store, facts, planArtifact(t, "t1", false, twoStepPlan()), assign("t-crit")); err != nil {
		t.Fatalf("draft->critique with empty checks rejected by state: %v", err)
	}
	loaded, _, _ := store.Load()
	if loaded.CandidateChecks == nil || loaded.CandidateChecks.Digest != empty.Digest {
		t.Fatalf("persisted empty check digest disagrees: %+v", loaded.CandidateChecks)
	}
	_ = draft
}

// --- full dry run over the matrix ---

func TestDryRunPlanThroughCheckpointFix(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	bootstrapPlanDraft(t, store, "t-plan")

	// PLAN_DRAFT -> PLAN_CRITIQUE (candidate + empty checks).
	rs, err := step(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", false, f), assign("t-crit1"))
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if rs.Phase != state.PhasePlanCritique || rs.CandidatePlan == nil || rs.CandidatePlan.Digest != f.digest(t) || rs.CandidatePlan.StepCount != 2 {
		t.Fatalf("after draft: %+v", rs)
	}

	// PLAN_CRITIQUE REVISE with a blocking finding + a check add -> PLAN_REVISE.
	critFacts := ProjectionFacts{CandidatePlan: f.canonicalPlan(), CandidateChecks: nil}
	addCheck := []map[string]any{{"key": "chk-1", "action": "add", "target_step": "step one"}}
	rs, err = step(t, store, critFacts, critiqueArtifact(t, "t-crit1", "REVISE", false, []string{"blocking"}, nil, addCheck), assign("t-rev1"))
	if err != nil {
		t.Fatalf("critique->revise: %v", err)
	}
	if rs.Phase != state.PhasePlanRevise || rs.PendingFindings == nil || rs.Counters.PlanRevisions != 1 {
		t.Fatalf("after critique-revise: %+v", rs)
	}
	if len(rs.CandidateChecks.Keys) != 1 || rs.CandidateChecks.Keys[0] != "chk-1" {
		t.Fatalf("check op not applied: %+v", rs.CandidateChecks)
	}

	// PLAN_REVISE (answers the outstanding finding exactly) -> PLAN_CRITIQUE.
	reviseFacts := ProjectionFacts{CandidatePlan: f.canonicalPlan(), CandidateChecks: []MaterializedCheck{{Key: "chk-1", TargetStep: strptr("step one")}}}
	revision := revisionArtifact(t, "t-rev1", f.digest(t), rs.PendingFindings.Keys)
	rs, err = step(t, store, reviseFacts, revision, assign("t-crit2"))
	if err != nil {
		t.Fatalf("revise->critique: %v", err)
	}
	if rs.Phase != state.PhasePlanCritique || rs.PendingFindings != nil || len(rs.CandidateChecks.Keys) != 1 {
		t.Fatalf("after revise: %+v (checks must be preserved, findings cleared)", rs)
	}

	// PLAN_CRITIQUE AGREE (target valid) -> IMPLEMENT_STEP (promote).
	rs, err = step(t, store, reviseFacts, critiqueArtifact(t, "t-crit2", "AGREE", false, nil, nil, nil), assign("t-impl1"))
	if err != nil {
		t.Fatalf("critique->promote: %v", err)
	}
	if rs.Phase != state.PhaseImplementStep || rs.AgreedPlan == nil || rs.StepIndex == nil || *rs.StepIndex != 0 || len(rs.Counters.StepFixes) != 2 {
		t.Fatalf("after promote: %+v", rs)
	}

	// IMPLEMENT_STEP -> CHECKPOINT.
	rs, err = step(t, store, ProjectionFacts{}, implReport(t, "t-impl1", false), assign("t-chk1"))
	if err != nil || rs.Phase != state.PhaseCheckpoint {
		t.Fatalf("impl->checkpoint: %+v err=%v", rs, err)
	}

	// CHECKPOINT REVISE (blocking) -> FIX.
	rs, err = step(t, store, ProjectionFacts{}, checkpointArtifact(t, "t-chk1", "REVISE", false, true, []string{"blocking"}, nil), assign("t-fix1"))
	if err != nil {
		t.Fatalf("checkpoint->fix: %v", err)
	}
	if rs.Phase != state.PhaseFix || rs.FixReturn != state.PhaseCheckpoint || rs.Counters.StepFixes[0] != 1 {
		t.Fatalf("after checkpoint-fix: %+v", rs)
	}

	// FIX -> CHECKPOINT.
	rs, err = step(t, store, ProjectionFacts{}, implReport(t, "t-fix1", false), assign("t-chk2"))
	if err != nil || rs.Phase != state.PhaseCheckpoint || rs.FixReturn != "" {
		t.Fatalf("fix->checkpoint: %+v err=%v", rs, err)
	}

	// CHECKPOINT AGREE on the non-final step -> next IMPLEMENT_STEP (cursor 1).
	rs, err = step(t, store, ProjectionFacts{}, checkpointArtifact(t, "t-chk2", "AGREE", false, true, nil, nil), assign("t-impl2"))
	if err != nil {
		t.Fatalf("checkpoint->next: %v", err)
	}
	if rs.Phase != state.PhaseImplementStep || *rs.StepIndex != 1 {
		t.Fatalf("after next step: %+v", rs)
	}

	// IMPLEMENT_STEP -> CHECKPOINT, then AGREE on the FINAL step -> ErrPhaseUnsupported.
	rs, err = step(t, store, ProjectionFacts{}, implReport(t, "t-impl2", false), assign("t-chk3"))
	if err != nil || rs.Phase != state.PhaseCheckpoint {
		t.Fatalf("impl2->checkpoint: %+v err=%v", rs, err)
	}
	if _, err := step(t, store, ProjectionFacts{}, checkpointArtifact(t, "t-chk3", "AGREE", false, true, nil, nil), assign("t-x")); err != ErrPhaseUnsupported {
		t.Fatalf("final checkpoint err = %v, want ErrPhaseUnsupported", err)
	}
}

func strptr(s string) *string { return &s }

func revisionArtifact(t *testing.T, turnID, baseDigest string, findingKeys []string) []byte {
	responses := make([]any, 0, len(findingKeys))
	for _, k := range findingKeys {
		responses = append(responses, map[string]any{"finding_key": k, "finding": "f", "action": "accepted", "rationale": "r"})
	}
	// A null steps/risks/open_questions preserves the base; here we preserve all.
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "plan_revision", "turn_id": turnID, "state_revision": 1,
		"human_context": nil, "requires_human_decision": false, "decision_question": nil,
		"base_plan_sha256": baseDigest, "plan_markdown": nil, "steps": nil, "risks": nil, "open_questions": nil,
		"responses": responses,
	})
}
