package coordinator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/engine"
	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/fsclass"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/reviewpacket"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// --- attach seams (fakes) ---

func opID(c string) string { return "op-" + strings.Repeat(c, 32) }

type fakeBase struct{ commit string }

func (f fakeBase) ResolveBase(context.Context, string, string) (string, error) { return f.commit, nil }

type fakePreflight struct{}

func (fakePreflight) Preflight(context.Context, string) error { return nil }

type fakeWorktree struct{ applied bool }

func (f *fakeWorktree) ObserveWorktree(context.Context, string, attach.BootstrapIntent) (txn.StepStatus, error) {
	if f.applied {
		return txn.StatusApplied, nil
	}
	return txn.StatusNotApplied, nil
}

func (f *fakeWorktree) ApplyWorktree(context.Context, string, attach.BootstrapIntent) error {
	f.applied = true
	return nil
}

func (f *fakeWorktree) ConfirmWorktree(context.Context, string, attach.BootstrapIntent) error {
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
	return []byte(`{"schema_version":2,"goal":"drive the pairing loop","current_behavior":"none","desired_behavior":"two terminals converge","scope":"coordinator core","non_goals":[],"constraints":[],"acceptance_criteria":["it works"],"required_tests":[],"relevant_files":[],"relevant_repo_paths":["README"],"open_questions":[]}`)
}

// initGitRepo turns the empty repo dir into a real one-commit repository on branch
// main with .claudex git-ignored — the shape FirstAttach's real preflight requires
// and the git commit transaction snapshots against.
func initGitRepo(t *testing.T, repo string) {
	t.Helper()
	g, err := gitx.New()
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	env := map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
	}
	mustGit(t, g, repo, nil, "init", "-b", "main")
	writeRepoFile(t, filepath.Join(repo, ".gitignore"), ".claudex/\n")
	writeRepoFile(t, filepath.Join(repo, "README"), "hi\n")
	mustGit(t, g, repo, env, "add", "-A")
	mustGit(t, g, repo, env, "commit", "-m", "init")
}

func mustGit(t *testing.T, g *gitx.Git, dir string, env map[string]string, args ...string) []byte {
	t.Helper()
	out, err := g.Run(context.Background(), dir, env, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func writeRepoFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// editWorktree makes a fresh tracked-content change in the run's linked worktree so
// the next IMPLEMENT/FIX submit snapshots a non-empty tree (the transaction rejects
// a no-op snapshot).
var editSeq int

func editWorktree(t *testing.T, rn *Run) {
	t.Helper()
	editSeq++
	writeRepoFile(t, filepath.Join(rn.runWorktree(), "work.txt"), fmt.Sprintf("edit %d\n", editSeq))
}

// newPairedRun bootstraps a lead-claude run over a REAL git repository (created in
// the empty repo dir) and fills the codex pair, leaving the run at PLAN_DRAFT with
// the lead's first turn issued. The real seams matter: an IMPLEMENT/FIX
// submit routes through the git commit transaction against the real linked worktree.
func newPairedRun(t *testing.T, repo string) (runID, lead, pair string) {
	return newPairedRunWithPolicy(t, repo, policyBytes())
}

func newPairedRunWithPolicy(t *testing.T, repo string, pol []byte) (runID, lead, pair string) {
	t.Helper()
	initGitRepo(t, repo)
	g, err := gitx.New()
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	base, pre, wt := attach.NewGitSeams(g)
	fa, err := attach.FirstAttach(context.Background(), attach.FirstAttachRequest{
		RepoDir: repo, Agent: state.AgentClaude, OperationID: opID("a"),
		TaskCanonical: taskBytes(), PolicyCanonical: pol,
		CreatedUnix: 1000, RNG: rand.Reader,
		Base: base, Preflight: pre, Worktree: wt, Classifier: supportedFS(),
	})
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	ja, err := attach.JoinAttach(attach.JoinAttachRequest{
		RepoDir: repo, RunID: fa.RunID, OperationID: opID("b"),
		Agent: state.AgentCodex, Role: state.SlotPair, Now: 2000, RNG: rand.Reader,
		Evidence: testIssuer(t, repo, fa.RunID),
	})
	if err != nil {
		t.Fatalf("join attach: %v", err)
	}
	return fa.RunID, fa.SessionID, ja.SessionID
}

// testIssuer builds the REAL review-packet issuer for a test run. The coordinator suite drives real
// linked worktrees and real commits, so it exercises genuine packet production end to end rather
// than a stub: every read-only turn these tests issue publishes a packet resolved from the committed
// source object.
func testIssuer(t *testing.T, repo, runID string) attach.EvidenceIssuer {
	t.Helper()
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve run for the evidence issuer: %v", err)
	}
	g, err := gitx.New()
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	store, err := transport.NewArtifactStore(loc.ArtifactsDir)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return reviewpacket.NewIssuer(context.Background(), g, repo, loc.RunDir, loc.EvidenceDir, store)
}

// packetEntryBytes returns the payload a packet records under a logical path. The manifest maps the
// lossless base64 path to the blob's sha256, which is also the blob's name in the packet root.
func packetEntryBytes(t *testing.T, rn *Run, ev *state.AssignmentEvidence, logicalPath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rn.loc.EvidenceDir, ev.ManifestRelPath))
	if err != nil {
		t.Fatalf("read packet manifest: %v", err)
	}
	var m struct {
		Entries []struct {
			PathB64 string `json:"path_b64"`
			SHA256  string `json:"sha256"`
		} `json:"entries"`
	}
	if uerr := json.Unmarshal(raw, &m); uerr != nil {
		t.Fatalf("decode packet manifest: %v", uerr)
	}
	want := base64.StdEncoding.EncodeToString([]byte(logicalPath))
	for _, e := range m.Entries {
		if e.PathB64 == want {
			body, rerr := os.ReadFile(filepath.Join(rn.loc.EvidenceDir, filepath.Dir(ev.ManifestRelPath), e.SHA256))
			if rerr != nil {
				t.Fatalf("read packet blob for %s: %v", logicalPath, rerr)
			}
			return body
		}
	}
	t.Fatalf("packet has no entry for %s", logicalPath)
	return nil
}

