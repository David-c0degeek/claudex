package gitx

import (
	"context"
	"errors"
	"math"
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

// snapshotWorktree provisions a real LINKED run worktree (branch claudex/r1 at the base commit)
// and returns the repo, handle, worktree path, base OID, and branch.
func snapshotWorktree(t *testing.T) (repo string, g *Git, wt, base, branch string) {
	repo, g = initRepo(t)
	base = oidOf(t, g, repo, "refs/heads/main")
	branch = "claudex/r1"
	spec := WorktreeSpec{RelPath: ".claudex/runs/r1/worktree", Branch: branch, BaseCommit: base, OwnerToken: "boot-token"}
	if err := NewWorktree(g).Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("provision run worktree: %v", err)
	}
	return repo, g, worktreeAbs(repo, spec), base, branch
}

func snapReq(repo, wt, base, branch string) SnapshotReq {
	return SnapshotReq{
		RepoDir: repo, Worktree: wt, Branch: branch, Parent: base,
		RunID: "run-" + strings.Repeat("a", 32), StartingRevision: 5, CreatedUnix: 1700000000,
	}
}

// A snapshot of the linked run worktree captures its content, commits off the parent, moves no ref,
// never touches the real index, and reproduces its OID deterministically.
func TestSnapshotCommitHappyPath(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	writeFile(t, filepath.Join(wt, "b.txt"), "new")

	commit, tree, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !isHex40or64(commit) || !isHex40or64(tree) {
		t.Fatalf("bad ids commit=%q tree=%q", commit, tree)
	}
	if p := oidOf(t, g, wt, commit+"^"); p != base {
		t.Fatalf("commit parent %q, want base %q", p, base)
	}
	names := string(mustRun(t, g, wt, nil, "ls-tree", "--name-only", tree))
	if !strings.Contains(names, "a.txt") || !strings.Contains(names, "b.txt") {
		t.Fatalf("tree contents = %q, want a.txt + b.txt", names)
	}
	if h := oidOf(t, g, wt, "HEAD"); h != base {
		t.Fatalf("HEAD moved to %q", h)
	}
	if st := strings.TrimSpace(string(mustRun(t, g, wt, nil, "diff", "--cached", "--name-only"))); st != "" {
		t.Fatalf("real index changed: %q", st)
	}
	commit2, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch))
	if err != nil || commit2 != commit {
		t.Fatalf("non-deterministic commit: %q vs %q (err %v)", commit2, commit, err)
	}
}

func TestSnapshotRejectsNoOp(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch)); !errors.Is(err, ErrEmptySnapshot) {
		t.Fatalf("clean worktree = %v, want ErrEmptySnapshot", err)
	}
}

// The main worktree is not a linked run worktree, even at the matching HEAD.
func TestSnapshotRejectsMainWorktree(t *testing.T) {
	repo, g := initRepo(t)
	base := oidOf(t, g, repo, "refs/heads/main")
	writeFile(t, filepath.Join(repo, "a.txt"), "edited")
	req := snapReq(repo, repo, base, "main") // repo itself = the main worktree
	if _, _, err := g.SnapshotCommit(context.Background(), req); !errors.Is(err, ErrSnapshotWorktree) {
		t.Fatalf("main worktree = %v, want ErrSnapshotWorktree", err)
	}
}

// A detached HEAD (no run branch) is rejected.
func TestSnapshotRejectsDetachedHead(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	mustRun(t, g, wt, nil, "checkout", "--detach")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch)); !errors.Is(err, ErrSnapshotWorktree) {
		t.Fatalf("detached HEAD = %v, want ErrSnapshotWorktree", err)
	}
}

// A worktree that belongs to a different repository is rejected even if its HEAD matches.
func TestSnapshotRejectsForeignRepo(t *testing.T) {
	repo, g, _, base, branch := snapshotWorktree(t)
	foreign, _ := initRepo(t) // a different repo, on main at its own base
	writeFile(t, filepath.Join(foreign, "a.txt"), "edited")
	req := snapReq(repo, foreign, base, branch) // RepoDir is repo, worktree is foreign
	if _, _, err := g.SnapshotCommit(context.Background(), req); !errors.Is(err, ErrSnapshotWorktree) {
		t.Fatalf("foreign repo = %v, want ErrSnapshotWorktree", err)
	}
}

