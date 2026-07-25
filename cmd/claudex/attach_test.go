package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/attach"
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
		// Cross-mode: a flag outside the mode's exact allowed set is a usage error, not a
		// silently-ignored different mode.
		{"join + --task", []string{"attach", "--run", "run-" + strings.Repeat("a", 32), "--agent", "codex", "--role", "pair", "--operation-id", op, "--task", "ignored.json"}},
		{"first + --session-id", []string{"attach", "--agent", "claude", "--task", "t", "--config", "c", "--operation-id", op, "--session-id", "sess-" + strings.Repeat("b", 32)}},
		{"first + --expected-generation", []string{"attach", "--agent", "claude", "--task", "t", "--config", "c", "--operation-id", op, "--expected-generation", "2"}},
		{"reattach + --operation-id", []string{"attach", "--reattach", "--run", "run-" + strings.Repeat("a", 32), "--session-id", "sess-" + strings.Repeat("b", 32), "--agent", "codex", "--role", "pair", "--operation-id", op}},
		{"replace + --config", []string{"attach", "--replace", "--run", "run-" + strings.Repeat("a", 32), "--agent", "codex", "--role", "pair", "--expected-generation", "2", "--operation-id", op, "--config", "x"}},
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
	writeF(t, p, `{"schema_version":2,"goal":"drive the pairing loop","current_behavior":"none","desired_behavior":"two terminals converge","scope":"cli e2e","non_goals":[],"constraints":[],"acceptance_criteria":["it works"],"required_tests":[],"relevant_files":[],"relevant_repo_paths":["README"],"open_questions":[]}`)
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
	firstOp := mintOp(t)

	doFirst := func() struct {
		Mode        string   `json:"mode"`
		RunID       string   `json:"run_id"`
		SessionID   string   `json:"session_id"`
		Role        string   `json:"role"`
		Agent       string   `json:"agent"`
		JoinCommand []string `json:"join_command"`
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(),
			[]string{"attach", "--repo", repo, "--agent", "claude", "--task", task, "--config", cfg, "--operation-id", firstOp},
			&out, &errb); code != 0 {
			t.Fatalf("first attach exit = %d; stderr=%q", code, errb.String())
		}
		var r struct {
			Mode        string   `json:"mode"`
			RunID       string   `json:"run_id"`
			SessionID   string   `json:"session_id"`
			Role        string   `json:"role"`
			Agent       string   `json:"agent"`
			JoinCommand []string `json:"join_command"`
		}
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("decode first result %q: %v", out.String(), err)
		}
		return r
	}

	first := doFirst()
	if first.Mode != "first" || !state.IsRunID(first.RunID) || !state.IsSessionID(first.SessionID) || first.Role != "lead" || first.Agent != "claude" {
		t.Fatalf("first result wrong: %+v", first)
	}
	// The join command must be self-contained: run + complement agent + pair role + a frozen op id,
	// and an absolute repo locator (never `--repo .`).
	jc := strings.Join(first.JoinCommand, " ")
	if first.JoinCommand[0] != "attach" || !strings.Contains(jc, "--run "+first.RunID) ||
		!strings.Contains(jc, "--agent codex") || !strings.Contains(jc, "--role pair") ||
		!strings.Contains(jc, "--operation-id op-") {
		t.Fatalf("join_command missing required parts: %v", first.JoinCommand)
	}
	if i := indexOf(first.JoinCommand, "--repo"); i < 0 || i+1 >= len(first.JoinCommand) || !filepath.IsAbs(first.JoinCommand[i+1]) {
		t.Fatalf("join_command --repo is not an absolute locator: %v", first.JoinCommand)
	}

	// A same-first-op replay BEFORE the join returns the same run/session and a BYTE-IDENTICAL
	// join command (the pair join op id is frozen in the intent, not re-minted per call).
	replayPre := doFirst()
	if replayPre.RunID != first.RunID || replayPre.SessionID != first.SessionID {
		t.Fatalf("first-op replay changed identity: %+v vs %+v", replayPre, first)
	}
	if !slices.Equal(replayPre.JoinCommand, first.JoinCommand) {
		t.Fatalf("first-op replay join command drifted:\n %v\n %v", first.JoinCommand, replayPre.JoinCommand)
	}

	// Run the emitted join command VERBATIM (unchanged argv).
	var out, errb bytes.Buffer
	if code := run(context.Background(), first.JoinCommand, &out, &errb); code != 0 {
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

	// A same-first-op replay AFTER pair completion STILL advertises the byte-identical join
	// command (so a lost-response join stays recoverable).
	replayPost := doFirst()
	if !slices.Equal(replayPost.JoinCommand, first.JoinCommand) {
		t.Fatalf("post-join first-op replay join command drifted:\n %v\n %v", first.JoinCommand, replayPost.JoinCommand)
	}

	// An idempotent join replay (same frozen op id) returns the same pair session.
	out.Reset()
	errb.Reset()
	if code := run(context.Background(), first.JoinCommand, &out, &errb); code != 0 {
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

// TestReplaceResultClassification pins the replace exit/output contract without a real
// replace: an unknown outcome emits its candidate and exits 1 (recovery-required); a clean
// outcome exits 0; a proven-committed CommitWarning is a success-with-warning (exit 0).
func TestReplaceResultClassification(t *testing.T) {
	base := attach.ReplaceResult{RunID: "run-x", SessionID: "sess-x", Role: state.SlotPair, Agent: state.AgentCodex, Generation: 2}

	out, warns, exit := classifyReplaceResult(base, attach.ErrReplaceOutcomeUnknown)
	if exit != 1 || out["outcome_unknown"] != true || len(warns) != 1 {
		t.Fatalf("unknown outcome: exit=%d out=%v warns=%v, want exit 1 + outcome_unknown + 1 warning", exit, out, warns)
	}

	out, warns, exit = classifyReplaceResult(base, nil)
	if exit != 0 || out["outcome_unknown"] != nil || len(warns) != 0 {
		t.Fatalf("clean outcome: exit=%d out=%v warns=%v, want exit 0 + no warning", exit, out, warns)
	}

	withWarn := base
	withWarn.CommitWarning = errors.New("guard release failed")
	out, warns, exit = classifyReplaceResult(withWarn, nil)
	if exit != 0 || out["commit_warning"] != "guard release failed" || len(warns) != 1 {
		t.Fatalf("commit-warning outcome: exit=%d out=%v warns=%v, want exit 0 + commit_warning + 1 warning", exit, out, warns)
	}
}

// TestAttachFirstResetsMailbox proves first attach resets the repo-level mailbox to the new
// run's (empty) projection, so a prior run's stale transcript is never left visible.
func TestAttachFirstResetsMailbox(t *testing.T) {
	repo := gitRepo(t)
	inputs := t.TempDir()
	task, cfg := taskFile(t, inputs), configFile(t, inputs)

	// A stale transcript from a prior run sits at the repo-level mailbox (.claudex is git-ignored).
	claudex := filepath.Join(repo, ".claudex")
	if err := os.MkdirAll(claudex, 0o700); err != nil {
		t.Fatalf("mkdir .claudex: %v", err)
	}
	stale := "STALE PRIOR RUN TRANSCRIPT\n"
	if err := os.WriteFile(filepath.Join(claudex, "mailbox.md"), []byte(stale), 0o600); err != nil {
		t.Fatalf("write stale mailbox: %v", err)
	}

	var out, errb bytes.Buffer
	if code := run(context.Background(),
		[]string{"attach", "--repo", repo, "--agent", "claude", "--task", task, "--config", cfg, "--operation-id", mintOp(t)},
		&out, &errb); code != 0 {
		t.Fatalf("first attach: exit %d, %s", code, errb.String())
	}
	md, err := os.ReadFile(filepath.Join(claudex, "mailbox.md"))
	if err != nil {
		t.Fatalf("read mailbox after attach: %v", err)
	}
	if string(md) == stale {
		t.Fatal("first attach did not reset the prior run's stale mailbox transcript")
	}
}

func indexOf(s []string, v string) int {
	for i := range s {
		if s[i] == v {
			return i
		}
	}
	return -1
}