// A plan_revision is a PATCH: the fixture leaves every section null and inherits them from the base.
// So the packet must carry the MATERIALIZED resulting plan, not the revision artifact — otherwise a
// reviewer sees only the responses and never the plan those responses produced.
func assertPlanIsMaterialized(t *testing.T, rn *Run, rs state.RunState, logicalPath string) {
	t.Helper()
	if rs.Evidence == nil {
		t.Fatalf("%s assignment must carry an evidence binding", rs.Phase)
	}
	body := packetEntryBytes(t, rn, rs.Evidence, logicalPath)
	// The base draft's content, which the null revision inherited rather than restated.
	for _, want := range []string{"architecture prose", "a risk", "step one"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("%s in the %s packet is missing inherited content %q; got %s", logicalPath, rs.Phase, want, body)
		}
	}
	// It is the plan DOCUMENT, not the submit envelope: a revision artifact would carry responses and
	// a message type, and the materialized document carries neither.
	for _, unwanted := range []string{"responses", "message_type", "plan_revision"} {
		if bytes.Contains(body, []byte(unwanted)) {
			t.Fatalf("%s in the %s packet looks like a submit artifact (contains %q): %s", logicalPath, rs.Phase, unwanted, body)
		}
	}
	// The bytes must be the exact document state froze the digest of.
	if d, derr := canonjson.Digest(body); derr != nil {
		t.Fatalf("digest the materialized plan: %v", derr)
	} else if want := frozenPlanDigest(rs, logicalPath); d != want {
		t.Fatalf("%s digest = %s, want the frozen %s", logicalPath, d, want)
	}
}

// frozenPlanDigest is the materialized plan digest run state carries for this packet entry.
func frozenPlanDigest(rs state.RunState, logicalPath string) string {
	if logicalPath == evidence.ReservedContextPrefix+"agreed-plan.json" {
		return rs.AgreedPlan.Plan.Digest
	}
	return rs.CandidatePlan.Digest
}

