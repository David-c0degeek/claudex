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

func TestParseGitVersion(t *testing.T) {
	ok := []struct {
		in       string
		maj, min int
	}{
		{"git version 2.54.0.windows.1", 2, 54},
		{"git version 2.36.0", 2, 36},
		{"git version 2.35.1", 2, 35},
		{"git version 3.0.0", 3, 0},
	}
	for _, c := range ok {
		maj, min, err := parseGitVersion(c.in)
		if err != nil || maj != c.maj || min != c.min {
			t.Errorf("parseGitVersion(%q) = %d.%d err=%v, want %d.%d", c.in, maj, min, err, c.maj, c.min)
		}
	}
	bad := []string{"", "git 2.54", "not a version", "git version x.y", "git version 2"}
	for _, in := range bad {
		if _, _, err := parseGitVersion(in); err == nil {
			t.Errorf("parseGitVersion(%q) = nil error, want a parse error", in)
		}
	}
}

func TestBelowMinVersion(t *testing.T) {
	below := [][2]int{{2, 31}, {2, 35}, {1, 99}, {0, 0}}
	for _, v := range below {
		if !belowMinVersion(v[0], v[1]) {
			t.Errorf("belowMinVersion(%d,%d) = false, want true (min %d.%d)", v[0], v[1], minGitMajor, minGitMinor)
		}
	}
	okv := [][2]int{{2, 36}, {2, 54}, {3, 0}}
	for _, v := range okv {
		if belowMinVersion(v[0], v[1]) {
			t.Errorf("belowMinVersion(%d,%d) = true, want false", v[0], v[1])
		}
	}
}

// The installed git satisfies the minimum-version preflight (a supported real output).
func TestCheckMinVersionSupported(t *testing.T) {
	repo, g := repoWithIgnore(t, ".claudex/\n")
	if err := g.checkMinVersion(context.Background(), repo); err != nil {
		t.Fatalf("installed git rejected by min-version check: %v", err)
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
