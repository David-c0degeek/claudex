package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/engine"
	"github.com/David-c0degeek/claudex/internal/fsclass"
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

func policyBytes() []byte {
	p := config.DefaultRunPolicy()
	p.TestGate = config.TestGate{Disabled: true}
	b, _ := json.Marshal(p)
	return b
}

func taskBytes() []byte {
	return []byte(`{"schema_version":1,"goal":"drive the pairing loop","current_behavior":"none","desired_behavior":"two terminals converge","scope":"coordinator core","non_goals":[],"constraints":[],"acceptance_criteria":["it works"],"required_tests":[],"relevant_files":[],"open_questions":[]}`)
}

// newPairedRun bootstraps a lead-claude run and fills the codex pair, leaving the run
// at PLAN_DRAFT with the lead's first turn issued.
func newPairedRun(t *testing.T, repo string) (runID, lead, pair string) {
	t.Helper()
	fa, err := attach.FirstAttach(attach.FirstAttachRequest{
		RepoDir: repo, Agent: state.AgentClaude, OperationID: opID("a"),
		TaskCanonical: taskBytes(), PolicyCanonical: policyBytes(),
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

// The full non-gated chain through the real stores: plan -> critique(revise) ->
// revision -> critique(agree/promote) -> implement -> checkpoint(next) -> implement ->
// final checkpoint (the deferred TESTS edge).
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

	// PLAN_CRITIQUE REVISE with a blocking finding + a check add (pair).
	rs = cur(t, rn)
	if rs.Phase != state.PhasePlanCritique {
		t.Fatalf("not at PLAN_CRITIQUE: %s", rs.Phase)
	}
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", false, []string{"blocking"}, []map[string]any{addCheck("chk-1", "step one")}))

	// PLAN_REVISE answering the finding exactly (lead).
	rs = cur(t, rn)
	if rs.Phase != state.PhasePlanRevise || rs.PendingFindings == nil {
		t.Fatalf("not at PLAN_REVISE with pending: %+v", rs)
	}
	submitOK(t, rn, lead, revisionArtifact(t, rs.Assignment.ID, rs.Revision, rs.CandidatePlan.Digest, rs.PendingFindings.Keys))

	// PLAN_CRITIQUE AGREE (targets valid) -> promote to IMPLEMENT_STEP (pair).
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

	// CHECKPOINT AGREE on the non-final step (pair) -> next IMPLEMENT_STEP.
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

// A submit prepared against a superseded revision is stale.
func TestE2EStaleSubmit(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn)
	staleRev := rs.Revision
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, staleRev, false)) // advances past staleRev

	// The current critique turn submitted against the superseded revision is stale
	// (its turn is not yet accepted, so staleness — not replay — decides).
	rs = cur(t, rn)
	var se *transport.StaleError
	_, err = rn.Submit(context.Background(), pair, critiqueArtifact(t, rs.Assignment.ID, staleRev, "AGREE", false, nil, nil))
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want StaleError", err)
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
	// Exactly 16 bytes: enough for the single mint the first accept performs.
	rn, err := OpenRun(repo, runID, &failAfterN{left: 16})
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

// Two concurrent submits of the same turn linearize under the run lock: exactly one
// fresh acceptance, the other an idempotent replay; the run advances once.
func TestE2EConcurrentSubmitLinearizes(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn)
	draft := planArtifact(t, rs.Assignment.ID, rs.Revision, false)

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]transport.SubmitResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = rn.Submit(context.Background(), lead, draft)
		}(i)
	}
	close(start)
	wg.Wait()

	fresh := 0
	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if !results[i].Idempotent {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("want exactly one fresh acceptance, got %d", fresh)
	}
	if cur(t, rn).Phase != state.PhasePlanCritique {
		t.Fatalf("run did not advance exactly once")
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