// assertCheckpointPacket verifies the packet bound to a live CHECKPOINT turn and checks it contains
// what a step reviewer needs. It re-verifies through the real verifier, so the assertion covers the
// binding, the packet, and its content together.
func assertCheckpointPacket(t *testing.T, rn *Run, rs state.RunState) {
	t.Helper()
	if rs.Evidence == nil {
		t.Fatal("a CHECKPOINT assignment must carry an evidence binding")
	}
	src, ok := state.LatestGitCommit(rs)
	if !ok {
		t.Fatal("a CHECKPOINT follows an accepted implementation commit")
	}
	expect := evidence.Expectation{
		RunID: rs.RunID, TurnID: rs.Assignment.ID, Phase: string(rs.Phase),
		Source: evidence.SourceObject{Commit: src.Commit, Tree: src.Tree},
	}
	ref := evidence.EvidenceRef{ManifestRelPath: rs.Evidence.ManifestRelPath, RootDigest: rs.Evidence.RootDigest}
	lim := rs.EffectivePolicy.Limits
	bounds := evidence.Bounds{
		MaxTotalBytes: lim.EvidenceMaxTotalBytes,
		MaxFileBytes:  lim.EvidenceMaxFileBytes,
		MaxRequests:   lim.EvidenceMaxRequests,
	}
	if err := evidence.VerifyRef(rn.loc.EvidenceDir, ref, bounds, expect); err != nil {
		t.Fatalf("the bound CHECKPOINT packet does not verify: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(rn.loc.EvidenceDir, rs.Evidence.ManifestRelPath))
	if err != nil {
		t.Fatalf("read packet manifest: %v", err)
	}
	// Paths are stored losslessly as base64, so assert on the encoded form.
	for _, want := range []string{
		evidence.ReservedContextPrefix + "agreed-plan.json",
		evidence.ReservedContextPrefix + "implementation-report.json",
		"work.txt", // the file editWorktree changed for this step
	} {
		enc := base64.StdEncoding.EncodeToString([]byte(want))
		if !bytes.Contains(manifest, []byte(`"path_b64":"`+enc+`"`)) {
			t.Fatalf("CHECKPOINT packet is missing %q", want)
		}
	}
	// The step review is scoped to the change: the run's declared relevant_repo_paths (README) belong
	// to the PLANNING packet, not this one.
	if enc := base64.StdEncoding.EncodeToString([]byte("README")); bytes.Contains(manifest, []byte(`"path_b64":"`+enc+`"`)) {
		t.Fatal("CHECKPOINT packet should carry the step change, not the task-declared plan selection")
	}
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

// verificationArtifact covers the task's single acceptance criterion ("it works"); a "pass"
// with met=true is a consistent acceptance, a "fail" with met=false a consistent rejection.
func verificationArtifact(t *testing.T, turnID string, rev uint64, verdict string, met bool) []byte {
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "verification", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": false, "decision_question": nil,
		"verdict":            verdict,
		"criteria":           []any{map[string]any{"criterion": "it works", "met": met, "evidence": "e"}},
		"scope_expansion":    []any{},
		"tests_meaningful":   true,
		"unsupported_claims": []any{},
		"notes":              "n",
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
// driveToTests drives a freshly-opened paired run through the full non-gated agent chain
// (plan draft/critique/revise, implement/checkpoint over two steps with one FIX round) to
// ownerless TESTS, asserting the path, and returns the RunState at TESTS.
func driveToTests(t *testing.T, rn *Run, lead, pair string) state.RunState {
	t.Helper()

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
	// The critique that follows a REVISION must see the resulting plan, not the null-laden patch:
	// revisionArtifact leaves every section nil, so the base content is inherited, not restated.
	assertPlanIsMaterialized(t, rn, rs, evidence.ReservedContextPrefix+"candidate-plan.json")
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseImplementStep || rs.AgreedPlan == nil || rs.StepIndex == nil || *rs.StepIndex != 0 {
		t.Fatalf("not promoted to IMPLEMENT_STEP: %+v", rs)
	}

	// IMPLEMENT_STEP (lead) -> CHECKPOINT (through the git commit transaction).
	editWorktree(t, rn)
	submitOK(t, rn, lead, implReport(t, rs.Assignment.ID, rs.Revision))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseCheckpoint {
		t.Fatalf("not at CHECKPOINT: %s", rs.Phase)
	}
	// The CHECKPOINT packet must actually carry the step under review: the agreed plan, the
	// implementation report, and the CHANGED file's committed bytes — not the whole tree, and not
	// the worktree.
	assertCheckpointPacket(t, rn, rs)
	assertPlanIsMaterialized(t, rn, rs, evidence.ReservedContextPrefix+"agreed-plan.json")

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
	editWorktree(t, rn)
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

	// IMPLEMENT the final step, then AGREE on it. The final checkpoint is accepted and
	// the run enters ownerless TESTS; this build does not yet author the coordinator
	// TESTS outcome that would leave TESTS.
	editWorktree(t, rn)
	submitOK(t, rn, lead, implReport(t, rs.Assignment.ID, rs.Revision))
	rs = cur(t, rn)
	final := checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", true, nil)
	fn, _ := transport.Normalize(final)
	res := submitOK(t, rn, pair, final)
	if res.Idempotent {
		t.Fatal("the final checkpoint should be a fresh acceptance")
	}
	rs = cur(t, rn)
	if rs.Phase != state.PhaseTests || rs.Assignment != nil {
		t.Fatalf("final checkpoint should enter ownerless TESTS: %+v", rs)
	}
	// The checkpoint artifact was published and accepted.
	if _, gerr := rn.store.Get(fn.TurnID, fn.Digest); gerr != nil {
		t.Fatalf("the accepted checkpoint artifact was not published: %v", gerr)
	}
	return rs
}

// The full non-gated chain reaches ownerless TESTS, and a coordinator-authored TESTS pass
// leaves it for ownerless VERIFY with a fresh threshold one generation past the pair.
func TestE2ENonGatedChain(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	tests := driveToTests(t, rn, lead, pair)

	// The coordinator authors the TESTS pass (ownerless: no agent, no artifact, no id),
	// carrying the revision the outcome was derived against.
	res, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision)
	if err != nil {
		t.Fatalf("submit tests pass: %v", err)
	}
	if res.Revision <= tests.Revision {
		t.Fatalf("committed revision %d did not advance past %d", res.Revision, tests.Revision)
	}
	// Ownerless VERIFY: no assignment, and the fresh threshold is one past the pair
	// session (freshly paired at generation 1, never replaced, so 2).
	next := cur(t, rn)
	if next.Phase != state.PhaseVerify || next.Assignment != nil || next.Verify == nil {
		t.Fatalf("TESTS pass did not enter ownerless VERIFY: %+v", next)
	}
	if next.Verify.RequiredGeneration != 2 {
		t.Fatalf("verify threshold = %d, want 2", next.Verify.RequiredGeneration)
	}
}

// A TESTS fail is applied and leaves the ownerless TESTS phase (the engine routes it to the
// lead's FIX or the test-budget gate).
func TestSubmitTestOutcomeFailLeavesTests(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)

	// A live FAIL pre-mints its FIX candidate off-guard: it reads the RNG (unlike a pass).
	crng := &countingRNG{r: rand.Reader}
	rn.rng = crng
	if _, err := rn.SubmitTestOutcome(context.Background(), false, strings.Repeat("f", 64), tests.Revision); err != nil {
		t.Fatalf("submit tests fail: %v", err)
	}
	if crng.count() == 0 {
		t.Fatal("a live FAIL should pre-mint the FIX candidate (read the RNG)")
	}
	if next := cur(t, rn); next.Phase == state.PhaseTests {
		t.Fatalf("a TESTS fail should leave the TESTS phase: %+v", next)
	}
}

// A PASS is ownerless (IDNone): it depends on no RNG. Even with an exhausted reader the
// pass commits and enters VERIFY.
func TestSubmitTestOutcomePassNeedsNoRNG(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)

	rn.rng = errReader{} // a PASS must not read the RNG
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision); err != nil {
		t.Fatalf("a PASS must not depend on the RNG: %v", err)
	}
	if cur(t, rn).Phase != state.PhaseVerify {
		t.Fatal("the pass did not enter ownerless VERIFY")
	}
}

// Every request the primitive will reject — a cancelled context, malformed evidence, a stale
// round, or a wrong live phase — mints nothing: it reads no RNG, so a rejected request never
// consumes a future identity and its RNG cannot mask the typed rejection.
func TestSubmitTestOutcomeRejectedMintNothing(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	crng := &countingRNG{r: rand.Reader}
	rn, err := OpenRun(repo, runID, crng)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)
	digest := strings.Repeat("e", 64)

	readsNothing := func(name string, call func()) {
		t.Helper()
		before := crng.count()
		call()
		if got := crng.count() - before; got != 0 {
			t.Fatalf("%s read %d RNG bytes, want 0", name, got)
		}
	}
	readsNothing("cancelled", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rn.SubmitTestOutcome(ctx, false, digest, tests.Revision)
	})
	readsNothing("bad evidence", func() { rn.SubmitTestOutcome(context.Background(), false, "bad", tests.Revision) })
	readsNothing("stale round", func() { rn.SubmitTestOutcome(context.Background(), false, digest, tests.Revision+1) })

	// Wrong live phase (a fresh PLAN_DRAFT run): a FAIL mints nothing.
	repo2 := t.TempDir()
	id2, _, _ := newPairedRun(t, repo2)
	crng2 := &countingRNG{r: rand.Reader}
	rn2, err := OpenRun(repo2, id2, crng2)
	if err != nil {
		t.Fatalf("open run2: %v", err)
	}
	defer rn2.Close()
	before := crng2.count()
	rn2.SubmitTestOutcome(context.Background(), false, digest, cur(t, rn2).Revision)
	if got := crng2.count() - before; got != 0 {
		t.Fatalf("a wrong-phase fail read %d RNG bytes, want 0", got)
	}
}

