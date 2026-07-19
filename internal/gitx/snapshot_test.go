package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func snapReq(repo, base string) SnapshotReq {
	return SnapshotReq{
		Worktree: repo, Parent: base,
		RunID: "run-" + strings.Repeat("a", 32), StartingRevision: 5, CreatedUnix: 1700000000,
	}
}

// A snapshot captures the exact worktree, creates a commit off the parent, moves no ref and never
// touches the real index, and is deterministic (reproducible OID from the frozen facts).
func TestSnapshotCommitHappyPath(t *testing.T) {
	repo, g := initRepo(t) // a.txt committed on main
	base := oidOf(t, g, repo, "HEAD")
	writeFile(t, filepath.Join(repo, "a.txt"), "edited")
	writeFile(t, filepath.Join(repo, "b.txt"), "new")

	commit, tree, err := g.SnapshotCommit(context.Background(), snapReq(repo, base))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !isHex40or64(commit) || !isHex40or64(tree) {
		t.Fatalf("bad ids commit=%q tree=%q", commit, tree)
	}
	// The commit's parent is the base and its tree is the snapshot tree.
	if p := oidOf(t, g, repo, commit+"^"); p != base {
		t.Fatalf("commit parent %q, want base %q", p, base)
	}
	if ct := oidOf(t, g, repo, commit+"^{tree}"); ct != tree {
		t.Fatalf("commit tree %q != returned tree %q", ct, tree)
	}
	// The tree captured both the edit and the new file.
	names := string(mustRun(t, g, repo, nil, "ls-tree", "--name-only", tree))
	if !strings.Contains(names, "a.txt") || !strings.Contains(names, "b.txt") {
		t.Fatalf("tree contents = %q, want a.txt + b.txt", names)
	}
	// No ref moved and the real index/HEAD are untouched.
	if h := oidOf(t, g, repo, "HEAD"); h != base {
		t.Fatalf("HEAD moved to %q", h)
	}
	if st := strings.TrimSpace(string(mustRun(t, g, repo, nil, "diff", "--cached", "--name-only"))); st != "" {
		t.Fatalf("real index changed: %q", st)
	}
	// Deterministic: a second snapshot of the unchanged worktree yields the same commit.
	commit2, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base))
	if err != nil || commit2 != commit {
		t.Fatalf("non-deterministic commit: %q vs %q (err %v)", commit2, commit, err)
	}
}

// A clean worktree (tree equals the parent) is rejected rather than making an empty commit.
func TestSnapshotRejectsNoOp(t *testing.T) {
	repo, g := initRepo(t)
	base := oidOf(t, g, repo, "HEAD")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base)); !errors.Is(err, ErrEmptySnapshot) {
		t.Fatalf("snapshot of a clean worktree = %v, want ErrEmptySnapshot", err)
	}
}

// A parent that is not the worktree HEAD fails closed.
func TestSnapshotWrongParent(t *testing.T) {
	repo, g := initRepo(t)
	writeFile(t, filepath.Join(repo, "a.txt"), "edited")
	req := snapReq(repo, strings.Repeat("0", 40)) // not HEAD
	if _, _, err := g.SnapshotCommit(context.Background(), req); !errors.Is(err, ErrSnapshotWorktree) {
		t.Fatalf("wrong parent = %v, want ErrSnapshotWorktree", err)
	}
}

// A staged change that differs from the worktree would be silently discarded by the worktree
// snapshot, so it fails closed.
func TestSnapshotStagedOnlyDivergence(t *testing.T) {
	repo, g := initRepo(t)
	base := oidOf(t, g, repo, "HEAD")
	writeFile(t, filepath.Join(repo, "a.txt"), "staged-version")
	mustRun(t, g, repo, commitEnv(), "add", "a.txt") // index has the staged version
	writeFile(t, filepath.Join(repo, "a.txt"), "worktree-version-differs")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base)); !errors.Is(err, ErrStagedDivergence) {
		t.Fatalf("staged-only divergence = %v, want ErrStagedDivergence", err)
	}
}

// A racing edit between the two independent captures is detected and rejected.
func TestSnapshotRacingEdit(t *testing.T) {
	repo, g := initRepo(t)
	base := oidOf(t, g, repo, "HEAD")
	writeFile(t, filepath.Join(repo, "a.txt"), "edited")
	orig := snapshotRaceHook
	defer func() { snapshotRaceHook = orig }()
	snapshotRaceHook = func() error {
		return os.WriteFile(filepath.Join(repo, "a.txt"), []byte("raced-in edit"), 0o600)
	}
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base)); !errors.Is(err, ErrRacingEdit) {
		t.Fatalf("racing edit = %v, want ErrRacingEdit", err)
	}
}

// A symlink in the worktree is captured as a symlink entry (mode 120000), not dereferenced.
func TestSnapshotCapturesSymlink(t *testing.T) {
	repo, g := initRepo(t)
	base := oidOf(t, g, repo, "HEAD")
	if err := os.Symlink("a.txt", filepath.Join(repo, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, tree, err := g.SnapshotCommit(context.Background(), snapReq(repo, base))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	entries := string(mustRun(t, g, repo, nil, "ls-tree", "-r", tree))
	if !strings.Contains(entries, "120000") || !strings.Contains(entries, "link") {
		t.Fatalf("tree %q did not capture the symlink as mode 120000", entries)
	}
}

// A submodule with uncommitted content fails closed (only a clean gitlink is representable).
func TestSnapshotRejectsDirtySubmodule(t *testing.T) {
	// A standalone repo to use as the submodule source.
	subSrc, sg := initRepo(t)
	_ = sg
	repo, g := initRepo(t)
	allow := []string{"-c", "protocol.file.allow=always"}
	if _, err := g.Run(context.Background(), repo, commitEnv(), append(allow, "submodule", "add", subSrc, "sub")...); err != nil {
		t.Skipf("submodule add unavailable: %v", err)
	}
	mustRun(t, g, repo, commitEnv(), "commit", "-m", "add sub")
	base := oidOf(t, g, repo, "HEAD")
	// Dirty the submodule worktree.
	writeFile(t, filepath.Join(repo, "sub", "dirty.txt"), "uncommitted")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base)); !errors.Is(err, ErrDirtySubmodule) {
		t.Fatalf("dirty submodule = %v, want ErrDirtySubmodule", err)
	}
}

func isHex40or64(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
