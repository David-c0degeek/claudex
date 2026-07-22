package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// bootstrapPair drives a real first attach + pair join over a fresh git repo and returns the
// run id and the lead session id (the run is then at PLAN_DRAFT with the lead's turn issued).
func bootstrapPair(t *testing.T) (repo, runID, leadSession string) {
	t.Helper()
	repo = gitRepo(t)
	inputs := t.TempDir()
	task, cfg := taskFile(t, inputs), configFile(t, inputs)

	var out, errb bytes.Buffer
	if code := run(context.Background(),
		[]string{"attach", "--repo", repo, "--agent", "claude", "--task", task, "--config", cfg, "--operation-id", mintOp(t)},
		&out, &errb); code != 0 {
		t.Fatalf("first attach: exit %d, %s", code, errb.String())
	}
	var first struct {
		RunID       string   `json:"run_id"`
		SessionID   string   `json:"session_id"`
		JoinCommand []string `json:"join_command"`
	}
	if err := json.Unmarshal(out.Bytes(), &first); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	out.Reset()
	errb.Reset()
	if code := run(context.Background(), first.JoinCommand, &out, &errb); code != 0 {
		t.Fatalf("join: exit %d, %s", code, errb.String())
	}
	return repo, first.RunID, first.SessionID
}

// currentTurn loads the run state and returns the outstanding assignment turn id + revision.
func currentTurn(t *testing.T, repo, runID string) (turnID string, rev uint64) {
	t.Helper()
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve run: %v", err)
	}
	rs, ok, err := state.Open(loc.StateDir, loc.RunLock).Load()
	if err != nil || !ok {
		t.Fatalf("load state: ok=%v err=%v", ok, err)
	}
	if rs.Assignment == nil {
		t.Fatalf("run has no outstanding assignment at %s", rs.Phase)
	}
	return rs.Assignment.ID, rs.Revision
}

func planArtifactFile(t *testing.T, dir, turnID string, rev uint64) string {
	t.Helper()
	body := fmt.Sprintf(`{"protocol_version":1,"message_type":"plan","turn_id":%q,"state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"plan_markdown":"architecture prose","steps":[{"title":"step one","description":"do step one","files":["a.go"],"tests":["a_test.go"]}],"risks":["a risk"],"open_questions":[]}`, turnID, rev)
	p := filepath.Join(dir, "plan.json")
	writeF(t, p, body)
	return p
}

// TestSubmitE2E drives a real plan submit through the CLI, asserting a schema-valid receipt on
// stdout, the mailbox mirror rebuilt from the durable ledger, an idempotent replay, and a
// crash-missed mirror repaired by that replay.
func TestSubmitE2E(t *testing.T) {
	repo, runID, lead := bootstrapPair(t)
	turnID, rev := currentTurn(t, repo, runID)
	planFile := planArtifactFile(t, t.TempDir(), turnID, rev)

	var out, errb bytes.Buffer
	if code := run(context.Background(),
		[]string{"submit", "--repo", repo, "--run", runID, "--session-id", lead, "--file", planFile},
		&out, &errb); code != 0 {
		t.Fatalf("submit: exit %d, %s", code, errb.String())
	}
	// The receipt is schema-valid and acknowledges the turn.
	if _, err := protocol.Validate("receipt", bytes.TrimSpace(out.Bytes())); err != nil {
		t.Fatalf("receipt not schema-valid: %v (%q)", err, out.String())
	}
	var rc struct {
		MessageType   string `json:"message_type"`
		TurnID        string `json:"turn_id"`
		StateRevision uint64 `json:"state_revision"`
	}
	if err := json.Unmarshal(out.Bytes(), &rc); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if rc.MessageType != "receipt" || rc.TurnID != turnID || rc.StateRevision != rev+1 {
		t.Fatalf("receipt wrong: %+v (turn %s rev %d)", rc, turnID, rev)
	}

	// The mailbox mirror was rebuilt and contains the accepted plan turn.
	mailbox := filepath.Join(repo, ".claudex", "mailbox.md")
	md, err := os.ReadFile(mailbox)
	if err != nil {
		t.Fatalf("mailbox not written: %v", err)
	}
	if len(md) == 0 {
		t.Fatal("mailbox mirror is empty after a submit")
	}

	// Crash-missed mirror: delete it, then an idempotent replay must repair it.
	if err := os.Remove(mailbox); err != nil {
		t.Fatalf("remove mailbox: %v", err)
	}
	out.Reset()
	errb.Reset()
	if code := run(context.Background(),
		[]string{"submit", "--repo", repo, "--run", runID, "--session-id", lead, "--file", planFile},
		&out, &errb); code != 0 {
		t.Fatalf("submit replay: exit %d, %s", code, errb.String())
	}
	var rc2 struct {
		TurnID        string `json:"turn_id"`
		StateRevision uint64 `json:"state_revision"`
	}
	if err := json.Unmarshal(out.Bytes(), &rc2); err != nil {
		t.Fatalf("decode replay receipt: %v", err)
	}
	if rc2.TurnID != rc.TurnID || rc2.StateRevision != rc.StateRevision {
		t.Fatalf("idempotent replay returned a different receipt: %+v vs %+v", rc2, rc)
	}
	if _, err := os.ReadFile(mailbox); err != nil {
		t.Fatalf("idempotent replay did not repair the crash-missed mailbox: %v", err)
	}
}

