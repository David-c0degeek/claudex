package engine

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// --- test adapter: simulate transport (schema + CAS), then Project->Evaluate->Apply ---

// step drives one accepted submit through the pure engine exactly as a production
// adapter would: it schema-validates the phase's expected artifact type, checks the
// envelope revision against the CAS revision, projects, evaluates, and (inside the
// mutation) appends the accepted turn as transport does after Apply.
func step(t *testing.T, store *state.Store, facts ProjectionFacts, canonical []byte, ids Ids) (state.RunState, error) {
	t.Helper()
	cur, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	spec, ok := transport.TurnSpec(cur.Phase)
	if !ok {
		t.Fatalf("no turn spec for phase %s", cur.Phase)
	}
	canon, err := protocol.Validate(spec.ArtifactMessageType, canonical)
	if err != nil {
		return state.RunState{}, fmt.Errorf("schema: %w", err)
	}
	var env struct {
		StateRevision uint64 `json:"state_revision"`
	}
	if err := json.Unmarshal(canon, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.StateRevision != cur.Revision {
		return state.RunState{}, fmt.Errorf("stale: artifact revision %d != current %d", env.StateRevision, cur.Revision)
	}
	// The production loader reconstructs the candidate facts from the accepted history
	// (MaterializeCandidate), whose source always equals the durable candidate source.
	// Mirror that here when the caller left it unset, so fixtures need not thread it.
	if facts.CandidateSource == (state.EventRef{}) && cur.CandidatePlan != nil {
		facts.CandidateSource = cur.CandidatePlan.Source
	}
	ev, err := Project(cur, canon, facts)
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

func newStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	return state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
}

// --- schema-complete fixtures whose materialized facts agree by construction ---

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
		out = append(out, map[string]any{"title": s.Title, "description": s.Description, "files": arr(s.Files), "tests": arr(s.Tests)})
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

// arr coerces a possibly-nil string slice into a non-nil JSON array (a nil slice
// marshals to null, which the schemas reject).
func arr(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

func planArtifact(t *testing.T, turnID string, rev uint64, rhd bool, f planFixture) []byte {
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "plan", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": decisionQuestion(rhd),
		"plan_markdown": f.Markdown, "steps": stepsJSON(f.Steps), "risks": f.Risks, "open_questions": f.OpenQ,
	})
}

func decisionQuestion(rhd bool) any {
	if rhd {
		return "which option?"
	}
	return nil
}

// addCheck builds a schema-complete implementation-check op.
func addCheck(key, target string) map[string]any {
	var t any
	if target != "" {
		t = target
	}
	return map[string]any{"key": key, "description": "check " + key, "evidence": "ev " + key, "action": "add", "target_step": t}
}

func critiqueArtifact(t *testing.T, turnID string, rev uint64, verdict string, rhd bool, severities, missing []string, checkOps []map[string]any) []byte {
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
		"protocol_version": 1, "message_type": "plan_critique", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": decisionQuestion(rhd),
		"verdict": verdict, "findings": findings, "implementation_checks": checks,
		"missing_evidence": arr(missing), "simpler_alternative": nil, "notes": "n",
	})
}

func implReport(t *testing.T, turnID string, rev uint64, rhd bool) []byte {
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "implementation_report", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": decisionQuestion(rhd),
		"files_changed": []any{"a.go"}, "deviations_from_plan": []any{}, "notes": "n",
	})
}

func checkpointArtifact(t *testing.T, turnID string, rev uint64, verdict string, rhd, testsAdequate bool, severities, missing []string) []byte {
	findings := make([]any, 0, len(severities))
	for _, sev := range severities {
		findings = append(findings, map[string]any{"severity": sev, "file": nil, "line": nil, "problem": "p", "evidence": "e", "suggested_fix": "s"})
	}
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "checkpoint_review", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": decisionQuestion(rhd),
		"verdict": verdict, "findings": findings, "missing_evidence": arr(missing),
		"tests_adequate": testsAdequate, "tests_critique": "tc",
	})
}

