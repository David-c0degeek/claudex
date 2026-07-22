package gitx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	i0, err := g.IndexDigest(context.Background(), repo, wt)
	if err != nil {
		t.Fatalf("pre-index digest: %v", err)
	}
	if err := g.ApplyRef(context.Background(), repo, RefTarget{Branch: runBranch("r1"), Parent: base, Tree: tree, Commit: commit}); err != nil {
		t.Fatalf("move branch: %v", err)
	}
	private := TargetIndexName("idx-r1")
	td, err := g.BuildTargetIndex(context.Background(), repo, wt, commit, private)
	if err != nil {
		t.Fatalf("build target index: %v", err)
	}
	return repo, g, wt, IndexTarget{RepoDir: repo, Worktree: wt, Commit: commit, Tree: tree, PreDigest: i0, TargetDigest: td, Private: private}
}

func adminDir(t *testing.T, g *Git, wt string) string {
	t.Helper()
	return strings.TrimSpace(string(mustRun(t, g, wt, nil, "rev-parse", "--absolute-git-dir")))
}

func adminIndex(t *testing.T, g *Git, wt string) string {
	t.Helper()
	return filepath.Join(adminDir(t, g, wt), "index")
}

// digestOf is the test-side byte hash of a file (the attacker's view; the production
// digest path is the rooted, symlink-refusing adminRoot.digest).
func digestOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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

// A txn-private name swapped for a SYMLINK to byte-identical target contents is never an
// ownership anchor: Observe classifies it foreign (never NotApplied/Applied), Apply refuses,
// and the real index is untouched — identity binds to the regular object, not to bytes.
func TestIndexCASPrivateSymlinkRefused(t *testing.T) {
	_, g, wt, target := indexcasSetup(t)
	privateAbs := filepath.Join(adminDir(t, g, wt), target.Private)
	// Copy the exact frozen bytes elsewhere, then replace the private NAME with a symlink.
	copyPath := privateAbs + ".copy"
	b, err := os.ReadFile(privateAbs)
	if err != nil {
		t.Fatalf("read private: %v", err)
	}
	if err := os.WriteFile(copyPath, b, 0o600); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	if err := os.Remove(privateAbs); err != nil {
		t.Fatalf("remove private: %v", err)
	}
	if err := os.Symlink(copyPath, privateAbs); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}

	liveBefore, err := os.ReadFile(adminIndex(t, g, wt))
	if err != nil {
		t.Fatalf("read live index: %v", err)
	}
	st, oerr := g.ObserveIndex(context.Background(), target)
	if st != IndexForeign || !errors.Is(oerr, ErrIndexCAS) {
		t.Fatalf("Observe over a symlinked private = %v (err %v), want foreign + ErrIndexCAS", st, oerr)
	}
	if aerr := g.ApplyIndex(context.Background(), target); !errors.Is(aerr, ErrIndexCAS) {
		t.Fatalf("Apply over a symlinked private err = %v, want ErrIndexCAS", aerr)
	}
	liveAfter, err := os.ReadFile(adminIndex(t, g, wt))
	if err != nil {
		t.Fatalf("re-read live index: %v", err)
	}
	if string(liveBefore) != string(liveAfter) {
		t.Fatal("a symlinked private still mutated the real index")
	}
	if fi, err := os.Lstat(privateAbs); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the foreign symlink was not preserved (mode %v, err %v)", fi.Mode(), err)
	}
}

// An INTERMEDIATE admin path component swapped for a symlink to an outside directory —
// containing regular, byte-identical index and private leaves — must fail every identity
// and mutation closed: the CAS is anchored in the repository's .git root and refuses the
// redirect, and the outside index/lock are never touched.
func TestIndexCASAdminEscapeFailsClosed(t *testing.T) {
	repo, g, wt, target := indexcasSetup(t)
	admin := adminDir(t, g, wt)
	outside := filepath.Join(t.TempDir(), "outside-admin")
	if err := os.Rename(admin, outside); err != nil {
		t.Fatalf("relocate admin dir: %v", err)
	}
	if err := os.Symlink(outside, admin); err != nil {
		// Restore and skip: the host cannot express the attack.
		if rerr := os.Rename(outside, admin); rerr != nil {
			t.Fatalf("restore admin dir: %v", rerr)
		}
		t.Skipf("directory symlinks unavailable on this host: %v", err)
	}
	outsideIndex := filepath.Join(outside, "index")
	beforeIndex, err := os.ReadFile(outsideIndex)
	if err != nil {
		t.Fatalf("read outside index: %v", err)
	}

	if _, derr := g.IndexDigest(context.Background(), repo, wt); !errors.Is(derr, ErrIndexCAS) {
		t.Fatalf("IndexDigest through the redirect err = %v, want ErrIndexCAS", derr)
	}
	st, oerr := g.ObserveIndex(context.Background(), target)
	if st != IndexForeign || !errors.Is(oerr, ErrIndexCAS) {
		t.Fatalf("Observe through the redirect = %v (err %v), want foreign + ErrIndexCAS", st, oerr)
	}
	if aerr := g.ApplyIndex(context.Background(), target); !errors.Is(aerr, ErrIndexCAS) {
		t.Fatalf("Apply through the redirect err = %v, want ErrIndexCAS", aerr)
	}

	afterIndex, err := os.ReadFile(outsideIndex)
	if err != nil {
		t.Fatalf("re-read outside index: %v", err)
	}
	if string(beforeIndex) != string(afterIndex) {
		t.Fatal("the redirect still mutated the outside index")
	}
	if _, err := os.Lstat(outsideIndex + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("the redirect still created an outside index.lock (err %v)", err)
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
	stagedDigest := digestOf(t, adminIndex(t, g, wt))
	if err := g.ApplyIndex(context.Background(), target); !errors.Is(err, ErrIndexCAS) {
		t.Fatalf("foreign staged change = %v, want ErrIndexCAS", err)
	}
	if d := digestOf(t, adminIndex(t, g, wt)); d != stagedDigest {
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
	privateAbs := filepath.Join(adminDir(t, g, wt), target.Private)
	lockPath := adminIndex(t, g, wt) + ".lock"
	if err := os.Link(privateAbs, lockPath); err != nil { // simulate a crash after link, before replace
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
	before := digestOf(t, adminIndex(t, g, wt))
	if st, err := g.ObserveIndex(context.Background(), target); err != nil || st != IndexForeign {
		t.Fatalf("Observe = %v (err %v), want foreign", st, err)
	}
	if err := g.ApplyIndex(context.Background(), target); !errors.Is(err, ErrIndexCAS) {
		t.Fatalf("apply after worktree change = %v, want ErrIndexCAS", err)
	}
	if d := digestOf(t, adminIndex(t, g, wt)); d != before {
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
