package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/engine"
	"github.com/David-c0degeek/claudex/internal/fsclass"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// --- attach seams (fakes) ---

func opID(c string) string { return "op-" + strings.Repeat(c, 32) }

type fakeBase struct{ commit string }

func (f fakeBase) ResolveBase(string, string) (string, error) { return f.commit, nil }

type fakeWorktree struct{ applied bool }

func (f *fakeWorktree) ObserveWorktree(string, attach.BootstrapIntent) (txn.StepStatus, error) {
	if f.applied {
		return txn.StatusApplied, nil
	}
	return txn.StatusNotApplied, nil
}

func (f *fakeWorktree) ApplyWorktree(string, attach.BootstrapIntent) error {
	f.applied = true
	return nil
}

type fakeClassifier struct{ res fsclass.Result }

func (f fakeClassifier) Classify(string) (fsclass.Result, error) { return f.res, nil }

func supportedFS() fakeClassifier {
	return fakeClassifier{res: fsclass.Result{Class: fsclass.SupportedLocal, Reason: "local fixed drive"}}
}

func policyBytes() []byte { return policyBytesWith(1, 3) }

func policyBytesWith(planRounds, checkpointRounds int) []byte {
	p := config.DefaultRunPolicy()
	p.TestGate = config.TestGate{Disabled: true}
	p.Budgets.PlanRounds = planRounds
	p.Budgets.CheckpointRounds = checkpointRounds
	b, _ := json.Marshal(p)
	return b
}

func taskBytes() []byte {
	return []byte(`{"schema_version":1,"goal":"drive the pairing loop","current_behavior":"none","desired_behavior":"two terminals converge","scope":"coordinator core","non_goals":[],"constraints":[],"acceptance_criteria":["it works"],"required_tests":[],"relevant_files":[],"open_questions":[]}`)
}

// newPairedRun bootstraps a lead-claude run and fills the codex pair, leaving the run
// at PLAN_DRAFT with the lead's first turn issued.
func newPairedRun(t *testing.T, repo string) (runID, lead, pair string) {
	return newPairedRunWithPolicy(t, repo, policyBytes())
}

func newPairedRunWithPolicy(t *testing.T, repo string, pol []byte) (runID, lead, pair string) {
	t.Helper()
	fa, err := attach.FirstAttach(attach.FirstAttachRequest{
		RepoDir: repo, Agent: state.AgentClaude, OperationID: opID("a"),
		TaskCanonical: taskBytes(), PolicyCanonical: pol,
		CreatedUnix: 1000, RNG: rand.Reader,
		Base: fakeBase{commit: strings.Repeat("a", 40)}, Worktree: &fakeWorktree{}, Classifier: supportedFS(),
	})
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	ja, err := attach.JoinAttach(attach.JoinAttachRequest{
		RepoDir: repo, RunID: fa.RunID, OperationID: opID("b"),
		Agent: state.AgentCodex, Role: state.SlotPair, Now: 2000, RNG: rand.Reader,
	})
	if err != nil {
		t.Fatalf("join attach: %v", err)
	}
	return fa.RunID, fa.SessionID, ja.SessionID
}

// --- artifact builders (schema-complete; the store canonicalizes) ---

func decisionQuestion(rhd bool) any {
	if rhd {
		return "which option?"
	}
	return nil
}

func stepsJSON() []any {
	step := func(title string) map[string]any {
		return map[string]any{"title": title, "description": "do " + title, "files": []any{title + ".go"}, "tests": []any{title + "_test.go"}}
	}
	return []any{step("step one"), step("step two")}
}

func planArtifact(t *testing.T, turnID string, rev uint64, rhd bool) []byte {
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "plan", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": decisionQuestion(rhd),
		"plan_markdown": "architecture prose", "steps": stepsJSON(), "risks": []any{"a risk"}, "open_questions": []any{},
	})
}

func addCheck(key, target string) map[string]any {
	var tv any
	if target != "" {
		tv = target
	}
	return map[string]any{"key": key, "description": "check " + key, "evidence": "ev " + key, "action": "add", "target_step": tv}
}

