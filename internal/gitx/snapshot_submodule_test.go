package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// snapshotRunWithSubmodule provisions a linked run worktree whose base contains a submodule at
// subPath (which may contain a space). When initSub is true the submodule is populated in the run
// worktree; otherwise its mount point is left as git's empty directory.
func snapshotRunWithSubmodule(t *testing.T, runID, subPath string, initSub bool) (repo string, g *Git, wt, base string) {
	subSrc, _ := initRepo(t)
	repo, g = initRepo(t)
	allow := func(args ...string) []string { return append([]string{"-c", "protocol.file.allow=always"}, args...) }
	if _, err := g.Run(context.Background(), repo, commitEnv(), allow("submodule", "add", subSrc, subPath)...); err != nil {
		t.Skipf("submodule add unavailable: %v", err)
	}
	mustRun(t, g, repo, commitEnv(), "commit", "-m", "add sub")
	base = oidOf(t, g, repo, "refs/heads/main")
	spec := WorktreeSpec{RelPath: runWorktreeRel(runID), Branch: runBranch(runID), BaseCommit: base, OwnerToken: "tok"}
	if err := NewWorktree(g).Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("provision: %v", err)
	}
	wt = runWorktreeAbs(repo, runID)
	if initSub {
		if _, err := g.Run(context.Background(), wt, commitEnv(), allow("submodule", "update", "--init")...); err != nil {
			t.Skipf("submodule init in linked worktree unavailable: %v", err)
		}
	}
	return repo, g, wt, base
}

// An uninitialized submodule (an empty mount directory) is clean: the gitlink is captured as-is.
func TestSnapshotUninitializedSubmoduleClean(t *testing.T) {
	repo, g, wt, base := snapshotRunWithSubmodule(t, "r1", "sub", false)
	writeFile(t, filepath.Join(wt, "a.txt"), "ordinary edit")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); err != nil {
		t.Fatalf("uninitialized submodule should be clean: %v", err)
	}
}

// A gitlink path populated with foreign, non-repository content fails closed (a git error there is
// NOT collapsed into "uninitialized clean").
func TestSnapshotPopulatedNonRepoSubmoduleRejected(t *testing.T) {
	repo, g, wt, base := snapshotRunWithSubmodule(t, "r1", "sub", false)
	writeFile(t, filepath.Join(wt, "sub", "foreign.txt"), "not a submodule checkout")
	writeFile(t, filepath.Join(wt, "a.txt"), "ordinary edit")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrDirtySubmodule) {
		t.Fatalf("populated non-repo gitlink = %v, want ErrDirtySubmodule", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "sub", "foreign.txt")); err != nil {
		t.Fatalf("foreign submodule content disturbed: %v", err)
	}
}

// A submodule with uncommitted content fails closed; a path with a space is parsed NUL-safely.
func TestSnapshotRejectsDirtySubmoduleWithSpacePath(t *testing.T) {
	repo, g, wt, base := snapshotRunWithSubmodule(t, "r1", "sub dir", true)
	writeFile(t, filepath.Join(wt, "a.txt"), "ordinary edit")
	writeFile(t, filepath.Join(wt, "sub dir", "dirty.txt"), "uncommitted submodule content")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrDirtySubmodule) {
		t.Fatalf("dirty submodule = %v, want ErrDirtySubmodule", err)
	}
}

// The barrier catches a submodule dirtied at the seam even though both superproject trees match.
func TestSnapshotBarrierCatchesSubmoduleDirt(t *testing.T) {
	repo, g, wt, base := snapshotRunWithSubmodule(t, "r1", "sub", true)
	writeFile(t, filepath.Join(wt, "a.txt"), "ordinary edit")
	orig := snapshotRaceHook
	defer func() { snapshotRaceHook = orig }()
	snapshotRaceHook = func() error {
		return os.WriteFile(filepath.Join(wt, "sub", "untracked.txt"), []byte("dirtied at the seam"), 0o600)
	}
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrDirtySubmodule) {
		t.Fatalf("barrier submodule dirt = %v, want ErrDirtySubmodule", err)
	}
}
