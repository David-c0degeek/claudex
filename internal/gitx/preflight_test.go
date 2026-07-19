package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// repoWithIgnore builds a one-commit repo whose .gitignore holds the given body (empty for none).
func repoWithIgnore(t *testing.T, ignore string) (string, *Git) {
	t.Helper()
	g, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	repo := t.TempDir()
	mustRun(t, g, repo, nil, "init", "-b", "main")
	if ignore != "" {
		if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(ignore), 0o600); err != nil {
			t.Fatalf("write .gitignore: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	mustRun(t, g, repo, commitEnv(), "add", "-A")
	mustRun(t, g, repo, commitEnv(), "commit", "-m", "init")
	return repo, g
}

func TestPreflightCleanOK(t *testing.T) {
	repo, g := repoWithIgnore(t, ".claudex/\n")
	if err := g.Preflight(context.Background(), repo, ".claudex"); err != nil {
		t.Fatalf("preflight on a clean repo: %v", err)
	}
}

// An existing but ignored runtime directory does not make the tree dirty, so preflight still
// passes (a bootstrap that already created .claudex must not fail its own precondition).
func TestPreflightIgnoredRuntimePresentOK(t *testing.T) {
	repo, g := repoWithIgnore(t, ".claudex/\n")
	if err := os.MkdirAll(filepath.Join(repo, ".claudex", "runs"), 0o700); err != nil {
		t.Fatalf("mkdir .claudex: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claudex", "x"), []byte("y"), 0o600); err != nil {
		t.Fatalf("write under .claudex: %v", err)
	}
	if err := g.Preflight(context.Background(), repo, ".claudex"); err != nil {
		t.Fatalf("preflight with an ignored .claudex present: %v", err)
	}
}

// Running from a subdirectory, not the repository root, fails closed.
func TestPreflightNotRepoRoot(t *testing.T) {
	repo, g := repoWithIgnore(t, ".claudex/\n")
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := g.Preflight(context.Background(), sub, ".claudex"); !errors.Is(err, ErrPreflight) {
		t.Fatalf("preflight from a subdir = %v, want ErrPreflight", err)
	}
}

// A repository that does not ignore the runtime dir is refused.
func TestPreflightRuntimeNotIgnored(t *testing.T) {
	repo, g := repoWithIgnore(t, "")
	if err := g.Preflight(context.Background(), repo, ".claudex"); !errors.Is(err, ErrPreflight) {
		t.Fatalf("preflight without a .claudex ignore = %v, want ErrPreflight", err)
	}
}

// A tracked path under the runtime dir (a committed .claudex) defeats the ignore and is refused.
func TestPreflightTrackedUnderRuntime(t *testing.T) {
	repo, g := repoWithIgnore(t, ".claudex/\n")
	if err := os.MkdirAll(filepath.Join(repo, ".claudex"), 0o700); err != nil {
		t.Fatalf("mkdir .claudex: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claudex", "tracked"), []byte("z"), 0o600); err != nil {
		t.Fatalf("write tracked: %v", err)
	}
	mustRun(t, g, repo, commitEnv(), "add", "-f", ".claudex/tracked")
	mustRun(t, g, repo, commitEnv(), "commit", "-m", "track claudex")
	if err := g.Preflight(context.Background(), repo, ".claudex"); !errors.Is(err, ErrPreflight) {
		t.Fatalf("preflight with a tracked .claudex path = %v, want ErrPreflight", err)
	}
}

// A dirty working tree (a non-ignored untracked file) is refused.
func TestPreflightDirtyTree(t *testing.T) {
	repo, g := repoWithIgnore(t, ".claudex/\n")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("dirt"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}
	if err := g.Preflight(context.Background(), repo, ".claudex"); !errors.Is(err, ErrPreflight) {
		t.Fatalf("preflight on a dirty tree = %v, want ErrPreflight", err)
	}
}