func critiqueArtifact(t *testing.T, turnID string, rev uint64, verdict string, rhd bool, severities []string, ops []map[string]any) []byte {
	findings := make([]any, 0, len(severities))
	for i, sev := range severities {
		findings = append(findings, map[string]any{
			"key": "finding-" + string(rune('a'+i)), "kind": "new", "category": "safety",
			"severity": sev, "file": nil, "line": nil, "problem": "p", "evidence": "e", "suggested_fix": "s",
		})
	}
	checks := make([]any, 0, len(ops))
	for _, op := range ops {
		checks = append(checks, op)
	}
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "plan_critique", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": decisionQuestion(rhd),
		"verdict": verdict, "findings": findings, "implementation_checks": checks,
		"missing_evidence": []any{}, "simpler_alternative": nil, "notes": "n",
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

func implReport(t *testing.T, turnID string, rev uint64) []byte {
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "implementation_report", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": false, "decision_question": nil,
		"files_changed": []any{"a.go"}, "deviations_from_plan": []any{}, "notes": "n",
	})
}

func checkpointArtifact(t *testing.T, turnID string, rev uint64, verdict string, testsAdequate bool, severities []string) []byte {
	findings := make([]any, 0, len(severities))
	for _, sev := range severities {
		findings = append(findings, map[string]any{"severity": sev, "file": nil, "line": nil, "problem": "p", "evidence": "e", "suggested_fix": "s"})
	}
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "checkpoint_review", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": false, "decision_question": nil,
		"verdict": verdict, "findings": findings, "missing_evidence": []any{},
		"tests_adequate": testsAdequate, "tests_critique": "tc",
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// --- drive helpers ---

func cur(t *testing.T, rn *Run) state.RunState {
	t.Helper()
	rs, ok, err := rn.state.Load()
	if err != nil || !ok {
		t.Fatalf("load state: ok=%v err=%v", ok, err)
	}
	return rs
}

func submitOK(t *testing.T, rn *Run, sess string, raw []byte) transport.SubmitResult {
	t.Helper()
	res, err := rn.Submit(context.Background(), sess, raw)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return res
}

// --- tests ---

// The full non-gated chain through the real stores, exercising a plan reviewer retry,
// a revise cycle, a checkpoint reviewer retry, CHECKPOINT->FIX, FIX->CHECKPOINT, a
// next-step advance, and the deferred final TESTS edge.
func TestE2ENonGatedChain(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	// PLAN_DRAFT (lead).
	rs := cur(t, rn)
	if rs.Phase != state.PhasePlanDraft {
		t.Fatalf("not at PLAN_DRAFT: %s", rs.Phase)
	}
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))

	// PLAN_CRITIQUE reviewer retry: REVISE with only a nit -> stays at PLAN_CRITIQUE.
	rs = cur(t, rn)
	if rs.Phase != state.PhasePlanCritique {
		t.Fatalf("not at PLAN_CRITIQUE: %s", rs.Phase)
	}
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", false, []string{"nit"}, nil))
	rs = cur(t, rn)
	if rs.Phase != state.PhasePlanCritique || rs.CandidatePlan == nil {
		t.Fatalf("reviewer retry should stay at PLAN_CRITIQUE: %+v", rs)
	}

	// PLAN_CRITIQUE REVISE with a blocking finding + a check add -> PLAN_REVISE.
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", false, []string{"blocking"}, []map[string]any{addCheck("chk-1", "step one")}))
	rs = cur(t, rn)
	if rs.Phase != state.PhasePlanRevise || rs.PendingFindings == nil || rs.Counters.PlanRevisions != 1 {
		t.Fatalf("not at PLAN_REVISE with pending: %+v", rs)
	}

	// PLAN_REVISE answering the finding exactly (lead) -> PLAN_CRITIQUE.
	submitOK(t, rn, lead, revisionArtifact(t, rs.Assignment.ID, rs.Revision, rs.CandidatePlan.Digest, rs.PendingFindings.Keys))

	// PLAN_CRITIQUE AGREE (targets valid) -> promote to IMPLEMENT_STEP.
	rs = cur(t, rn)
	if rs.Phase != state.PhasePlanCritique || rs.PendingFindings != nil {
		t.Fatalf("not back at PLAN_CRITIQUE: %+v", rs)
	}
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseImplementStep || rs.AgreedPlan == nil || rs.StepIndex == nil || *rs.StepIndex != 0 {
		t.Fatalf("not promoted to IMPLEMENT_STEP: %+v", rs)
	}

	// IMPLEMENT_STEP (lead) -> CHECKPOINT.
	submitOK(t, rn, lead, implReport(t, rs.Assignment.ID, rs.Revision))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseCheckpoint {
		t.Fatalf("not at CHECKPOINT: %s", rs.Phase)
	}

	// CHECKPOINT reviewer retry: REVISE, only a nit, tests adequate -> stays CHECKPOINT.
	submitOK(t, rn, pair, checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", true, []string{"nit"}))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseCheckpoint {
		t.Fatalf("checkpoint reviewer retry should stay at CHECKPOINT: %s", rs.Phase)
	}

	// CHECKPOINT REVISE with a blocking finding -> FIX.
	submitOK(t, rn, pair, checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", true, []string{"blocking"}))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseFix || rs.FixReturn != state.PhaseCheckpoint || rs.Counters.StepFixes[0] != 1 {
		t.Fatalf("not at FIX: %+v", rs)
	}

	// FIX implementation report -> back to CHECKPOINT.
	submitOK(t, rn, lead, implReport(t, rs.Assignment.ID, rs.Revision))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseCheckpoint || rs.FixReturn != "" {
		t.Fatalf("FIX did not return to CHECKPOINT: %+v", rs)
	}

	// CHECKPOINT AGREE on the non-final step -> next IMPLEMENT_STEP.
	submitOK(t, rn, pair, checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", true, nil))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseImplementStep || *rs.StepIndex != 1 {
		t.Fatalf("not advanced to step 1: %+v", rs)
	}

	// IMPLEMENT the final step, then AGREE on it -> the deferred TESTS edge.
	submitOK(t, rn, lead, implReport(t, rs.Assignment.ID, rs.Revision))
	rs = cur(t, rn)
	if _, err := rn.Submit(context.Background(), pair, checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", true, nil)); !errors.Is(err, engine.ErrPhaseUnsupported) {
		t.Fatalf("final checkpoint should reject with the deferred TESTS edge; err = %v", err)
	}
}

