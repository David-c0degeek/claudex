package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// provisionRepo returns a one-commit repo, its git handle, a worktree spec anchored to the
// initial commit, and that base OID.
func provisionRepo(t *testing.T) (string, *Git, WorktreeSpec, string) {
	t.Helper()
	repo, g := initRepo(t)
	base := oidOf(t, g, repo, "refs/heads/main")
	spec := WorktreeSpec{RelPath: ".claudex/runs/r1/worktree", Branch: "claudex/r1", BaseCommit: base}
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
	reg := findReg(regs, abs)
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
