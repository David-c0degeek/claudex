package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
)

// --- usage-error dispatch (no git needed) ---

func TestAttachUsageErrors(t *testing.T) {
	op := "op-" + strings.Repeat("a", 32)
	cases := []struct {
		name string
		args []string
	}{
		{"no agent (first)", []string{"attach", "--repo", ".", "--task", "t", "--config", "c", "--operation-id", op}},
		{"bad agent", []string{"attach", "--repo", ".", "--agent", "gemini", "--task", "t", "--config", "c", "--operation-id", op}},
		{"first without operation-id", []string{"attach", "--repo", ".", "--agent", "claude", "--task", "t", "--config", "c"}},
		{"first with bad operation-id", []string{"attach", "--repo", ".", "--agent", "claude", "--task", "t", "--config", "c", "--operation-id", "nope"}},
		{"join without role pair", []string{"attach", "--run", "run-" + strings.Repeat("a", 32), "--agent", "codex", "--role", "lead", "--operation-id", op}},
		{"join without operation-id", []string{"attach", "--run", "run-" + strings.Repeat("a", 32), "--agent", "codex", "--role", "pair"}},
		{"reattach without session-id", []string{"attach", "--reattach", "--run", "run-" + strings.Repeat("a", 32), "--agent", "codex", "--role", "pair"}},
		{"replace without expected-generation", []string{"attach", "--replace", "--run", "run-" + strings.Repeat("a", 32), "--agent", "codex", "--role", "pair", "--operation-id", op}},
		{"reattach and replace together", []string{"attach", "--reattach", "--replace", "--run", "run-" + strings.Repeat("a", 32), "--agent", "codex", "--role", "pair"}},
		{"positional arg", []string{"attach", "extra"}},
		{"unknown flag", []string{"attach", "--nope"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run(context.Background(), tc.args, &out, &errb); code != 2 {
				t.Fatalf("exit = %d, want 2 (usage); stderr=%q", code, errb.String())
			}
			if out.Len() != 0 {
				t.Fatalf("usage error wrote to stdout: %q", out.String())
			}
		})
	}
}

// --- real-repo first -> join e2e ---

func gitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
	mustGitCLI(t, repo, env, "init", "-b", "main")
	writeF(t, filepath.Join(repo, ".gitignore"), ".claudex/\n")
	writeF(t, filepath.Join(repo, "README"), "hi\n")
	mustGitCLI(t, repo, env, "add", "-A")
	mustGitCLI(t, repo, env, "commit", "-m", "init")
	return repo
}

func mustGitCLI(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeF(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func taskFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "task.json")
	writeF(t, p, `{"schema_version":1,"goal":"drive the pairing loop","current_behavior":"none","desired_behavior":"two terminals converge","scope":"cli e2e","non_goals":[],"constraints":[],"acceptance_criteria":["it works"],"required_tests":[],"relevant_files":[],"open_questions":[]}`)
	return p
}

func configFile(t *testing.T, dir string) string {
	t.Helper()
	pol := config.DefaultRunPolicy()
	pol.TestGate = config.TestGate{Disabled: true}
	b, err := json.Marshal(pol)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	p := filepath.Join(dir, "config.json")
	writeF(t, p, string(b))
	return p
}

func mintOp(t *testing.T) string {
	t.Helper()
	op, err := state.MintOperationID(rand.Reader)
	if err != nil {
		t.Fatalf("mint op: %v", err)
	}
	return op
}

// TestAttachFirstThenJoin drives a real first attach then a pair join using the emitted
// join_command verbatim, asserting the run bootstraps and the pair fills its slot.
func TestAttachFirstThenJoin(t *testing.T) {
	// The e2e writes into a temp dir but the CLI runs relative to --repo; keep the process
	// cwd stable by passing absolute --repo (the emitted join_command uses --repo ".").
	repo := gitRepo(t)
	inputs := t.TempDir() // task/config live OUTSIDE the repo so the tree stays clean for preflight
	task, cfg := taskFile(t, inputs), configFile(t, inputs)

	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"attach", "--repo", repo, "--agent", "claude", "--task", task, "--config", cfg, "--operation-id", mintOp(t)},
		&out, &errb)
	if code != 0 {
		t.Fatalf("first attach exit = %d; stderr=%q", code, errb.String())
	}
	var first struct {
		Mode        string   `json:"mode"`
		RunID       string   `json:"run_id"`
		SessionID   string   `json:"session_id"`
		Role        string   `json:"role"`
		Agent       string   `json:"agent"`
		JoinCommand []string `json:"join_command"`
	}
	if err := json.Unmarshal(out.Bytes(), &first); err != nil {
		t.Fatalf("decode first result %q: %v", out.String(), err)
	}
	if first.Mode != "first" || !state.IsRunID(first.RunID) || !state.IsSessionID(first.SessionID) || first.Role != "lead" || first.Agent != "claude" {
		t.Fatalf("first result wrong: %+v", first)
	}
	// The join command must name the run, the complement agent+pair role, and carry its own op id.
	jc := strings.Join(first.JoinCommand, " ")
	if first.JoinCommand[0] != "attach" || !strings.Contains(jc, "--run "+first.RunID) ||
		!strings.Contains(jc, "--agent codex") || !strings.Contains(jc, "--role pair") ||
		!strings.Contains(jc, "--operation-id op-") {
		t.Fatalf("join_command missing required parts: %v", first.JoinCommand)
	}

	// Run the emitted join command verbatim (drop the leading "attach"), but point --repo at the
	// absolute repo so the test's cwd is irrelevant.
	joinArgs := append([]string(nil), first.JoinCommand...)
	for i := range joinArgs {
		if joinArgs[i] == "." && i > 0 && joinArgs[i-1] == "--repo" {
			joinArgs[i] = repo
		}
	}
	out.Reset()
	errb.Reset()
	if code := run(context.Background(), joinArgs, &out, &errb); code != 0 {
		t.Fatalf("join exit = %d; stderr=%q", code, errb.String())
	}
	var join struct {
		Mode        string `json:"mode"`
		RunID       string `json:"run_id"`
		SessionID   string `json:"session_id"`
		Role        string `json:"role"`
		Agent       string `json:"agent"`
		FirstTurnID string `json:"first_turn_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &join); err != nil {
		t.Fatalf("decode join result %q: %v", out.String(), err)
	}
	if join.Mode != "join" || join.RunID != first.RunID || !state.IsSessionID(join.SessionID) ||
		join.Role != "pair" || join.Agent != "codex" || join.FirstTurnID == "" {
		t.Fatalf("join result wrong: %+v", join)
	}
	if join.SessionID == first.SessionID {
		t.Fatalf("pair session id equals the lead's")
	}

	// An idempotent join replay (same op id) returns the same session.
	out.Reset()
	errb.Reset()
	if code := run(context.Background(), joinArgs, &out, &errb); code != 0 {
		t.Fatalf("join replay exit = %d; stderr=%q", code, errb.String())
	}
	var join2 struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(out.Bytes(), &join2)
	if join2.SessionID != join.SessionID {
		t.Fatalf("idempotent join replay minted a new session: %q -> %q", join.SessionID, join2.SessionID)
	}
}