// A staged edit that differs from the worktree would be discarded and fails closed.
func TestSnapshotStagedEditDivergence(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	writeFile(t, filepath.Join(wt, "a.txt"), "staged-version")
	mustRun(t, g, wt, commitEnv(), "add", "a.txt")
	writeFile(t, filepath.Join(wt, "a.txt"), "worktree-version-differs")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch)); !errors.Is(err, ErrStagedDivergence) {
		t.Fatalf("staged edit divergence = %v, want ErrStagedDivergence", err)
	}
}

// A staged DELETION with the worktree file retained is discarded by the snapshot and fails closed
// (the name-set intersection missed this; the exact entry comparison catches it).
func TestSnapshotStagedDeletionDivergence(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	mustRun(t, g, wt, commitEnv(), "rm", "--cached", "a.txt") // staged deletion, file retained
	writeFile(t, filepath.Join(wt, "b.txt"), "make it non-empty")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch)); !errors.Is(err, ErrStagedDivergence) {
		t.Fatalf("staged deletion = %v, want ErrStagedDivergence", err)
	}
}

// A racing edit that changes the tree between captures is rejected.
func TestSnapshotRacingEdit(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	orig := snapshotRaceHook
	defer func() { snapshotRaceHook = orig }()
	snapshotRaceHook = func() error {
		return os.WriteFile(filepath.Join(wt, "a.txt"), []byte("raced-in edit"), 0o600)
	}
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch)); !errors.Is(err, ErrRacingEdit) {
		t.Fatalf("racing edit = %v, want ErrRacingEdit", err)
	}
}

// The barrier catches a staged divergence interposed at the seam even when both trees match: the
// hook stages alternate bytes and restores the worktree bytes, so tree1 == tree2 but the real
// index now holds a version the snapshot discards.
func TestSnapshotBarrierCatchesStagedInjection(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	writeFile(t, filepath.Join(wt, "a.txt"), "worktree")
	orig := snapshotRaceHook
	defer func() { snapshotRaceHook = orig }()
	snapshotRaceHook = func() error {
		writeFile(t, filepath.Join(wt, "a.txt"), "alternate-staged")
		if _, err := g.Run(context.Background(), wt, commitEnv(), "add", "a.txt"); err != nil {
			return err
		}
		writeFile(t, filepath.Join(wt, "a.txt"), "worktree") // restore so both trees match
		return nil
	}
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch)); !errors.Is(err, ErrStagedDivergence) {
		t.Fatalf("barrier staged injection = %v, want ErrStagedDivergence", err)
	}
}

// A symlink is captured as a symlink entry (mode 120000), not dereferenced.
func TestSnapshotCapturesSymlink(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	if err := os.Symlink("a.txt", filepath.Join(wt, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, tree, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	entries := string(mustRun(t, g, wt, nil, "ls-tree", "-r", tree))
	if !strings.Contains(entries, "120000") || !strings.Contains(entries, "link") {
		t.Fatalf("tree %q did not capture the symlink as mode 120000", entries)
	}
}

// Object-writing commands run under the fsync contract so a frozen object cannot be lost.
func TestSnapshotObjectWritersFsynced(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	orig := runObserver
	defer func() { runObserver = orig }()
	var seen []string
	runObserver = func(argv []string) { seen = append(seen, strings.Join(argv, "\x00")) }
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch)); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, want := range []string{"add", "write-tree", "commit-tree"} {
		found := false
		for _, line := range seen {
			f := strings.Split(line, "\x00")
			if containsField(f, want) && containsField(f, "core.fsync=all") {
				found = true
			}
		}
		if !found {
			t.Errorf("%q was not run with core.fsync=all", want)
		}
	}
}

