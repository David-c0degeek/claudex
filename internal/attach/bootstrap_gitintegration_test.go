package attach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// realGitRepo builds a one-commit repo on branch main with .claudex git-ignored, and returns the
// repo dir, a hardened git handle (closed on cleanup), and the base commit OID.
func realGitRepo(t *testing.T) (string, *gitx.Git, string) {
	t.Helper()
	g, err := gitx.New()
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	repo := t.TempDir()
	env := map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
	}
	mustGit(t, g, repo, nil, "init", "-b", "main")
	writeFile(t, filepath.Join(repo, ".gitignore"), ".claudex/\n")
	writeFile(t, filepath.Join(repo, "README"), "hi\n")
	mustGit(t, g, repo, env, "add", "-A")
	mustGit(t, g, repo, env, "commit", "-m", "init")
	base := strings.TrimSpace(string(mustGit(t, g, repo, nil, "rev-parse", "--verify", "refs/heads/main")))
	return repo, g, base
}

func mustGit(t *testing.T, g *gitx.Git, dir string, env map[string]string, args ...string) []byte {
	t.Helper()
	out, err := g.Run(context.Background(), dir, env, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// realRequest builds a FirstAttach request wired to the real git seams over g.
func realRequest(t *testing.T, repo string, g *gitx.Git, op string, rng io.Reader) FirstAttachRequest {
	t.Helper()
	base, pre, wt := NewGitSeams(g)
	return FirstAttachRequest{
		RepoDir:         repo,
		Agent:           state.AgentClaude,
		OperationID:     op,
		TaskCanonical:   taskBytes(),
		PolicyCanonical: policyBytes(),
		CreatedUnix:     1000,
		RNG:             rng,
		Base:            base,
		Preflight:       pre,
		Worktree:        wt,
		Classifier:      supportedFS(),
	}
}

// assertProvisioned proves the observable deliverable: the run is active and its persisted
// workspace identity resolves to an applied worktree (branch at base, clean checkout).
func assertProvisioned(t *testing.T, g *gitx.Git, repo, runID string) {
	t.Helper()
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(runID))
	rs, ok, err := state.Open(filepath.Join(runDir, "state"), lay.repoLock).Load()
	if err != nil || !ok {
		t.Fatalf("load run state: ok=%v err=%v", ok, err)
	}
	spec := gitx.WorktreeSpec{RelPath: rs.WorktreeRelPath, Branch: rs.RunBranch, BaseCommit: rs.BaseCommit}
	st, err := gitx.NewWorktree(g).Observe(context.Background(), repo, spec)
	if err != nil || st != gitx.WorktreeApplied {
		t.Fatalf("worktree Observe = %v (err %v), want applied", st, err)
	}
	cur, ok, err := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if err != nil || !ok || !cur.Active || cur.RunID != runID {
		t.Fatalf("current run = %+v ok=%v err=%v, want active %s", cur, ok, err, runID)
	}
}

// End to end against native git: a first attach provisions the exact derived local branch and a
// clean registered worktree at BaseCommit.
func TestFirstAttachRealGitEndToEnd(t *testing.T) {
	repo, g, base := realGitRepo(t)
	res, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), rand.Reader))
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	// The derived branch is exactly claudex/<runID> at the base commit.
	got := strings.TrimSpace(string(mustGit(t, g, repo, nil, "for-each-ref", "--format=%(objectname)", "refs/heads/"+wantRunBranch(res.RunID))))
	if got != base {
		t.Fatalf("run branch at %q, want base %q", got, base)
	}
	assertProvisioned(t, g, repo, res.RunID)
}

// A same-operation retry (a lost response after completion) returns the same run, still applied.
func TestFirstAttachRealGitSameOpRecovery(t *testing.T) {
	repo, g, _ := realGitRepo(t)
	first, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), rand.Reader))
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	again, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), rand.Reader))
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if again.RunID != first.RunID || again.SessionID != first.SessionID {
		t.Fatalf("retry identity %s/%s != %s/%s", again.RunID, again.SessionID, first.RunID, first.SessionID)
	}
	assertProvisioned(t, g, repo, first.RunID)
}

// A crash after the real worktree provisioning applied but before the journal recorded its
// progress recovers forward on retry to a fully bootstrapped run.
func TestFirstAttachRealGitCutRecovers(t *testing.T) {
	repo, g, _ := realGitRepo(t)
	fired := false
	stepFailpoint = func(s string) error {
		if s == "worktree" && !fired {
			fired = true
			return errors.New("injected crash after worktree provisioning")
		}
		return nil
	}
	defer func() { stepFailpoint = nil }()
	if _, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), rand.Reader)); err == nil {
		t.Fatal("expected the injected worktree cut to fail the attach")
	}
	stepFailpoint = nil
	res, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), rand.Reader))
	if err != nil {
		t.Fatalf("recovery after the worktree cut: %v", err)
	}
	assertProvisioned(t, g, repo, res.RunID)
}