func revisionArtifact(t *testing.T, turnID string, rev uint64, baseDigest string, findingKeys []string) []byte {
	responses := make([]any, 0, len(findingKeys))
	for _, k := range findingKeys {
		responses = append(responses, map[string]any{"finding_key": k, "finding": "f", "action": "accepted", "rationale": "r"})
	}
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "plan_revision", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": false, "decision_question": nil,
		"base_plan_sha256": baseDigest, "plan_markdown": nil, "steps": nil, "risks": nil, "open_questions": nil,
		"responses": responses,
	})
}

// bootstrapPlanDraft creates a valid run at PLAN_DRAFT with turnID assigned.
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

func assign(turn string) Ids  { return Ids{AssignmentTurnID: turn} }
func gate(id string) Ids      { return Ids{GateID: id} }
func strptr(s string) *string { return &s }

func mustStep(t *testing.T, store *state.Store, facts ProjectionFacts, canonical []byte, ids Ids) state.RunState {
	t.Helper()
	rs, err := step(t, store, facts, canonical, ids)
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	return rs
}

// --- unit test: engine's empty check digest is what state pins ---

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
	mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t1", draft.Revision, false, twoStepPlan()), assign("t-crit"))
	loaded, _, _ := store.Load()
	if loaded.CandidateChecks == nil || loaded.CandidateChecks.Digest != empty.Digest {
		t.Fatalf("persisted empty check digest disagrees: %+v", loaded.CandidateChecks)
	}
}

// Two obligations differing only in description or evidence are distinct sets.
func TestCheckContentInDigest(t *testing.T) {
	a, _ := materializeChecks([]MaterializedCheck{{Key: "k", Description: "one", Evidence: "e"}})
	b, _ := materializeChecks([]MaterializedCheck{{Key: "k", Description: "two", Evidence: "e"}})
	c, _ := materializeChecks([]MaterializedCheck{{Key: "k", Description: "one", Evidence: "other"}})
	if a.Digest == b.Digest || a.Digest == c.Digest {
		t.Fatalf("description/evidence do not affect the digest: %s %s %s", a.Digest, b.Digest, c.Digest)
	}
}

// --- full dry run over the matrix ---