// SubmitTestOutcome fails closed for a run that is not a live TESTS phase (transport's typed
// ErrNotTestsPhase), distinct from malformed evidence (ErrBadEvidence).
func TestSubmitTestOutcomeWrongPhase(t *testing.T) {
	repo := t.TempDir()
	runID, _, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader) // fresh run at PLAN_DRAFT
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), cur(t, rn).Revision); !errors.Is(err, transport.ErrNotTestsPhase) {
		t.Fatalf("wrong-phase err = %v, want transport.ErrNotTestsPhase", err)
	}
}

// SubmitTestOutcome rejects a malformed (non hex64) evidence digest as ErrBadEvidence — bad
// input and a wrong live phase are distinguishable.
func TestSubmitTestOutcomeBadEvidenceDigest(t *testing.T) {
	rn := openPaired(t)
	if _, err := rn.SubmitTestOutcome(context.Background(), true, "not-a-digest", 1); !errors.Is(err, transport.ErrBadEvidence) {
		t.Fatalf("bad digest err = %v, want transport.ErrBadEvidence", err)
	}
}

// A stale-round outcome (derived against an earlier TESTS revision than the live one) is
// rejected: only the round the outcome was derived from may apply, even though the live
// phase is still TESTS. This is the delayed-runner hazard the expectedRevision guard closes.
func TestSubmitTestOutcomeStaleRoundRejected(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)

	// A fail routes to the lead's FIX (which returns to TESTS); the lead's FIX report lands
	// the run back at a NEW TESTS round at a later revision.
	if _, err := rn.SubmitTestOutcome(context.Background(), false, strings.Repeat("f", 64), tests.Revision); err != nil {
		t.Fatalf("first TESTS fail: %v", err)
	}
	fix := cur(t, rn)
	if fix.Phase != state.PhaseFix || fix.FixReturn != state.PhaseTests {
		t.Fatalf("a TESTS fail did not open a TESTS-returning FIX: %+v", fix)
	}
	editWorktree(t, rn)
	submitOK(t, rn, lead, implReport(t, fix.Assignment.ID, fix.Revision)) // FIX -> TESTS
	back := cur(t, rn)
	if back.Phase != state.PhaseTests || back.Revision <= tests.Revision {
		t.Fatalf("FIX did not return to a later TESTS round: phase=%s rev=%d (was %d)", back.Phase, back.Revision, tests.Revision)
	}

	// A delayed pass carrying the ORIGINAL round's revision is stale, even though the live
	// phase is TESTS again — the delayed-runner hazard the expectedRevision guard closes. The
	// rejection is the typed StaleError, not a generic failure.
	_, err = rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision)
	var stale *transport.StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("stale-round err = %v, want *transport.StaleError", err)
	}
	if after := cur(t, rn); after.Revision != back.Revision {
		t.Fatalf("the stale outcome mutated the run: rev %d -> %d", back.Revision, after.Revision)
	}
}

// An already-cancelled context commits nothing, honoring the transport primitive's
// cancellation contract.
func TestSubmitTestOutcomeContextCancelled(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rn.SubmitTestOutcome(ctx, true, strings.Repeat("e", 64), tests.Revision); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx err = %v, want context.Canceled", err)
	}
	if after := cur(t, rn); after.Revision != tests.Revision || after.Phase != state.PhaseTests {
		t.Fatalf("a cancelled outcome mutated the run: %+v", after)
	}
}