// A cancellation mid-provisioning (the branch ref created, the worktree not registered, the git
// command reporting the context error) leaves a recoverable pending bootstrap: a later mutator
// observes the ref-only prefix and completes it forward.
func TestFirstAttachRealGitCancellationRecoverable(t *testing.T) {
	repo, g, _ := realGitRepo(t)
	base, pre, _ := NewGitSeams(g)
	req := FirstAttachRequest{
		RepoDir: repo, Agent: state.AgentClaude, OperationID: opID("a"),
		TaskCanonical: taskBytes(), PolicyCanonical: policyBytes(), CreatedUnix: 1000,
		RNG: rand.Reader, Base: base, Preflight: pre,
		Worktree:   &cancelOnceProvisioner{real: NewGitWorktreeProvisioner(g), g: g},
		Classifier: supportedFS(),
	}
	if _, err := FirstAttach(context.Background(), req); err == nil {
		t.Fatal("expected the mid-provision cancellation to fail the attach")
	}
	// Retry with the real provisioner: it recovers the pending bootstrap forward.
	res, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), rand.Reader))
	if err != nil {
		t.Fatalf("recovery after cancellation: %v", err)
	}
	assertProvisioned(t, g, repo, res.RunID)
}

// cancelOnceProvisioner provisions the branch ref only and then reports a cancelled context on
// its FIRST Apply, simulating a cancellation partway through provisioning; later Applies delegate
// to the real provisioner, which observes the ref-only prefix and completes it forward.
type cancelOnceProvisioner struct {
	real WorktreeProvisioner
	g    *gitx.Git
	done bool
}

func (p *cancelOnceProvisioner) ObserveWorktree(ctx context.Context, repoDir string, in BootstrapIntent) (txn.StepStatus, error) {
	return p.real.ObserveWorktree(ctx, repoDir, in)
}

func (p *cancelOnceProvisioner) ApplyWorktree(ctx context.Context, repoDir string, in BootstrapIntent) error {
	if !p.done {
		p.done = true
		if _, err := p.g.Run(ctx, repoDir, nil, "update-ref", "refs/heads/"+in.RunBranch, in.BaseCommit, ""); err != nil {
			return err
		}
		return context.Canceled
	}
	return p.real.ApplyWorktree(ctx, repoDir, in)
}

func (p *cancelOnceProvisioner) ConfirmWorktree(ctx context.Context, repoDir string, in BootstrapIntent) error {
	return p.real.ConfirmWorktree(ctx, repoDir, in)
}

// A foreign preexisting run branch at a different commit fails the bootstrap closed: the worktree
// step is indeterminate, so no run is activated and the foreign ref is never overwritten.
func TestFirstAttachRealGitForeignRefFailsClosed(t *testing.T) {
	repo, g, base := realGitRepo(t)
	// A deterministic RNG fixes the run id (its first 16 bytes), so the foreign branch can be
	// planted at exactly the derived name before the attach runs.
	seed := make([]byte, 512)
	for i := range seed {
		seed[i] = byte(i)
	}
	runID := "run-" + hex.EncodeToString(seed[:16])
	// A distinct off-branch commit (a child of base with the same tree), planted at the derived
	// run branch WITHOUT moving main — so ResolveBase(main) is still base and the run branch is
	// genuinely foreign (a different commit than the frozen base).
	idEnv := map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
		"GIT_AUTHOR_DATE": "@1700000001 +0000", "GIT_COMMITTER_DATE": "@1700000001 +0000",
	}
	baseTree := strings.TrimSpace(string(mustGit(t, g, repo, nil, "rev-parse", "main^{tree}")))
	c2 := strings.TrimSpace(string(mustGit(t, g, repo, idEnv, "commit-tree", baseTree, "-p", base, "-m", "foreign")))
	if c2 == base {
		t.Fatal("expected a distinct off-branch commit")
	}
	mustGit(t, g, repo, nil, "update-ref", "refs/heads/"+wantRunBranch(runID), c2, "")

	req := realRequest(t, repo, g, opID("a"), newSeedReader(seed))
	if _, err := FirstAttach(context.Background(), req); err == nil {
		t.Fatal("a foreign run branch should fail the bootstrap closed")
	}
	// No run was activated and the foreign ref is untouched.
	lay := layoutFor(repo)
	if cur, ok, _ := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load(); ok && cur.Active {
		t.Fatalf("a run was activated despite the foreign ref: %+v", cur)
	}
	if got := strings.TrimSpace(string(mustGit(t, g, repo, nil, "for-each-ref", "--format=%(objectname)", "refs/heads/"+wantRunBranch(runID)))); got != c2 {
		t.Fatalf("foreign ref moved to %q, want %q untouched", got, c2)
	}
}

