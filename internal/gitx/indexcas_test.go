package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// indexcasSetup provisions a run worktree, snapshots an edit into a commit, moves the branch to it
// (so HEAD == commit with the real index still at the old base), and builds the txn-private target.
func indexcasSetup(t *testing.T) (repo string, g *Git, wt string, target IndexTarget) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	commit, tree, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	admin := strings.TrimSpace(string(mustRun(t, g, wt, nil, "rev-parse", "--absolute-git-dir")))
	i0, err := fileDigest(filepath.Join(admin, "index"))
	if err != nil {
		t.Fatalf("pre-index digest: %v", err)
	}
	if err := g.ApplyRef(context.Background(), repo, RefTarget{Branch: runBranch("r1"), Parent: base, Tree: tree, Commit: commit}); err != nil {
		t.Fatalf("move branch: %v", err)
	}
	private := filepath.Join(admin, "claudex-target-idx-r1")
	td, err := g.BuildTargetIndex(context.Background(), wt, commit, private)
	if err != nil {
		t.Fatalf("build target index: %v", err)
	}
	return repo, g, wt, IndexTarget{Worktree: wt, Commit: commit, Tree: tree, PreDigest: i0, TargetDigest: td, Private: private}
}

func adminIndex(t *testing.T, g *Git, wt string) string {
	t.Helper()
	admin := strings.TrimSpace(string(mustRun(t, g, wt, nil, "rev-parse", "--absolute-git-dir")))
	return filepath.Join(admin, "index")
}

func TestIndexCASHappyPath(t *testing.T) {
	_, g, wt, target := indexcasSetup(t)
	if st, err := g.ObserveIndex(context.Background(), target); err != nil || st != IndexAtOld {
		t.Fatalf("initial Observe = %v (err %v), want at-old", st, err)
	}
	if err := g.ApplyIndex(context.Background(), target); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if st, err := g.ObserveIndex(context.Background(), target); err != nil || st != IndexAtTarget {
		t.Fatalf("Observe after apply = %v (err %v), want at-target", st, err)
	}
	if err := g.ConfirmIndex(context.Background(), target); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// The worktree now reads clean without its bytes changing.
	if st := strings.TrimSpace(string(mustRun(t, g, wt, nil, "status", "--porcelain"))); st != "" {
		t.Fatalf("worktree not clean after sync: %q", st)
	}
	if b, err := os.ReadFile(filepath.Join(wt, "a.txt")); err != nil || string(b) != "edited" {
		t.Fatalf("worktree bytes changed: %q err=%v", b, err)
	}
}

func TestIndexCASIdempotent(t *testing.T) {
	_, g, _, target := indexcasSetup(t)
	if err := g.ApplyIndex(context.Background(), target); err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	if err := g.ApplyIndex(context.Background(), target); err != nil {
		t.Fatalf("apply 2 (idempotent): %v", err)
	}
}

// A foreign staged change in the real index after the snapshot is never overwritten (the D
// counterexample): the CAS sees the live index diverge from the frozen pre-identity and fails closed.
func TestIndexCASForeignStagedChange(t *testing.T) {
	_, g, wt, target := indexcasSetup(t)
	mustRun(t, g, wt, commitEnv(), "add", "a.txt") // stage the worktree change into the real index
	stagedDigest, err := fileDigest(adminIndex(t, g, wt))
	if err != nil {
		t.Fatalf("staged digest: %v", err)
	}
	if err := g.ApplyIndex(context.Background(), target); !errors.Is(err, ErrIndexCAS) {
		t.Fatalf("foreign staged change = %v, want ErrIndexCAS", err)
	}
	if d, _ := fileDigest(adminIndex(t, g, wt)); d != stagedDigest {
		t.Fatalf("the staged change was overwritten")
	}
}

// A foreign index.lock (not the same file as our private target) is never removed or overwritten.
func TestIndexCASForeignLock(t *testing.T) {
	_, g, wt, target := indexcasSetup(t)
	lockPath := adminIndex(t, g, wt) + ".lock"
	writeFile(t, lockPath, "a foreign git lock")
	if err := g.ApplyIndex(context.Background(), target); !errors.Is(err, ErrIndexCAS) {
		t.Fatalf("foreign lock = %v, want ErrIndexCAS", err)
	}
	if b, err := os.ReadFile(lockPath); err != nil || string(b) != "a foreign git lock" {
		t.Fatalf("foreign lock disturbed: %q err=%v", b, err)
	}
}

// A leftover lock that is the SAME FILE as our private target (our own crash prefix) is adopted and
// the sync completes forward.
func TestIndexCASAdoptsOwnLeftoverLock(t *testing.T) {
	_, g, wt, target := indexcasSetup(t)
	lockPath := adminIndex(t, g, wt) + ".lock"
	if err := os.Link(target.Private, lockPath); err != nil { // simulate a crash after link, before replace
		t.Fatalf("plant owned lock: %v", err)
	}
	if err := g.ApplyIndex(context.Background(), target); err != nil {
		t.Fatalf("apply adopting own lock: %v", err)
	}
	if st, err := g.ObserveIndex(context.Background(), target); err != nil || st != IndexAtTarget {
		t.Fatalf("Observe = %v (err %v), want at-target", st, err)
	}
}

// A worktree edit after the snapshot makes the worktree no longer the frozen tree: fail closed,
// worktree and index untouched.
func TestIndexCASPostSnapshotWorktreeChange(t *testing.T) {
	_, g, wt, target := indexcasSetup(t)
	writeFile(t, filepath.Join(wt, "a.txt"), "foreign-post-snapshot")
	before, _ := fileDigest(adminIndex(t, g, wt))
	if st, err := g.ObserveIndex(context.Background(), target); err != nil || st != IndexForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
	if err := g.ApplyIndex(context.Background(), target); !errors.Is(err, ErrIndexCAS) {
		t.Fatalf("apply after worktree change = %v, want ErrIndexCAS", err)
	}
	if d, _ := fileDigest(adminIndex(t, g, wt)); d != before {
		t.Fatalf("index overwritten despite a post-snapshot worktree change")
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "a.txt")); string(b) != "foreign-post-snapshot" {
		t.Fatalf("worktree bytes overwritten: %q", b)
	}
}

// Confirm rejects an index that has not been synced to the target.
func TestIndexCASConfirmRejectsUnsynced(t *testing.T) {
	_, g, _, target := indexcasSetup(t)
	if err := g.ConfirmIndex(context.Background(), target); !errors.Is(err, ErrIndexCAS) {
		t.Fatalf("confirm before sync = %v, want ErrIndexCAS", err)
	}
}