// A pending replacement blocks a coordinator-authored TESTS outcome through the aggregate
// journal reader (transport.ErrRecoveryRequired), before any state mutation.
func TestSubmitTestOutcomeBlockedByPendingReplacement(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	before := driveToTests(t, rn, lead, pair)

	plantPendingReplace(t, repo, runID)
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), before.Revision); !errors.Is(err, transport.ErrRecoveryRequired) {
		t.Fatalf("blocked TESTS outcome err = %v, want transport.ErrRecoveryRequired", err)
	}
	// No durable effect: the run is still at TESTS at the same revision.
	if after := cur(t, rn); after.Revision != before.Revision || after.Phase != state.PhaseTests {
		t.Fatalf("state advanced despite a blocked TESTS outcome: rev %d->%d phase %s",
			before.Revision, after.Revision, after.Phase)
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

// errReader always errors, proving a code path (an ownerless PASS) reads no RNG.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

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
			// Reissuing the turn reissues its evidence binding with it: the two share a lifetime, so
			// a re-pull that rebinds one must rebind the other at the same revision.
			next.Assignment = &state.Ref{ID: next.Assignment.ID, IssuedRevision: gen}
			if next.Evidence != nil {
				next.Evidence.IssuedRevision = gen
			}
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
	fa, err := attach.FirstAttach(context.Background(), attach.FirstAttachRequest{
		RepoDir: repo, Agent: state.AgentClaude, OperationID: opID("a"),
		TaskCanonical: taskBytes(), PolicyCanonical: policyBytes(),
		CreatedUnix: 1000, RNG: rand.Reader,
		Base: fakeBase{commit: strings.Repeat("a", 40)}, Preflight: fakePreflight{}, Worktree: &fakeWorktree{}, Classifier: supportedFS(),
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
	editWorktree(t, rn)
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

	// Supersede the pair session with the real replacement API (no RunState advance).
	replOp, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint operation id: %v", err)
	}
	rep, err := attach.ReplaceAttach(attach.ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: replOp, RNG: rand.Reader,
		Evidence: testIssuer(t, repo, runID),
	})
	if err != nil {
		t.Fatalf("replace pair session: %v", err)
	}
	newSess := rep.SessionID

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

// plantPendingReplace leaves a prepared-but-not-completed record in the run's replacement
// journal — a replacement crashed mid-flight. Here the head is deliberately mis-bound (its
// kind is not the replacement kind), one recovery-required shape the aggregate reader must
// fail closed on; the clean bound-but-pending shape (which maps to recovery) is covered by
// attach's ClassifyReplaceJournal matrix, since only attach's seams can produce it.
func plantPendingReplace(t *testing.T, repo, runID string) {
	t.Helper()
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	g, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil || !ok {
		t.Fatalf("acquire run lock: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	j := txn.Open(loc.ReplaceDir, loc.RunLock)
	plan := txn.Plan{
		Intent: txn.Intent{
			Version: txn.IntentVersion, Kind: "planted-block",
			TxnID:   "run-" + strings.Repeat("e", 32),
			Payload: json.RawMessage(`{"planted":true}`),
		},
		Steps: []txn.Step{{
			Name:           "registry-replace",
			Status:         func() (txn.StepStatus, error) { return txn.StatusNotApplied, nil },
			Apply:          func() error { return errors.New("halt to leave a prepared head") },
			ConfirmDurable: func() error { return nil },
		}},
	}
	if _, err := j.Run(g, plan); err == nil {
		t.Fatal("planted plan should halt with a pending head")
	}
}

// A pending replacement journal blocks a submit: the aggregate reader classifies the
// replacement journal under the submit guard, BEFORE Registry authority, and fails closed
// rather than authorize off a Registry a replacement may be mid-superseding.
func TestSubmitBlockedByPendingReplacement(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn) // the lead owns the PLAN_DRAFT first turn
	before := cur(t, rn)
	plantPendingReplace(t, repo, runID)
	if _, err := rn.Submit(context.Background(), lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false)); !errors.Is(err, transport.ErrRecoveryRequired) {
		t.Fatalf("blocked submit err = %v, want transport.ErrRecoveryRequired", err)
	}
	// No durable effect: a submit refused at the journal gate never appends state (so no
	// accepted turn, hence no published artifact — transport refuses before the Sink).
	if after := cur(t, rn); after.Revision != before.Revision || len(after.AcceptedTurns) != len(before.AcceptedTurns) {
		t.Fatalf("state advanced despite a blocked submit: rev %d->%d, accepted %d->%d",
			before.Revision, after.Revision, len(before.AcceptedTurns), len(after.AcceptedTurns))
	}
}

// OpenRun fails fast when a replacement is pending, rather than opening the run for submits.
func TestOpenRunBlockedByPendingReplacement(t *testing.T) {
	repo := t.TempDir()
	runID, _, _ := newPairedRun(t, repo)
	plantPendingReplace(t, repo, runID)
	if _, err := OpenRun(repo, runID, rand.Reader); err == nil {
		t.Fatal("OpenRun should fail while a replacement is pending")
	}
}

// The real-run activation integration: a coordinator-driven run reaches ownerless VERIFY,
// and a qualifying pair replacement ACTIVATES it — issuing the verifier turn and binding the
// assignment to the run state. This is the 4c-2a activation authority meeting a genuine
// coordinator-driven VERIFY (not a hand-built one).
func TestE2EActivationAtOwnerlessVerify(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)

	// A coordinator TESTS pass enters ownerless VERIFY at threshold 2 (pair generation 1 + 1).
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision); err != nil {
		t.Fatalf("tests pass: %v", err)
	}
	v := cur(t, rn)
	if v.Phase != state.PhaseVerify || v.Assignment != nil || v.Verify == nil || v.Verify.RequiredGeneration != 2 {
		t.Fatalf("not ownerless VERIFY at threshold 2: %+v", v)
	}

	// A pair replacement (generation 1 -> 2, meeting the retained threshold) activates VERIFY.
	op, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint op: %v", err)
	}
	rep, err := attach.ReplaceAttach(attach.ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: op, RNG: rand.Reader,
		Evidence: testIssuer(t, repo, runID),
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if rep.VerifierTurnID == "" {
		t.Fatal("a qualifying pair replacement at ownerless VERIFY must issue a verifier turn")
	}
	// The ownerless VERIFY now has an owner: the verifier assignment, bound to the resulting
	// revision, and the run is still a live VERIFY awaiting that verifier.
	after := cur(t, rn)
	if after.Phase != state.PhaseVerify || after.Assignment == nil || after.Assignment.ID != rep.VerifierTurnID {
		t.Fatalf("activation did not issue the verifier assignment: turn=%q state=%+v", rep.VerifierTurnID, after)
	}
	if after.Assignment.IssuedRevision != after.Revision {
		t.Fatalf("verifier assignment not bound to the current revision: %+v", after.Assignment)
	}
}

// unconfirmedWrite publishes the record then reports a post-commit sync failure, so the
// append is committed-but-durability-unconfirmed.
func unconfirmedWrite(path string, data []byte, perm os.FileMode) error {
	if werr := atomicfile.Write(path, data, perm); werr != nil {
		return werr
	}
	return &atomicfile.PostCommitSyncError{Path: path, Err: errors.New("dir sync failed")}
}