// A human-decision plan draft opens a gate and pauses the run.
func TestE2EHumanGatePausesRun(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, true)) // requires_human_decision

	rs = cur(t, rn)
	if rs.Lifecycle != state.LifecyclePaused || rs.Gate == nil || rs.Pause == nil || rs.Assignment != nil {
		t.Fatalf("run did not pause at a human gate: %+v", rs)
	}
	if rs.Pause.Kind != state.PauseHumanDecision {
		t.Fatalf("pause kind = %v, want human_decision", rs.Pause.Kind)
	}

	// A fresh submit against a paused run is refused with no acceptance.
	if _, err := rn.Submit(context.Background(), lead, planArtifact(t, rs.Gate.ID, rs.Revision, false)); err == nil {
		t.Fatal("a submit against a paused run should be refused")
	}
}

// countingRNG counts bytes drawn, proving pre-minted candidates are not regenerated.
type countingRNG struct {
	mu sync.Mutex
	r  io.Reader
	n  int
}

func (c *countingRNG) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, err := c.r.Read(p)
	c.n += k
	return k, err
}

func (c *countingRNG) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// A submit whose facts and ids were captured at one revision, but whose durable state
// advances (under the real run guard) before transport acquires the lock, is stale:
// no artifact is published, the state advanced only by the deliberate reissue, and the
// pre-minted candidate pair is NOT regenerated after the barrier releases.
func TestE2EStalePrecomputeRace(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	rng := &countingRNG{r: rand.Reader}
	rn, err := OpenRun(repo, runID, rng)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn) // PLAN_DRAFT at revision R
	staleRev := rs.Revision
	draft := planArtifact(t, rs.Assignment.ID, staleRev, false)
	n, _ := transport.Normalize(draft)

	advanced := false
	hooks := &submitHooks{afterPrecompute: func() {
		// After facts/ids are captured, advance the durable state under the real run
		// guard (reissue the same assignment at the next generation).
		g, ok, aerr := genstore.Acquire(rn.RunLock())
		if aerr != nil || !ok {
			t.Errorf("advance acquire: ok=%v err=%v", ok, aerr)
			return
		}
		defer g.Release()
		if _, merr := rn.state.MutateLocked(g, staleRev, func(gen uint64, next *state.RunState) error {
			next.Assignment = &state.Ref{ID: next.Assignment.ID, IssuedRevision: gen}
			return nil
		}); merr != nil {
			t.Errorf("advance: %v", merr)
			return
		}
		advanced = true
	}}
	ctx := withHooks(context.Background(), hooks)

	before := rng.count()
	_, err = rn.Submit(ctx, lead, draft)
	var se *transport.StaleError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want StaleError", err)
	}
	if !advanced {
		t.Fatal("the advance barrier did not run")
	}
	// The pre-minted pair (two candidates, 32 bytes) was drawn once and not regenerated.
	if got := rng.count() - before; got != 32 {
		t.Fatalf("RNG consumed %d bytes, want 32 (no regeneration after release)", got)
	}
	// State advanced by exactly the deliberate reissue; the blocked turn is unaccepted
	// and its artifact was never published.
	rs = cur(t, rn)
	if rs.Revision != staleRev+1 {
		t.Fatalf("revision = %d, want %d", rs.Revision, staleRev+1)
	}
	if _, seen := rs.AcceptedTurns[n.TurnID]; seen {
		t.Fatal("the stale turn was accepted")
	}
	if _, gerr := rn.store.Get(n.TurnID, n.Digest); gerr == nil {
		t.Fatal("an artifact was published for the stale submission")
	}
}