// TestSubmitUsageErrors covers the submit command's usage-error dispatch (no run needed).
func TestSubmitUsageErrors(t *testing.T) {
	cases := [][]string{
		{"submit", "--session-id", "sess-" + repeat("a", 32)},                               // no --file
		{"submit", "--file", "x.json"},                                                      // no session-id
		{"submit", "--session-id", "not-a-session", "--file", "x.json"},                     // bad session-id
		{"submit", "--session-id", "sess-" + repeat("a", 32), "--file", "x.json", "extra"},  // positional
		{"submit", "--session-id", "sess-" + repeat("a", 32), "--file", "x.json", "--nope"}, // unknown flag
	}
	for i, args := range cases {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Fatalf("case %d: exit %d, want 2; stderr=%q", i, code, errb.String())
		}
	}
}

// TestClassifySubmit pins the receipt/exit contract for every Run.Submit result+error shape:
// an authoritative receipt accompanying a durability/reconfirm error is emitted (never dropped)
// and exits 1; a clean success mirrors and exits 0; a zero-receipt error is a receipt-less
// failure; a ReleaseWarning is a non-fatal warning on a committed acceptance.
func TestClassifySubmit(t *testing.T) {
	rc := state.Receipt{TurnID: "turn-1", Revision: 5, ArtifactDigest: repeat("a", 64)}
	durErr := errors.New("durability unconfirmed")

	// Clean success: emit, mirror, exit 0.
	if emit, warn, mirror, exit := classifySubmit(transport.SubmitResult{Receipt: rc}, nil); !emit || warn != nil || !mirror || exit != 0 {
		t.Fatalf("clean: emit=%v warn=%v mirror=%v exit=%d", emit, warn, mirror, exit)
	}
	// Idempotent replay whose state re-confirm failed: emit the receipt, warn, DO NOT mirror, exit 1.
	if emit, warn, mirror, exit := classifySubmit(transport.SubmitResult{Receipt: rc, Idempotent: true}, durErr); !emit || warn != durErr || mirror || exit != 1 {
		t.Fatalf("replay+durErr: emit=%v warn=%v mirror=%v exit=%d", emit, warn, mirror, exit)
	}
	// Visible-but-unconfirmed acceptance: same shape (receipt + error) -> emit + exit 1, no mirror.
	if emit, _, mirror, exit := classifySubmit(transport.SubmitResult{Receipt: rc}, durErr); !emit || mirror || exit != 1 {
		t.Fatalf("accept+durErr: emit=%v mirror=%v exit=%d", emit, mirror, exit)
	}
	// Zero receipt + error: an ordinary receipt-less failure.
	if emit, warn, _, exit := classifySubmit(transport.SubmitResult{}, durErr); emit || warn != durErr || exit != 1 {
		t.Fatalf("receiptless: emit=%v warn=%v exit=%d", emit, warn, exit)
	}
	// A committed acceptance with a release warning: success-with-warning (exit 0), still mirrors.
	relErr := errors.New("guard release failed")
	if emit, warn, mirror, exit := classifySubmit(transport.SubmitResult{Receipt: rc, ReleaseWarning: relErr}, nil); !emit || warn != relErr || !mirror || exit != 0 {
		t.Fatalf("releaseWarning: emit=%v warn=%v mirror=%v exit=%d", emit, warn, mirror, exit)
	}
}

// TestResolveRunIDExitCodes pins the run-resolution classification: only a malformed CLI id is a
// usage error (2); a canonical-but-unbound id and no-active-run are operational (1).
func TestResolveRunIDExitCodes(t *testing.T) {
	repo := gitRepo(t) // a real repo with NO active run
	var errb bytes.Buffer

	// Malformed explicit id (a space is outside the id grammar): usage (2).
	if _, code := resolveRunID(repo, "bad id", &errb); code != 2 {
		t.Fatalf("malformed --run exit = %d, want 2", code)
	}
	// Grammar-valid but unbound (no such active run): operational (1), not usage.
	if _, code := resolveRunID(repo, "run-"+repeat("a", 32), &errb); code != 1 {
		t.Fatalf("valid-but-unbound --run exit = %d, want 1", code)
	}
	// No --run and no active run in the repo: operational (1).
	if _, code := resolveRunID(repo, "", &errb); code != 1 {
		t.Fatalf("no active run exit = %d, want 1", code)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s[0])
	}
	return string(out)
}