// The frozen ASCII/timestamp contract is enforced, not assumed.
func TestSnapshotInputValidation(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	base0 := snapReq(repo, wt, base, branch)
	mut := func(f func(*SnapshotReq)) SnapshotReq { r := base0; f(&r); return r }
	bad := []SnapshotReq{
		mut(func(r *SnapshotReq) { r.RunID = "run\ninjected" }),
		mut(func(r *SnapshotReq) { r.RunID = "" }),
		mut(func(r *SnapshotReq) { r.RunID = "not ascii é" }),
		mut(func(r *SnapshotReq) { r.StartingRevision = math.MaxUint64 }),
		mut(func(r *SnapshotReq) { r.CreatedUnix = -1 }),
	}
	for i, req := range bad {
		if _, _, err := g.SnapshotCommit(context.Background(), req); !errors.Is(err, ErrSnapshotInput) {
			t.Errorf("bad input %d = %v, want ErrSnapshotInput", i, err)
		}
	}
}

// An ambient repo commitEncoding does not leak an encoding header into the object.
func TestSnapshotForcesCommitEncoding(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktree(t)
	mustRun(t, g, wt, nil, "config", "i18n.commitEncoding", "ISO-8859-1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	commit, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	body := string(mustRun(t, g, wt, nil, "cat-file", "commit", commit))
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "encoding ") {
			t.Fatalf("commit carries an ambient encoding header: %q", line)
		}
	}
}

// snapshotWorktreeWithSubmodule provisions a linked run worktree whose base contains an
// initialized submodule at subPath (which may contain a space).
func snapshotWorktreeWithSubmodule(t *testing.T, subPath string) (repo string, g *Git, wt, base, branch string) {
	subSrc, _ := initRepo(t)
	repo, g = initRepo(t)
	allow := func(args ...string) []string { return append([]string{"-c", "protocol.file.allow=always"}, args...) }
	if _, err := g.Run(context.Background(), repo, commitEnv(), allow("submodule", "add", subSrc, subPath)...); err != nil {
		t.Skipf("submodule add unavailable: %v", err)
	}
	mustRun(t, g, repo, commitEnv(), "commit", "-m", "add sub")
	base = oidOf(t, g, repo, "refs/heads/main")
	branch = "claudex/r1"
	spec := WorktreeSpec{RelPath: ".claudex/runs/r1/worktree", Branch: branch, BaseCommit: base, OwnerToken: "tok"}
	if err := NewWorktree(g).Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("provision: %v", err)
	}
	wt = worktreeAbs(repo, spec)
	if _, err := g.Run(context.Background(), wt, commitEnv(), allow("submodule", "update", "--init")...); err != nil {
		t.Skipf("submodule init in linked worktree unavailable: %v", err)
	}
	return repo, g, wt, base, branch
}

// A submodule with uncommitted content fails closed; the path (with a space) is parsed NUL-safely.
func TestSnapshotRejectsDirtySubmoduleWithSpacePath(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktreeWithSubmodule(t, "sub dir")
	writeFile(t, filepath.Join(wt, "a.txt"), "ordinary edit") // make the snapshot non-empty
	writeFile(t, filepath.Join(wt, "sub dir", "dirty.txt"), "uncommitted submodule content")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch)); !errors.Is(err, ErrDirtySubmodule) {
		t.Fatalf("dirty submodule = %v, want ErrDirtySubmodule", err)
	}
}

// The barrier catches a submodule dirtied at the seam even though both superproject trees match
// (the gitlink HEAD is unchanged).
func TestSnapshotBarrierCatchesSubmoduleDirt(t *testing.T) {
	repo, g, wt, base, branch := snapshotWorktreeWithSubmodule(t, "sub")
	writeFile(t, filepath.Join(wt, "a.txt"), "ordinary edit")
	orig := snapshotRaceHook
	defer func() { snapshotRaceHook = orig }()
	snapshotRaceHook = func() error {
		return os.WriteFile(filepath.Join(wt, "sub", "untracked.txt"), []byte("dirtied at the seam"), 0o600)
	}
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, wt, base, branch)); !errors.Is(err, ErrDirtySubmodule) {
		t.Fatalf("barrier submodule dirt = %v, want ErrDirtySubmodule", err)
	}
}

func containsField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
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