// compareRefs rejects any disagreement between the reconstructed refs and the durable
// state (the fact-drift guard the loader applies before Project). State validation
// prevents a genuinely-drifted state from ever persisting, so this exercises the guard
// directly against each perturbed field.
func TestCompareRefsDrift(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	src := state.EventRef{TurnID: "turn-" + strings.Repeat("b", 32), Digest: hex64}
	pending := &state.FindingObligations{Source: src, Keys: []string{"finding-a"}}
	base := func() state.RunState {
		return state.RunState{
			Phase:           state.PhasePlanRevise,
			CandidatePlan:   &state.PlanRef{Source: src, Digest: hex64, StepCount: 2},
			CandidateChecks: &state.CheckSetRef{Keys: []string{"chk-1"}, Digest: strings.Repeat("c", 64)},
			PendingFindings: &state.FindingObligations{Source: src, Keys: []string{"finding-a"}},
		}
	}
	good := engine.CandidateRefs{
		Source: src, PlanDigest: hex64, StepCount: 2,
		CheckKeys: []string{"chk-1"}, CheckDigest: strings.Repeat("c", 64),
		ExpectedPhase: state.PhasePlanRevise, PendingFindings: pending,
	}
	if err := compareRefs(good, base()); err != nil {
		t.Fatalf("matching refs should pass: %v", err)
	}

	perturb := []struct {
		name string
		mut  func(*engine.CandidateRefs)
	}{
		{"plan source", func(r *engine.CandidateRefs) { r.Source.TurnID = "turn-" + strings.Repeat("z", 32) }},
		{"plan digest", func(r *engine.CandidateRefs) { r.PlanDigest = strings.Repeat("d", 64) }},
		{"step count", func(r *engine.CandidateRefs) { r.StepCount = 3 }},
		{"check keys", func(r *engine.CandidateRefs) { r.CheckKeys = []string{"chk-2"} }},
		{"check digest", func(r *engine.CandidateRefs) { r.CheckDigest = strings.Repeat("e", 64) }},
		{"expected phase", func(r *engine.CandidateRefs) { r.ExpectedPhase = state.PhasePlanCritique }},
		{"pending keys", func(r *engine.CandidateRefs) {
			r.PendingFindings = &state.FindingObligations{Source: src, Keys: []string{"finding-z"}}
		}},
		{"pending nil", func(r *engine.CandidateRefs) { r.PendingFindings = nil }},
	}
	for _, p := range perturb {
		t.Run(p.name, func(t *testing.T) {
			bad := good
			p.mut(&bad)
			if err := compareRefs(bad, base()); !errors.Is(err, ErrFactDrift) {
				t.Fatalf("%s: err = %v, want ErrFactDrift", p.name, err)
			}
		})
	}
}

// failAfterN yields n bytes then errors, proving an idempotent replay never mints.
type failAfterN struct {
	mu   sync.Mutex
	left int
}