// A TESTS outcome whose durability confirmation PERSISTENTLY fails halts
// (IsDurabilityUnconfirmed) with the committed revision preserved and the run visible at
// VERIFY but not yet durable. A genuine recovery reopen then RE-CONFIRMS the visible state
// durable under the run guard before serving — it does not trust the unconfirmed entry — and
// a reopen whose re-confirmation persistently fails itself halts. Re-deriving a lost-response
// outcome identity is NOT this path's job: the primitive is non-idempotent and outcome
// identity belongs to the external evidence journal (4d); this pins state-entry durability
// recovery and non-reapplication.
func TestSubmitTestOutcomeRecoveryAfterPersistentConfirmFailure(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	tests := driveToTests(t, rn, lead, pair)

	// Persistent-failure-halts witness: the outcome append is visible but its durability
	// confirmation persistently fails.
	rn.state.WithWrite(unconfirmedWrite).WithSyncDir(func(string) error { return errors.New("sync down") })
	res, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision)
	if !genstore.IsDurabilityUnconfirmed(err) {
		t.Fatalf("persistent-confirm-fail err = %v, want IsDurabilityUnconfirmed", err)
	}
	if res.Revision != tests.Revision+1 {
		t.Fatalf("committed revision not preserved: %d, want %d", res.Revision, tests.Revision+1)
	}
	if v := cur(t, rn); v.Phase != state.PhaseVerify {
		t.Fatalf("outcome not visible after the unconfirmed commit: %s", v.Phase)
	}
	if err := rn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Recovery reopen: a fresh handle with a healthy barrier RE-CONFIRMS the visible state
	// durable under the guard before serving. A counting per-call confirmer witnesses the
	// whole chain (journals + stores) was re-confirmed.
	confirmed := 0
	rn2, err := openRun(repo, runID, rand.Reader, func(g *genstore.Guard, loc attach.RunLocation, st *state.Store, reg *state.RegistryStore) error {
		confirmed++
		return defaultRecoverConfirm(g, loc, st, reg)
	})
	if err != nil {
		t.Fatalf("recovery reopen: %v", err)
	}
	defer rn2.Close()
	if confirmed == 0 {
		t.Fatal("the recovery reopen did not re-confirm durability")
	}
	if v := cur(t, rn2); v.Phase != state.PhaseVerify {
		t.Fatalf("the recovered run lost the durable VERIFY: %s", v.Phase)
	}
	// Non-reapplication: a re-invocation of the same round on the recovered run observes the
	// advanced phase and refuses (outcome identity is the evidence journal's, not a re-apply).
	if _, err := rn2.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision); !errors.Is(err, transport.ErrNotTestsPhase) {
		t.Fatalf("re-invoke err = %v, want transport.ErrNotTestsPhase", err)
	}

	// A reopen whose re-confirmation PERSISTENTLY fails halts (recovery-required), never a
	// silent trust of the unconfirmed state.
	_, oerr := openRun(repo, runID, rand.Reader, func(*genstore.Guard, attach.RunLocation, *state.Store, *state.RegistryStore) error {
		return errors.New("persistent re-confirm failure")
	})
	if oerr == nil {
		t.Fatal("a persistent re-confirm failure on reopen must halt the open")
	}
}

// A same-operation activation retry is idempotent: it reconciles to the SAME verifier turn
// without re-minting, and it does not re-issue — the run-state revision, verifier assignment,
// and registry revision are all UNCHANGED by the retry.
func TestActivationIdempotentRetry(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision); err != nil {
		t.Fatalf("tests pass: %v", err)
	}

	op, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint op: %v", err)
	}
	req := attach.ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: op, RNG: rand.Reader,
		Evidence: testIssuer(t, repo, runID),
	}
	rep1, err := attach.ReplaceAttach(req)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	// Capture the activated state + registry: the retry must leave them byte-identical.
	activated := cur(t, rn)
	regBefore, _, _ := rn.registry.Load()

	// Retry the SAME operation with an RNG that ERRORS if minted — proving no re-mint.
	req.RNG = errReader{}
	rep2, err := attach.ReplaceAttach(req)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if rep2.VerifierTurnID != rep1.VerifierTurnID || rep2.SessionID != rep1.SessionID || rep2.Generation != rep1.Generation {
		t.Fatalf("retry did not reconcile to the frozen activation: %+v vs %+v", rep1, rep2)
	}
	// No re-issue: no second state append (same revision + assignment) and no registry advance.
	after := cur(t, rn)
	if after.Revision != activated.Revision {
		t.Fatalf("retry appended a new state generation: rev %d -> %d", activated.Revision, after.Revision)
	}
	if after.Assignment == nil || activated.Assignment == nil || *after.Assignment != *activated.Assignment {
		t.Fatalf("retry changed the verifier assignment: %+v -> %+v", activated.Assignment, after.Assignment)
	}
	if regAfter, _, _ := rn.registry.Load(); regAfter.Revision != regBefore.Revision {
		t.Fatalf("retry advanced the registry: rev %d -> %d", regBefore.Revision, regAfter.Revision)
	}
}

// A persisted activation whose run-state append lands across a genstore GAP still binds and
// recovers: a torn file occupies the next state slot, so the verifier assignment binds at a
// revision past expected+1, and classifyActivation must recognize that as Applied (the exact
// case the slice-ad fix and the iii-b apply fix cover — proven here end-to-end against a real
// VERIFY, not a hand-built one).
func TestActivationBindsAcrossStateGap(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision); err != nil {
		t.Fatalf("tests pass: %v", err)
	}
	v := cur(t, rn) // ownerless VERIFY at revision R

	// Occupy the NEXT state slot (R+1) with a torn file: genstore quarantines it and the
	// activation's append skips to R+2, binding the verifier assignment across the gap.
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	torn := filepath.Join(loc.StateDir, fmt.Sprintf("%012d.gen", v.Revision+1))
	if err := os.WriteFile(torn, []byte("torn quarantined generation"), 0o600); err != nil {
		t.Fatalf("plant torn gen: %v", err)
	}

	op, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint op: %v", err)
	}
	req := attach.ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: op, RNG: rand.Reader,
		Evidence: testIssuer(t, repo, runID),
	}
	rep, err := attach.ReplaceAttach(req)
	if err != nil {
		t.Fatalf("activate across gap: %v", err)
	}
	after := cur(t, rn)
	if after.Assignment == nil || after.Assignment.ID != rep.VerifierTurnID {
		t.Fatalf("activation did not issue the verifier: %+v", after.Assignment)
	}
	if after.Revision <= v.Revision+1 {
		t.Fatalf("activation did not bind across the gap: revision %d, want > %d", after.Revision, v.Revision+1)
	}
	if after.Assignment.IssuedRevision != after.Revision {
		t.Fatalf("assignment not bound to the gapped resulting revision: %+v", after.Assignment)
	}
	// A same-op retry recognizes the gapped activation as Applied (idempotent, no re-mint).
	req.RNG = errReader{}
	rep2, err := attach.ReplaceAttach(req)
	if err != nil {
		t.Fatalf("gapped idempotent retry: %v", err)
	}
	if rep2.VerifierTurnID != rep.VerifierTurnID {
		t.Fatalf("gapped retry did not reconcile: %q vs %q", rep2.VerifierTurnID, rep.VerifierTurnID)
	}
}

