package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// provisionRepo returns a one-commit repo, its git handle, a worktree spec anchored to the
// initial commit, and that base OID.
func provisionRepo(t *testing.T) (string, *Git, WorktreeSpec, string) {
	t.Helper()
	repo, g := initRepo(t)
	base := oidOf(t, g, repo, "refs/heads/main")
	spec := WorktreeSpec{RelPath: ".claudex/runs/r1/worktree", Branch: "claudex/r1", BaseCommit: base, OwnerToken: "boot-token-r1"}
	return repo, g, spec, base
}

// assertApplied checks the observable deliverable: the run branch at BaseCommit and a clean
// worktree registered at the path with HEAD == BaseCommit on that branch.
func assertApplied(t *testing.T, g *Git, repo string, spec WorktreeSpec) {
	t.Helper()
	w := NewWorktree(g)
	if st, err := w.Observe(context.Background(), repo, spec); err != nil || st != WorktreeApplied {
		t.Fatalf("Observe = %v (err %v), want applied", st, err)
	}
	if oid, _ := w.refOID(context.Background(), repo, spec.Branch); oid != spec.BaseCommit {
		t.Fatalf("run branch at %q, want base %q", oid, spec.BaseCommit)
	}
	abs := worktreeAbs(repo, spec)
	regs, err := w.listWorktrees(context.Background(), repo)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	reg := findRegByPath(regs, abs)
	if reg == nil || !reg.onBranch(spec.Branch) || reg.head != spec.BaseCommit || reg.prunable {
		t.Fatalf("registration = %+v, want on %s at %s clean", reg, spec.Branch, spec.BaseCommit)
	}
	if !isDir(abs) {
		t.Fatalf("worktree dir %s missing", abs)
	}
}

func TestWorktreeProvisionFromAbsent(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	w := NewWorktree(g)
	if st, err := w.Observe(context.Background(), repo, spec); err != nil || st != WorktreeAbsent {
		t.Fatalf("initial Observe = %v (err %v), want absent", st, err)
	}
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertApplied(t, g, repo, spec)
	if err := w.Confirm(context.Background(), repo, spec); err != nil {
		t.Fatalf("confirm: %v", err)
	}
}

func TestWorktreeApplyIdempotent(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	w := NewWorktree(g)
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("apply 2 (idempotent): %v", err)
	}
	assertApplied(t, g, repo, spec)
}

// A crash after the branch ref was created but before the worktree registered: Observe is
// OwnPartial and Apply completes it forward.
func TestWorktreeRecoverRefOnly(t *testing.T) {
	repo, g, spec, base := provisionRepo(t)
	mustRun(t, g, repo, nil, "update-ref", "refs/heads/"+spec.Branch, base, "")
	w := NewWorktree(g)
	if st, err := w.Observe(context.Background(), repo, spec); err != nil || st != WorktreeOwnPartial {
		t.Fatalf("Observe = %v (err %v), want own-partial", st, err)
	}
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("recover apply: %v", err)
	}
	assertApplied(t, g, repo, spec)
}

// A crash that left the registration but lost the worktree directory: Observe is OwnPartial and
// Apply prunes the stale entry and re-adds a clean checkout.
func TestWorktreeRecoverMissingDir(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	w := NewWorktree(g)
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	abs := worktreeAbs(repo, spec)
	if err := os.RemoveAll(abs); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	if st, err := w.Observe(context.Background(), repo, spec); err != nil || st != WorktreeOwnPartial {
		t.Fatalf("Observe after dir loss = %v (err %v), want own-partial", st, err)
	}
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("recover apply: %v", err)
	}
	assertApplied(t, g, repo, spec)
}