func (f *failAfterN) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.left <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := len(p)
	if n > f.left {
		n = f.left
	}
	for i := 0; i < n; i++ {
		p[i] = byte(0x40 + (f.left+i)%16) // varied so minted ids do not collide
	}
	f.left -= n
	return n, nil
}

// An idempotent replay of an already-accepted turn re-confirms the receipt without
// minting — proved by an RNG that is exhausted after the first (real) mint.
func TestE2EReplaySkipsRNG(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	// Exactly 32 bytes: enough for the two candidates (assignment + gate) the first
	// accept pre-mints, and nothing more.
	rn, err := OpenRun(repo, runID, &failAfterN{left: 32})
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn)
	draft := planArtifact(t, rs.Assignment.ID, rs.Revision, false)
	first := submitOK(t, rn, lead, draft)
	if first.Idempotent {
		t.Fatal("first accept should not be idempotent")
	}
	// The RNG is now exhausted; a replay must succeed WITHOUT minting.
	again, err := rn.Submit(context.Background(), lead, draft)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.Idempotent || again.Receipt != first.Receipt {
		t.Fatalf("replay was not an idempotent re-confirm: %+v", again)
	}
}

// OpenRun refuses a run that is not a completed pairing.
func TestOpenRunRejectsUnpaired(t *testing.T) {
	repo := t.TempDir()
	fa, err := attach.FirstAttach(attach.FirstAttachRequest{
		RepoDir: repo, Agent: state.AgentClaude, OperationID: opID("a"),
		TaskCanonical: taskBytes(), PolicyCanonical: policyBytes(),
		CreatedUnix: 1000, RNG: rand.Reader,
		Base: fakeBase{commit: strings.Repeat("a", 40)}, Worktree: &fakeWorktree{}, Classifier: supportedFS(),
	})
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if _, err := OpenRun(repo, fa.RunID, rand.Reader); !errors.Is(err, ErrNotReady) {
		t.Fatalf("open unpaired err = %v, want ErrNotReady", err)
	}
}

// An actionable critique at an exhausted plan budget parks at a plan-quality gate.
func TestE2EPlanQualityGate(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRunWithPolicy(t, repo, policyBytesWith(0, 3)) // 0 plan rounds
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))
	rs = cur(t, rn)
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", false, []string{"blocking"}, []map[string]any{addCheck("chk-1", "step one")}))

	rs = cur(t, rn)
	if rs.Lifecycle != state.LifecyclePaused || rs.Pause == nil || rs.Pause.Kind != state.PauseQualityBudget {
		t.Fatalf("not paused at a quality gate: %+v", rs)
	}
	if rs.Pause.Budget == nil || rs.Pause.Budget.Kind != state.BudgetPlan {
		t.Fatalf("not a plan-budget gate: %+v", rs.Pause)
	}
	// The gate carries the actionable projection (updated checks + pending) for resume.
	if rs.PendingFindings == nil || rs.CandidateChecks == nil || len(rs.CandidateChecks.Keys) != 1 {
		t.Fatalf("plan gate did not carry the actionable projection: %+v", rs)
	}
}

// A needs-fix checkpoint at an exhausted checkpoint budget parks at a checkpoint-quality gate.
func TestE2ECheckpointQualityGate(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRunWithPolicy(t, repo, policyBytesWith(1, 0)) // 0 checkpoint rounds
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))
	rs = cur(t, rn)
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseImplementStep {
		t.Fatalf("not promoted: %s", rs.Phase)
	}
	submitOK(t, rn, lead, implReport(t, rs.Assignment.ID, rs.Revision))
	rs = cur(t, rn)
	submitOK(t, rn, pair, checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", true, []string{"blocking"}))

	rs = cur(t, rn)
	if rs.Lifecycle != state.LifecyclePaused || rs.Pause == nil || rs.Pause.Kind != state.PauseQualityBudget {
		t.Fatalf("not paused at a quality gate: %+v", rs)
	}
	if rs.Pause.Budget == nil || rs.Pause.Budget.Kind != state.BudgetCheckpoint {
		t.Fatalf("not a checkpoint-budget gate: %+v", rs.Pause)
	}
}

