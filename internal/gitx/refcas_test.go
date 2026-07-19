package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// refcasTarget provisions a run worktree, snapshots an edit into a real commit, and returns the
// repo, handle, and the frozen ref target (branch still at the base parent).
func refcasTarget(t *testing.T) (repo string, g *Git, target RefTarget) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	commit, tree, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return repo, g, RefTarget{Branch: runBranch("r1"), Parent: base, Tree: tree, Commit: commit}
}

func TestRefCASHappyPath(t *testing.T) {
	repo, g, target := refcasTarget(t)
	if st, err := g.ObserveRef(context.Background(), repo, target); err != nil || st != RefAtParent {
		t.Fatalf("initial Observe = %v (err %v), want at-parent", st, err)
	}
	if err := g.ApplyRef(context.Background(), repo, target); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if st, err := g.ObserveRef(context.Background(), repo, target); err != nil || st != RefAtCommit {
		t.Fatalf("Observe after apply = %v (err %v), want at-commit", st, err)
	}
	if err := g.ConfirmRef(context.Background(), repo, target); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if oid := oidOf(t, g, repo, "refs/heads/"+target.Branch); oid != target.Commit {
		t.Fatalf("branch at %q, want commit %q", oid, target.Commit)
	}
}

func TestRefCASIdempotent(t *testing.T) {
	repo, g, target := refcasTarget(t)
	if err := g.ApplyRef(context.Background(), repo, target); err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	if err := g.ApplyRef(context.Background(), repo, target); err != nil {
		t.Fatalf("apply 2 (idempotent): %v", err)
	}
}

// A branch that has drifted off the expected parent (neither parent nor commit) fails closed and is
// never moved.
func TestRefCASForeignRef(t *testing.T) {
	repo, g, target := refcasTarget(t)
	// Advance the branch to an unrelated third commit.
	third := commitFile(t, g, repo, "z.txt", "third")
	mustRun(t, g, repo, nil, "update-ref", "refs/heads/"+target.Branch, third)
	if st, err := g.ObserveRef(context.Background(), repo, target); err != nil || st != RefForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
	if err := g.ApplyRef(context.Background(), repo, target); !errors.Is(err, ErrRefCAS) {
		t.Fatalf("apply over a foreign ref = %v, want ErrRefCAS", err)
	}
	if oid := oidOf(t, g, repo, "refs/heads/"+target.Branch); oid != third {
		t.Fatalf("foreign branch moved to %q, want %q untouched", oid, third)
	}
}

// A target whose frozen tree does not match the commit object fails closed before the ref moves.
func TestRefCASBadObjectTree(t *testing.T) {
	repo, g, target := refcasTarget(t)
	target.Tree = target.Parent // a valid OID, but not the commit's tree
	if err := g.ApplyRef(context.Background(), repo, target); !errors.Is(err, ErrRefCAS) {
		t.Fatalf("bad tree = %v, want ErrRefCAS", err)
	}
	if oid := oidOf(t, g, repo, "refs/heads/"+target.Branch); oid != target.Parent {
		t.Fatalf("branch moved despite a bad object: %q", oid)
	}
}

// A target whose frozen parent does not match the commit's sole parent fails closed.
func TestRefCASBadObjectParent(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	commit, tree, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Claim a different parent than the commit actually has.
	other := commitFile(t, g, repo, "o.txt", "other")
	target := RefTarget{Branch: runBranch("r1"), Parent: other, Tree: tree, Commit: commit}
	// Point the branch at `other` so the CAS old-value matches and we reach the object proof.
	mustRun(t, g, repo, nil, "update-ref", "refs/heads/"+target.Branch, other)
	if err := g.ApplyRef(context.Background(), repo, target); !errors.Is(err, ErrRefCAS) {
		t.Fatalf("bad parent = %v, want ErrRefCAS", err)
	}
}

// Confirm re-proves the applied ref, so a branch that drifted after Apply fails closed.
func TestRefCASConfirmDetectsDrift(t *testing.T) {
	repo, g, target := refcasTarget(t)
	if err := g.ApplyRef(context.Background(), repo, target); err != nil {
		t.Fatalf("apply: %v", err)
	}
	drift := commitFile(t, g, repo, "d.txt", "drift")
	mustRun(t, g, repo, nil, "update-ref", "refs/heads/"+target.Branch, drift)
	if err := g.ConfirmRef(context.Background(), repo, target); !errors.Is(err, ErrRefCAS) {
		t.Fatalf("confirm after drift = %v, want ErrRefCAS", err)
	}
}