// The precise internal cut of `git worktree add`: it creates the target directory before writing
// the linkage files, so a halt there leaves our ref + our ownership marker + a bare, unregistered
// target directory. That is our own prefix (the marker proves it), recovered forward — not foreign.
func TestWorktreeRecoverInternalAddCut(t *testing.T) {
	repo, g, spec, base := provisionRepo(t)
	abs := worktreeAbs(repo, spec)
	// Reconstruct the cut state.
	mustRun(t, g, repo, nil, "update-ref", "refs/heads/"+spec.Branch, base, "")
	if err := ensureOwnerMarker(abs, spec.OwnerToken); err != nil {
		t.Fatalf("plant owner marker: %v", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil { // git's leading-directory creation, no linkage yet
		t.Fatalf("plant bare target: %v", err)
	}
	w := NewWorktree(g)
	if st, err := w.Observe(context.Background(), repo, spec); err != nil || st != WorktreeOwnPartial {
		t.Fatalf("Observe = %v (err %v), want own-partial", st, err)
	}
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("recover apply: %v", err)
	}
	assertApplied(t, g, repo, spec)
}

// A bare target directory WITHOUT our marker (a different token, or none) is foreign even though a
// bare target is the same shape our own interrupted add leaves — ownership is proven, not shaped.
func TestWorktreeBareDirWrongTokenForeign(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	abs := worktreeAbs(repo, spec)
	if err := ensureOwnerMarker(abs, "some-other-run-token"); err != nil {
		t.Fatalf("plant foreign marker: %v", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		t.Fatalf("plant bare target: %v", err)
	}
	if st, err := NewWorktree(g).Observe(context.Background(), repo, spec); err != nil || st != WorktreeForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
	if err := NewWorktree(g).Apply(context.Background(), repo, spec); err == nil {
		t.Fatal("apply over a foreign-marked bare directory should fail closed")
	}
}

// Confirm forces the checked-out FILE CONTENTS durable, not only the linkage — a tracked checkout
// file is among the content barriers.
func TestWorktreeConfirmBarriersCheckoutContent(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	w := NewWorktree(g)
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	origFile := confirmFileBarrier
	defer func() { confirmFileBarrier = origFile }()
	var files []string
	confirmFileBarrier = func(p string) error { files = append(files, p); return origFile(p) }
	if err := w.Confirm(context.Background(), repo, spec); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// initRepo committed a.txt, so the base checkout contains it; its content must be barriered.
	if !containsPath(files, filepath.Join(worktreeAbs(repo, spec), "a.txt")) {
		t.Fatalf("checkout content barriers %v did not include the checked-out a.txt", files)
	}
}

// A durability failure on a CHECKOUT payload file fails Confirm — checkout content, not just
// linkage, is authority-bearing.
func TestWorktreeConfirmCheckoutBarrierFailure(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	w := NewWorktree(g)
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	origFile := confirmFileBarrier
	defer func() { confirmFileBarrier = origFile }()
	confirmFileBarrier = func(p string) error {
		if strings.HasSuffix(p, "a.txt") {
			return errors.New("injected checkout durability failure")
		}
		return origFile(p)
	}
	if err := w.Confirm(context.Background(), repo, spec); err == nil {
		t.Fatal("a checkout payload barrier failure should fail Confirm")
	}
}

// A branch of our name at a DIFFERENT commit is foreign: never overwritten, Apply fails closed.
func TestWorktreeForeignRef(t *testing.T) {
	repo, g, spec, base := provisionRepo(t)
	c2 := commitFile(t, g, repo, "b.txt", "second")
	if c2 == base {
		t.Fatal("expected a distinct second commit")
	}
	mustRun(t, g, repo, nil, "update-ref", "refs/heads/"+spec.Branch, c2, "")
	w := NewWorktree(g)
	if st, err := w.Observe(context.Background(), repo, spec); err != nil || st != WorktreeForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
	if err := w.Apply(context.Background(), repo, spec); err == nil {
		t.Fatal("apply over a foreign ref should fail closed")
	}
	// The foreign ref is untouched.
	if oid, _ := w.refOID(context.Background(), repo, spec.Branch); oid != c2 {
		t.Fatalf("foreign ref moved to %q, want %q untouched", oid, c2)
	}
}

// A worktree registered at OUR path but on a DIFFERENT branch is foreign: fail closed.
func TestWorktreeForeignRegistration(t *testing.T) {
	repo, g, spec, base := provisionRepo(t)
	abs := worktreeAbs(repo, spec)
	mustRun(t, g, repo, nil, "branch", "other", base)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	mustRun(t, g, repo, nil, "worktree", "add", abs, "other")
	w := NewWorktree(g)
	if st, err := w.Observe(context.Background(), repo, spec); err != nil || st != WorktreeForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
	if err := w.Apply(context.Background(), repo, spec); err == nil {
		t.Fatal("apply over a foreign registration should fail closed")
	}
}

// Confirm proves durable-applied identity, so a merely ref-only prefix is rejected.
func TestWorktreeConfirmRejectsPartial(t *testing.T) {
	repo, g, spec, base := provisionRepo(t)
	mustRun(t, g, repo, nil, "update-ref", "refs/heads/"+spec.Branch, base, "")
	if err := NewWorktree(g).Confirm(context.Background(), repo, spec); err == nil {
		t.Fatal("confirm on an unprovisioned worktree should fail")
	}
}

func TestWorktreeApplyContextCancelled(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewWorktree(g).Apply(ctx, repo, spec); err == nil {
		t.Fatal("a cancelled context should fail apply")
	}
}

// A bare directory at the target path with no worktree registration is NOT proven ours: it is
// foreign, and Apply must fail closed and never delete it.
func TestWorktreeForeignBareDirectory(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	abs := worktreeAbs(repo, spec)
	marker := filepath.Join(abs, "foreign-marker")
	if err := os.MkdirAll(abs, 0o700); err != nil {
		t.Fatalf("mkdir abs: %v", err)
	}
	if err := os.WriteFile(marker, []byte("not ours"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	w := NewWorktree(g)
	if st, err := w.Observe(context.Background(), repo, spec); err != nil || st != WorktreeForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
	if err := w.Apply(context.Background(), repo, spec); err == nil {
		t.Fatal("apply over an unregistered directory should fail closed")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("foreign marker was disturbed: %v", err)
	}
}

// A regular file at the target path is foreign (git only ever makes it a directory).
func TestWorktreeForeignRegularFile(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	abs := worktreeAbs(repo, spec)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(abs, []byte("i am a file"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	w := NewWorktree(g)
	if st, err := w.Observe(context.Background(), repo, spec); err != nil || st != WorktreeForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
	if err := w.Apply(context.Background(), repo, spec); err == nil {
		t.Fatal("apply over a regular file should fail closed")
	}
	if b, err := os.ReadFile(abs); err != nil || string(b) != "i am a file" {
		t.Fatalf("foreign file disturbed: %q err=%v", b, err)
	}
}

// A symlink at the target path is foreign (Lstat does not follow it).
func TestWorktreeForeignSymlink(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	abs := worktreeAbs(repo, spec)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.Symlink(t.TempDir(), abs); err != nil {
		t.Skipf("symlink creation unavailable: %v", err) // e.g. unprivileged Windows
	}
	if st, err := NewWorktree(g).Observe(context.Background(), repo, spec); err != nil || st != WorktreeForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
}

// Our run branch checked out at a DIFFERENT path is foreign/mismatched: never adopted.
func TestWorktreeForeignBranchElsewhere(t *testing.T) {
	repo, g, spec, base := provisionRepo(t)
	mustRun(t, g, repo, nil, "update-ref", "refs/heads/"+spec.Branch, base, "")
	other := filepath.Join(repo, ".claudex", "runs", "elsewhere")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustRun(t, g, repo, nil, "worktree", "add", other, spec.Branch)
	if st, err := NewWorktree(g).Observe(context.Background(), repo, spec); err != nil || st != WorktreeForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
	if err := NewWorktree(g).Apply(context.Background(), repo, spec); err == nil {
		t.Fatal("apply when our branch is checked out elsewhere should fail closed")
	}
}

// A durability-barrier failure fails Confirm (so the transaction never records progress over an
// unconfirmed worktree); a healthy retry confirms.
func TestWorktreeConfirmBarrierFailureRetries(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	w := NewWorktree(g)
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	orig := confirmDirBarrier
	defer func() { confirmDirBarrier = orig }()
	failed := false
	confirmDirBarrier = func(p string) error {
		if !failed {
			failed = true
			return errors.New("injected barrier failure")
		}
		return orig(p)
	}
	if err := w.Confirm(context.Background(), repo, spec); err == nil {
		t.Fatal("a barrier failure should fail Confirm")
	}
	if err := w.Confirm(context.Background(), repo, spec); err != nil {
		t.Fatalf("healthy retry Confirm: %v", err)
	}
}

// Confirm reaches the real barrier primitives over the structural identity (the worktree dir, the
// admin dir, and the loose ref), not a no-op.
func TestWorktreeConfirmReachesRealBarriers(t *testing.T) {
	repo, g, spec, _ := provisionRepo(t)
	w := NewWorktree(g)
	if err := w.Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	origDir, origFile := confirmDirBarrier, confirmFileBarrier
	defer func() { confirmDirBarrier, confirmFileBarrier = origDir, origFile }()
	var dirs, files []string
	confirmDirBarrier = func(p string) error { dirs = append(dirs, p); return origDir(p) }
	confirmFileBarrier = func(p string) error { files = append(files, p); return origFile(p) }
	if err := w.Confirm(context.Background(), repo, spec); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	abs := worktreeAbs(repo, spec)
	if !containsPath(dirs, abs) {
		t.Fatalf("dir barriers %v did not include the worktree dir %s", dirs, abs)
	}
	if !anyHasSuffix(files, filepath.Join(".git")) || len(files) == 0 {
		t.Fatalf("file barriers %v did not cover the linkage files", files)
	}
}

func containsPath(ps []string, want string) bool {
	for _, p := range ps {
		if samePath(p, want) {
			return true
		}
	}
	return false
}

func anyHasSuffix(ps []string, suffix string) bool {
	for _, p := range ps {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}