// A completed pair journal that vanishes after OpenRun forces recovery — the per-submit
// seam does not fail open on Absent.
func TestE2EJournalVanishedRecovery(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	if err := os.RemoveAll(rn.loc.AttachDir); err != nil {
		t.Fatalf("remove journal: %v", err)
	}
	rs := cur(t, rn)
	draft := planArtifact(t, rs.Assignment.ID, rs.Revision, false)
	n, _ := transport.Normalize(draft)
	if _, err := rn.Submit(context.Background(), lead, draft); !errors.Is(err, transport.ErrRecoveryRequired) {
		t.Fatalf("err = %v, want ErrRecoveryRequired", err)
	}
	if _, gerr := rn.store.Get(rs.Assignment.ID, n.Digest); gerr == nil {
		t.Fatal("an artifact was published despite recovery-required")
	}
	if cur(t, rn).Phase != state.PhasePlanDraft {
		t.Fatal("the run advanced despite recovery-required")
	}
}

// Two concurrent submits at PLAN_CRITIQUE prepare facts concurrently OFF the guard: a
// fact-preparation barrier proves BOTH reached the off-guard fact path before either
// enters transport, then they linearize to exactly one fresh acceptance.
func TestE2EConcurrentCritiquePrep(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))
	rs = cur(t, rn) // PLAN_CRITIQUE
	critique := critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil)

	// Barrier: neither submit may leave fact preparation until BOTH have arrived.
	var reached sync.WaitGroup
	reached.Add(2)
	proceed := make(chan struct{})
	hooks := &submitHooks{afterFacts: func() { reached.Done(); <-proceed }}
	go func() { reached.Wait(); close(proceed) }()
	ctx := withHooks(context.Background(), hooks)

	var wg sync.WaitGroup
	res := make([]transport.SubmitResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res[idx], errs[idx] = rn.Submit(ctx, pair, critique)
		}(i)
	}
	wg.Wait()

	fresh := 0
	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if !res[i].Idempotent {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("want exactly one fresh acceptance, got %d", fresh)
	}
	if cur(t, rn).Phase != state.PhaseImplementStep {
		t.Fatalf("run did not promote exactly once")
	}
}

// Close blocks until an in-flight submit completes, so it never releases the artifact
// store under an active Get/Put. The barrier is placed during precompute (afterFacts),
// covering the in-flight precompute/submit lifetime.
func TestE2ECloseWaitsForInflight(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	hooks := &submitHooks{afterFacts: func() { close(entered); <-release }}
	ctx := withHooks(context.Background(), hooks)

	rs := cur(t, rn)
	draft := planArtifact(t, rs.Assignment.ID, rs.Revision, false)
	submitDone := make(chan error, 1)
	go func() { _, e := rn.Submit(ctx, lead, draft); submitDone <- e }()
	<-entered // the submit is paused mid-precompute, holding the read lock

	closeDone := make(chan error, 1)
	go func() { closeDone <- rn.Close() }()
	select {
	case <-closeDone:
		t.Fatal("Close returned while a submit was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release) // let the submit finish
	if e := <-submitDone; e != nil {
		t.Fatalf("submit: %v", e)
	}
	if e := <-closeDone; e != nil {
		t.Fatalf("close: %v", e)
	}
}

// A session superseded by a later Registry generation can no longer submit; the new
// session can. (Replacement is not yet a coordinator feature — a direct Registry
// mutation stands in for the ordering it will produce.)
func TestE2EReplacedSessionUnauthorized(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false)) // -> PLAN_CRITIQUE (pair owns it)

	newSess, err := state.MintSessionID(rand.Reader, nil)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	g, ok, err := genstore.Acquire(rn.RunLock())
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	reg, _, _ := rn.registry.Load()
	_, err = rn.registry.MutateLocked(g, reg.Revision, func(rev uint64, next *state.Registry) error {
		next.Pair.Sessions = append(next.Pair.Sessions, state.SessionRecord{
			SessionID: newSess, Generation: uint64(len(next.Pair.Sessions)) + 1, IssuedRegistryRevision: rev,
		})
		next.Pair.CurrentSessionID = newSess
		return nil
	})
	_ = g.Release()
	if err != nil {
		t.Fatalf("supersede pair session: %v", err)
	}

	rs = cur(t, rn)
	if _, err := rn.Submit(context.Background(), pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil)); !errors.Is(err, transport.ErrUnauthorized) {
		t.Fatalf("superseded session err = %v, want ErrUnauthorized", err)
	}
	// The new current session submits successfully.
	submitOK(t, rn, newSess, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil))
	if cur(t, rn).Phase != state.PhaseImplementStep {
		t.Fatal("the new session's submit did not promote")
	}
}

