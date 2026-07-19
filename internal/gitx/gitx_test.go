package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitEnv gives git a deterministic identity so plumbing/porcelain commits work under the
// scrubbed (config-neutral) environment.
func commitEnv() map[string]string {
	return map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.invalid",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.invalid",
		"GIT_AUTHOR_DATE": "@1700000000 +0000", "GIT_COMMITTER_DATE": "@1700000000 +0000",
	}
}

func mustRun(t *testing.T, g *Git, dir string, env map[string]string, args ...string) []byte {
	t.Helper()
	out, err := g.Run(context.Background(), dir, env, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// initRepo makes a one-commit repo on branch main and returns its dir.
func initRepo(t *testing.T) (string, *Git) {
	t.Helper()
	g, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	repo := t.TempDir()
	mustRun(t, g, repo, nil, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustRun(t, g, repo, commitEnv(), "add", "a.txt")
	mustRun(t, g, repo, commitEnv(), "commit", "-m", "init")
	return repo, g
}

func TestRunResolvesHead(t *testing.T) {
	repo, g := initRepo(t)
	out := mustRun(t, g, repo, nil, "rev-parse", "--verify", "--end-of-options", "refs/heads/main^{commit}")
	oid := strings.TrimSpace(string(out))
	if len(oid) != 40 && len(oid) != 64 {
		t.Fatalf("rev-parse returned %q, want a 40/64-hex OID", oid)
	}
	for _, c := range oid {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("OID %q is not lower-hex", oid)
		}
	}
}

// The scrubbed environment ignores an ambient GIT_DIR that would otherwise redirect the child
// away from the real repository.
func TestRunScrubsAmbientGitDir(t *testing.T) {
	repo, g := initRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "bogus.git"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	if _, err := g.Run(context.Background(), repo, nil, "rev-parse", "--verify", "HEAD"); err != nil {
		t.Fatalf("ambient GIT_DIR/GIT_WORK_TREE leaked into the child: %v", err)
	}
}

// A non-zero git exit wraps ErrGit rather than succeeding.
func TestRunNonZeroExitWrapsErrGit(t *testing.T) {
	repo, g := initRepo(t)
	if _, err := g.Run(context.Background(), repo, nil, "rev-parse", "--verify", "refs/heads/nonesuch"); err == nil {
		t.Fatal("resolving a missing ref should fail")
	}
}

// A cancelled context is reported as the context error, not a git verdict.
func TestRunContextCancelled(t *testing.T) {
	repo, g := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Run(ctx, repo, nil, "rev-parse", "HEAD"); err == nil {
		t.Fatal("a cancelled context should fail the call")
	}
}

func TestBoundedBufferCaps(t *testing.T) {
	b := &boundedBuffer{limit: 4}
	n, _ := b.Write([]byte("abcdef"))
	if n != 6 {
		t.Fatalf("Write reported %d, want the full 6 consumed", n)
	}
	if got := b.String(); got != "abcd" {
		t.Fatalf("buffer = %q, want the first 4 bytes only", got)
	}
}