// A persisted activation whose run-state (verifier-issuance) step lands AMBIGUOUSLY leaves
// the replacement pending with no verifier issued; a same-op retry recovers it and issues the
// verifier exactly once (no re-mint). Drives the real replaceAttach authority via the thin
// crash-fault seam against a genuine coordinator-driven VERIFY.
func TestActivationRecoversPendingSameOp(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision); err != nil {
		t.Fatalf("tests pass: %v", err)
	}

	op, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint op: %v", err)
	}
	req := attach.ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: op, RNG: rand.Reader,
		Evidence: testIssuer(t, repo, runID),
	}
	if _, err := attach.ReplaceAttachWith(req, attach.ReplaceStores{StateMutate: ambiguousState}); !errors.Is(err, attach.ErrReplaceOutcomeUnknown) {
		t.Fatalf("ambiguous activation err = %v, want ErrReplaceOutcomeUnknown", err)
	}
	if v := cur(t, rn); v.Assignment != nil {
		t.Fatalf("ambiguous activation issued a verifier despite the unknown outcome: %+v", v.Assignment)
	}

	// A same-op retry (erroring RNG proves no re-mint) recovers the pending activation.
	req.RNG = errReader{}
	rep, err := attach.ReplaceAttach(req)
	if err != nil {
		t.Fatalf("recovery retry: %v", err)
	}
	if rep.VerifierTurnID == "" {
		t.Fatal("recovery did not issue the verifier turn")
	}
	after := cur(t, rn)
	if after.Phase != state.PhaseVerify || after.Assignment == nil || after.Assignment.ID != rep.VerifierTurnID {
		t.Fatalf("recovery did not durably issue the verifier assignment: %+v", after)
	}
}

// ambiguousState reports an ambiguous outcome WITHOUT applying the mutation — a crash BEFORE
// the participant effect (no verifier issued; classifyActivation NotApplied on retry).
func ambiguousState(*state.Store, *genstore.Guard, uint64, func(uint64, *state.RunState) error) (state.RunState, error) {
	return state.RunState{}, fmt.Errorf("%w: crash before the activation effect", genstore.ErrAmbiguous)
}

// applyThenAmbiguousState APPLIES the mutation durably and THEN reports an ambiguous outcome —
// a crash AFTER the participant effect but before progress is recorded (the verifier IS
// issued; classifyActivation StatusApplied on retry).
func applyThenAmbiguousState(s *state.Store, g *genstore.Guard, exp uint64, fn func(uint64, *state.RunState) error) (state.RunState, error) {
	rs, err := s.MutateLocked(g, exp, fn)
	if err != nil {
		return rs, err
	}
	return rs, fmt.Errorf("%w: crash after the activation effect", genstore.ErrAmbiguous)
}

// The genuine applied-but-unrecorded activation crash: the verifier-issuance step lands
// DURABLY, then the outcome is ambiguous (progress unrecorded), so the journal is pending. A
// same-op retry observes StatusApplied, confirms and finishes the journal, and does NOT
// re-append or re-issue the verifier.
func TestActivationAppliedButUnrecordedRetry(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	tests := driveToTests(t, rn, lead, pair)
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision); err != nil {
		t.Fatalf("tests pass: %v", err)
	}

	op, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint op: %v", err)
	}
	req := attach.ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: op, RNG: rand.Reader,
		Evidence: testIssuer(t, repo, runID),
	}
	if _, err := attach.ReplaceAttachWith(req, attach.ReplaceStores{StateMutate: applyThenAmbiguousState}); !errors.Is(err, attach.ErrReplaceOutcomeUnknown) {
		t.Fatalf("applied-but-unrecorded err = %v, want ErrReplaceOutcomeUnknown", err)
	}
	// The verifier assignment DID durably land (the effect applied before the crash).
	landed := cur(t, rn)
	if landed.Assignment == nil {
		t.Fatal("the applied activation effect is missing")
	}
	issued := landed.Assignment.ID

	// A same-op retry (erroring RNG proves no re-mint) observes the applied effect, finishes
	// the pending journal, and re-issues nothing.
	req.RNG = errReader{}
	rep, err := attach.ReplaceAttach(req)
	if err != nil {
		t.Fatalf("applied-but-unrecorded retry: %v", err)
	}
	if rep.VerifierTurnID != issued {
		t.Fatalf("retry re-issued a different verifier: %q vs %q", rep.VerifierTurnID, issued)
	}
	after := cur(t, rn)
	if after.Revision != landed.Revision {
		t.Fatalf("retry re-appended the run state: %d -> %d", landed.Revision, after.Revision)
	}
	if after.Assignment == nil || after.Assignment.ID != issued {
		t.Fatalf("retry changed the verifier assignment: %+v", after.Assignment)
	}
}

// activateRun drives a fresh paired run to ownerless VERIFY and completes a real pair
// activation, returning the open Run, the activation request, and the activated RunState.
func activateRun(t *testing.T, repo string) (*Run, attach.ReplaceRequest, state.RunState) {
	t.Helper()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	tests := driveToTests(t, rn, lead, pair)
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision); err != nil {
		t.Fatalf("tests pass: %v", err)
	}
	op, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint op: %v", err)
	}
	req := attach.ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: op, RNG: rand.Reader,
		Evidence: testIssuer(t, repo, runID),
	}
	if _, err := attach.ReplaceAttach(req); err != nil {
		t.Fatalf("activate: %v", err)
	}
	return rn, req, cur(t, rn)
}