// openPaired opens a Run over a freshly paired repo (for direct loader unit tests).
func openPaired(t *testing.T) *Run {
	t.Helper()
	repo := t.TempDir()
	runID, _, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	t.Cleanup(func() { rn.Close() })
	return rn
}

func hexTurn(i int) string { return fmt.Sprintf("turn-%032x", i) }

// The loader rejects an over-count BEFORE loading any artifact: the store is empty, so
// a Get would fail with ErrEvidence — an ErrHistoryTooLarge proves the count check ran
// first.
func TestLoadFactsOverCountBeforeGet(t *testing.T) {
	rn := openPaired(t)
	snap := state.RunState{Phase: state.PhasePlanCritique, AcceptedTurns: map[string]state.AcceptedTurn{}}
	for i := 0; i <= engine.MaxPlanningArtifacts; i++ {
		snap.AcceptedTurns[hexTurn(i)] = state.AcceptedTurn{
			ArtifactDigest: strings.Repeat("a", 64), Phase: state.PhasePlanDraft,
			Receipt: state.Receipt{Revision: uint64(i + 1)},
		}
	}
	if _, _, err := rn.loadFacts(snap); !errors.Is(err, engine.ErrHistoryTooLarge) {
		t.Fatalf("err = %v, want ErrHistoryTooLarge", err)
	}
}

// A referenced artifact missing from the store is ErrEvidence.
func TestLoadFactsMissingEvidence(t *testing.T) {
	rn := openPaired(t)
	snap := state.RunState{Phase: state.PhasePlanCritique, AcceptedTurns: map[string]state.AcceptedTurn{
		hexTurn(1): {ArtifactDigest: strings.Repeat("b", 64), Phase: state.PhasePlanDraft, Receipt: state.Receipt{Revision: 1}},
	}}
	if _, _, err := rn.loadFacts(snap); !errors.Is(err, ErrEvidence) {
		t.Fatalf("err = %v, want ErrEvidence", err)
	}
}

// The loader rejects when the cumulative artifact bytes exceed the materialize bound.
func TestLoadFactsByteOverflow(t *testing.T) {
	rn := openPaired(t)
	pad := strings.Repeat("x", 64000) // near the 64 KiB plan_markdown cap
	snap := state.RunState{Phase: state.PhasePlanCritique, AcceptedTurns: map[string]state.AcceptedTurn{}}
	// Enough ~64 KB artifacts to exceed the 8 MiB cumulative bound.
	count := engine.MaxMaterializeBytes/64000 + 4
	for i := 0; i < count; i++ {
		turnID := hexTurn(i)
		art := mustJSON(t, map[string]any{
			"protocol_version": 1, "message_type": "plan", "turn_id": turnID, "state_revision": 1,
			"human_context": nil, "requires_human_decision": false, "decision_question": nil,
			"plan_markdown": pad, "steps": stepsJSON(), "risks": []any{}, "open_questions": []any{},
		})
		nn, err := transport.Normalize(art)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if err := rn.store.Put(turnID, nn.Digest, []byte(nn.CanonicalRedacted)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		snap.AcceptedTurns[turnID] = state.AcceptedTurn{
			ArtifactDigest: nn.Digest, Phase: state.PhasePlanDraft, Receipt: state.Receipt{Revision: uint64(i + 1)},
		}
	}
	if _, _, err := rn.loadFacts(snap); !errors.Is(err, engine.ErrHistoryTooLarge) {
		t.Fatalf("err = %v, want ErrHistoryTooLarge", err)
	}
}

// A closed run refuses further submits.
func TestCloseRefusesSubmit(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	if err := rn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := rn.Close(); err != nil {
		t.Fatalf("second close should be a no-op: %v", err)
	}
	rs := cur(t, rn)
	if _, err := rn.Submit(context.Background(), lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false)); !errors.Is(err, ErrClosed) {
		t.Fatalf("submit after close err = %v, want ErrClosed", err)
	}
}