func TestDryRunPlanThroughCheckpointFix(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	bootstrapPlanDraft(t, store, "t-plan")

	rs := mustStep(t, store, ProjectionFacts{}, cur(t, store, "plan-artifact", f, "t-plan"), assign("t-crit1"))
	if rs.Phase != state.PhasePlanCritique || rs.CandidatePlan == nil || rs.CandidatePlan.Digest != f.digest(t) || rs.CandidatePlan.StepCount != 2 {
		t.Fatalf("after draft: %+v", rs)
	}

	// PLAN_CRITIQUE REVISE with a blocking finding + a check add -> PLAN_REVISE.
	critFacts := ProjectionFacts{CandidatePlan: f.canonicalPlan()}
	rs = mustStep(t, store, critFacts, critiqueArtifact(t, "t-crit1", rs.Revision, "REVISE", false, []string{"blocking"}, nil, []map[string]any{addCheck("chk-1", "step one")}), assign("t-rev1"))
	if rs.Phase != state.PhasePlanRevise || rs.PendingFindings == nil || rs.Counters.PlanRevisions != 1 || len(rs.CandidateChecks.Keys) != 1 {
		t.Fatalf("after critique-revise: %+v", rs)
	}

	// PLAN_REVISE (answers the finding exactly) -> PLAN_CRITIQUE (checks preserved).
	withCheck := ProjectionFacts{CandidatePlan: f.canonicalPlan(), CandidateChecks: []MaterializedCheck{{Key: "chk-1", Description: "check chk-1", Evidence: "ev chk-1", TargetStep: strptr("step one")}}}
	rs = mustStep(t, store, withCheck, revisionArtifact(t, "t-rev1", rs.Revision, f.digest(t), rs.PendingFindings.Keys), assign("t-crit2"))
	if rs.Phase != state.PhasePlanCritique || rs.PendingFindings != nil || len(rs.CandidateChecks.Keys) != 1 {
		t.Fatalf("after revise: %+v", rs)
	}

	// PLAN_CRITIQUE AGREE (target valid) -> IMPLEMENT_STEP (promote).
	rs = mustStep(t, store, withCheck, critiqueArtifact(t, "t-crit2", rs.Revision, "AGREE", false, nil, nil, nil), assign("t-impl1"))
	if rs.Phase != state.PhaseImplementStep || rs.AgreedPlan == nil || rs.StepIndex == nil || *rs.StepIndex != 0 || len(rs.Counters.StepFixes) != 2 {
		t.Fatalf("after promote: %+v", rs)
	}

	rs = mustStep(t, store, ProjectionFacts{}, implReport(t, "t-impl1", rs.Revision, false), assign("t-chk1"))
	if rs.Phase != state.PhaseCheckpoint {
		t.Fatalf("impl->checkpoint: %+v", rs)
	}

	// CHECKPOINT REVISE (blocking) -> FIX.
	rs = mustStep(t, store, ProjectionFacts{}, checkpointArtifact(t, "t-chk1", rs.Revision, "REVISE", false, true, []string{"blocking"}, nil), assign("t-fix1"))
	if rs.Phase != state.PhaseFix || rs.FixReturn != state.PhaseCheckpoint || rs.Counters.StepFixes[0] != 1 {
		t.Fatalf("after checkpoint-fix: %+v", rs)
	}

	rs = mustStep(t, store, ProjectionFacts{}, implReport(t, "t-fix1", rs.Revision, false), assign("t-chk2"))
	if rs.Phase != state.PhaseCheckpoint || rs.FixReturn != "" {
		t.Fatalf("fix->checkpoint: %+v", rs)
	}

	// CHECKPOINT AGREE on the non-final step -> next IMPLEMENT_STEP (cursor 1).
	rs = mustStep(t, store, ProjectionFacts{}, checkpointArtifact(t, "t-chk2", rs.Revision, "AGREE", false, true, nil, nil), assign("t-impl2"))
	if rs.Phase != state.PhaseImplementStep || *rs.StepIndex != 1 {
		t.Fatalf("after next step: %+v", rs)
	}

	rs = mustStep(t, store, ProjectionFacts{}, implReport(t, "t-impl2", rs.Revision, false), assign("t-chk3"))
	// AGREE on the FINAL step -> ErrPhaseUnsupported.
	if _, err := step(t, store, ProjectionFacts{}, checkpointArtifact(t, "t-chk3", rs.Revision, "AGREE", false, true, nil, nil), assign("t-x")); err != ErrPhaseUnsupported {
		t.Fatalf("final checkpoint err = %v, want ErrPhaseUnsupported", err)
	}
}

// cur builds the plan artifact for the current revision (small readability helper).
func cur(t *testing.T, store *state.Store, _ string, f planFixture, turnID string) []byte {
	rs, _, _ := store.Load()
	return planArtifact(t, turnID, rs.Revision, false, f)
}

// A schema-invalid artifact (a check op missing required fields) is rejected before
// the engine ever sees it.
func TestSchemaInvalidRejectedBeforeEngine(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	draft := bootstrapPlanDraft(t, store, "t-plan")
	mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, f), assign("t-crit1"))
	rs, _, _ := store.Load()
	badOp := []map[string]any{{"key": "chk-1", "action": "add", "target_step": nil}} // missing description/evidence
	_, err := step(t, store, ProjectionFacts{CandidatePlan: f.canonicalPlan()},
		critiqueArtifact(t, "t-crit1", rs.Revision, "REVISE", false, []string{"blocking"}, nil, badOp), assign("x"))
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("schema-invalid err = %v, want a schema rejection", err)
	}
}