// assertReplacementRecoveryRequired pins that BOTH a same-op retry and a fresh different-op
// step-over fail with ErrReplaceRecoveryRequired and mutate neither the Registry nor the
// RunState — the two production wiring branches the activation lineage gate protects.
func assertReplacementRecoveryRequired(t *testing.T, rn *Run, req attach.ReplaceRequest) {
	t.Helper()
	regBefore, _, _ := rn.registry.Load()
	stBefore := cur(t, rn)

	same := req
	same.RNG = errReader{} // a recovery-required retry never re-mints
	if _, err := attach.ReplaceAttach(same); !errors.Is(err, attach.ErrReplaceRecoveryRequired) {
		t.Fatalf("same-op retry err = %v, want ErrReplaceRecoveryRequired", err)
	}
	op2, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint op2: %v", err)
	}
	diff := req
	diff.OperationID = op2
	diff.ExpectedGeneration = 2 // the pair is at generation 2 after the first activation
	diff.RNG = rand.Reader
	if _, err := attach.ReplaceAttach(diff); !errors.Is(err, attach.ErrReplaceRecoveryRequired) {
		t.Fatalf("different-op step-over err = %v, want ErrReplaceRecoveryRequired", err)
	}
	if regAfter, _, _ := rn.registry.Load(); regAfter.Revision != regBefore.Revision {
		t.Fatalf("recovery-required replacement mutated the registry: %d -> %d", regBefore.Revision, regAfter.Revision)
	}
	if stAfter := cur(t, rn); stAfter.Revision != stBefore.Revision {
		t.Fatalf("recovery-required replacement mutated the run state: %d -> %d", stBefore.Revision, stAfter.Revision)
	}
}

// A completed activation whose RunState effect is later ROLLED BACK — a locally valid mutation
// that only clears the assignment leaves an ownerless VERIFY at a later revision while the
// terminal journal + Registry remain — is recovery-required for both the same-op retry and the
// different-op step-over (the lineage gate must not trust the journal/Registry alone).
func TestActivationRolledBackStateRequiresRecovery(t *testing.T) {
	rn, req, activated := activateRun(t, t.TempDir())
	defer rn.Close()

	// Clearing the assignment is locally valid (state owns no phase edges) and leaves an
	// ownerless VERIFY at a later revision — the activation effect is gone.
	if _, err := rn.state.Mutate(activated.Revision, func(_ uint64, next *state.RunState) error {
		next.Assignment = nil
		next.Evidence = nil // the binding is consumed with the turn it authorized
		next.Evidence = nil // the binding is consumed with the turn it authorized
		return nil
	}); err != nil {
		t.Fatalf("roll back the activation effect: %v", err)
	}
	assertReplacementRecoveryRequired(t, rn, req)
}

// A completed activation whose assignment is later left STALE — a no-op mutation advances the
// revision while preserving the assignment, so IssuedRevision < Revision, which is locally
// valid — is recovery-required for both a same-op retry and a different-op step-over (the
// lineage gate must not call a stale same-id assignment "still current").
func TestActivationStaleAssignmentRequiresRecovery(t *testing.T) {
	rn, req, activated := activateRun(t, t.TempDir())
	defer rn.Close()

	// A no-op mutation advances the revision while carrying the assignment unchanged.
	if _, err := rn.state.Mutate(activated.Revision, func(uint64, *state.RunState) error { return nil }); err != nil {
		t.Fatalf("staleify the assignment: %v", err)
	}
	staled := cur(t, rn)
	if staled.Assignment == nil || staled.Assignment.IssuedRevision >= staled.Revision {
		t.Fatalf("did not produce a stale assignment: %+v @ revision %d", staled.Assignment, staled.Revision)
	}
	assertReplacementRecoveryRequired(t, rn, req)
}

// verifyReadyRun drives a run to an ACTIVATED ownerless VERIFY (the verifier assignment
// issued to the new pair session) and returns the Run + the verifier session + the VERIFY
// RunState.
func verifyReadyRun(t *testing.T, repo string) (*Run, string, state.RunState) {
	t.Helper()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	tests := driveToTests(t, rn, lead, pair)
	if _, err := rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("e", 64), tests.Revision); err != nil {
		t.Fatalf("tests pass: %v", err)
	}
	op, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint op: %v", err)
	}
	rep, err := attach.ReplaceAttach(attach.ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: op, RNG: rand.Reader,
		Evidence: testIssuer(t, repo, runID),
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return rn, rep.SessionID, cur(t, rn)
}

// The full loop closes: after activation, the verifier's passing verification (projected
// against the frozen task snapshot) promotes the run to DONE.
func TestE2EVerifyToDone(t *testing.T) {
	rn, verifier, v := verifyReadyRun(t, t.TempDir())
	defer rn.Close()
	if v.Phase != state.PhaseVerify || v.Assignment == nil {
		t.Fatalf("not at an owned VERIFY: %+v", v)
	}
	submitOK(t, rn, verifier, verificationArtifact(t, v.Assignment.ID, v.Revision, "pass", true))
	if final := cur(t, rn); final.Phase != state.PhaseDone {
		t.Fatalf("passing verification did not reach DONE: %s", final.Phase)
	}
}

// A failing verification routes to the lead's FIX (returning to VERIFY) under the verify budget.
func TestE2EVerifyFailToFix(t *testing.T) {
	rn, verifier, v := verifyReadyRun(t, t.TempDir())
	defer rn.Close()
	submitOK(t, rn, verifier, verificationArtifact(t, v.Assignment.ID, v.Revision, "fail", false))
	if next := cur(t, rn); next.Phase != state.PhaseFix || next.FixReturn != state.PhaseVerify {
		t.Fatalf("failing verification did not route to a VERIFY-returning FIX: %+v", next)
	}
}

// A VERIFY submit whose frozen task snapshot is missing fails closed (ErrEvidence) — the
// verification cannot be projected without the durable task contract.
func TestE2EVerifyMissingTaskSnapshot(t *testing.T) {
	repo := t.TempDir()
	rn, verifier, v := verifyReadyRun(t, repo)
	defer rn.Close()
	loc, err := attach.ResolveRun(repo, v.RunID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := os.Remove(filepath.Join(loc.RunDir, v.TaskSnapshot.RelPath)); err != nil {
		t.Fatalf("remove task snapshot: %v", err)
	}
	if _, err := rn.Submit(context.Background(), verifier, verificationArtifact(t, v.Assignment.ID, v.Revision, "pass", true)); !errors.Is(err, ErrEvidence) {
		t.Fatalf("missing task snapshot err = %v, want ErrEvidence", err)
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