// deterministicSeed returns a fixed RNG seed and the run id its first 16 bytes mint, so a foreign
// object can be planted at the derived worktree path before the attach runs.
func deterministicSeed(t *testing.T) ([]byte, string) {
	t.Helper()
	seed := make([]byte, 512)
	for i := range seed {
		seed[i] = byte(i)
	}
	return seed, "run-" + hex.EncodeToString(seed[:16])
}

// A preexisting (ignored) directory with a foreign marker at the derived worktree path is not
// proven ours: the bootstrap fails closed, activates no run, and never deletes the marker.
func TestFirstAttachRealGitForeignWorktreeDirFailsClosed(t *testing.T) {
	repo, g, _ := realGitRepo(t)
	seed, runID := deterministicSeed(t)
	marker := filepath.Join(repo, ".claudex", "runs", runID, "worktree", "foreign-marker")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatalf("plant dir: %v", err)
	}
	writeFile(t, marker, "not ours\n")

	if _, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), newSeedReader(seed))); err == nil {
		t.Fatal("a foreign worktree directory should fail the bootstrap closed")
	}
	assertNoActiveRun(t, repo)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("foreign marker was disturbed: %v", err)
	}
}

// A regular file at the derived worktree path is foreign: fail closed, file untouched.
func TestFirstAttachRealGitForeignFileFailsClosed(t *testing.T) {
	repo, g, _ := realGitRepo(t)
	seed, runID := deterministicSeed(t)
	abs := filepath.Join(repo, ".claudex", "runs", runID, "worktree")
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, abs, "i am a file\n")

	if _, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), newSeedReader(seed))); err == nil {
		t.Fatal("a regular file at the worktree path should fail the bootstrap closed")
	}
	assertNoActiveRun(t, repo)
	if b, err := os.ReadFile(abs); err != nil || string(b) != "i am a file\n" {
		t.Fatalf("foreign file disturbed: %q err=%v", b, err)
	}
}

// The derived run branch already checked out at a different (ignored) path is foreign: fail closed.
func TestFirstAttachRealGitBranchElsewhereFailsClosed(t *testing.T) {
	repo, g, base := realGitRepo(t)
	seed, runID := deterministicSeed(t)
	branch := "claudex/" + runID
	mustGit(t, g, repo, nil, "update-ref", "refs/heads/"+branch, base, "")
	other := filepath.Join(repo, ".claudex", "elsewhere-wt")
	mustGit(t, g, repo, nil, "worktree", "add", other, branch)

	if _, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), newSeedReader(seed))); err == nil {
		t.Fatal("the run branch checked out elsewhere should fail the bootstrap closed")
	}
	assertNoActiveRun(t, repo)
}

func assertNoActiveRun(t *testing.T, repo string) {
	t.Helper()
	lay := layoutFor(repo)
	if cur, ok, _ := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load(); ok && cur.Active {
		t.Fatalf("a run was activated despite a foreign worktree object: %+v", cur)
	}
}

// A dirty working tree is refused by preflight before any run directory is created.
func TestFirstAttachRealGitDirtyPreflightNoRun(t *testing.T) {
	repo, g, _ := realGitRepo(t)
	writeFile(t, filepath.Join(repo, "untracked.txt"), "dirt\n")
	if _, err := FirstAttach(context.Background(), realRequest(t, repo, g, opID("a"), rand.Reader)); err == nil {
		t.Fatal("a dirty repo should be refused by preflight")
	}
	if _, e := os.Stat(filepath.Join(repo, ".claudex")); !os.IsNotExist(e) {
		t.Fatalf(".claudex created despite a preflight refusal")
	}
}

// newSeedReader returns a reader over a fixed seed that then repeats it, so id minting never runs
// dry even if a mint retries.
func newSeedReader(seed []byte) io.Reader { return &repeatReader{seed: seed} }

type repeatReader struct {
	seed []byte
	pos  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if len(r.seed) == 0 {
		return 0, io.EOF
	}
	for i := range p {
		p[i] = r.seed[r.pos%len(r.seed)]
		r.pos++
	}
	return len(p), nil
}
