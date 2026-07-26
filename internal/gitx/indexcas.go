package gitx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
)

// ErrIndexCAS is the sentinel an index-CAS failure wraps.
var ErrIndexCAS = errors.New("gitx: index CAS failed")

// adminRootRaceHook is a test-only seam fired inside openAdminRoot AFTER .git passes its Lstat
// as a real directory and BEFORE the OpenRoot + identity bind, so a test can swap .git for an
// in-root directory symlink and prove the bind rejects it. Nil in production.
var adminRootRaceHook func()

// IndexState classifies the real checked-out index against a frozen index target.
type IndexState int

const (
	// IndexAtOld: ref at the commit, real index still the frozen pre-index (I0), worktree content
	// exactly the frozen tree — the sync has not run (NotApplied).
	IndexAtOld IndexState = iota
	// IndexAtTarget: ref at the commit, real index the frozen target, worktree clean at the commit
	// (Applied).
	IndexAtTarget
	// IndexForeign: any foreign index/worktree/branch identity — fail closed, files/index untouched.
	IndexForeign
)

func (s IndexState) String() string {
	switch s {
	case IndexAtOld:
		return "at-old"
	case IndexAtTarget:
		return "at-target"
	default:
		return "foreign"
	}
}

// IndexTarget is the frozen identity of an index-CAS. Private is the txn-derived LEAF NAME of the
// deterministic target index inside the worktree's admin directory; every identity, link, replace,
// and removal runs through the pinned admin root (adminRoot), and ownership of index.lock is proven
// by same-file identity with the private target, never by content.
type IndexTarget struct {
	RepoDir      string // the repository root — the trust anchor the admin root is opened under
	Worktree     string // the run worktree directory
	Commit       string // the frozen commit (the run branch is already at it)
	Tree         string // the commit's tree — the worktree must still match it
	PreDigest    string // I0: the frozen digest of the real index at snapshot time
	TargetDigest string // the frozen digest of the target index bytes at Private
	Private      string // the txn-private target-index LEAF name inside the admin dir
}

const indexLeaf = "index"

// TargetIndexName derives the txn-private target-index leaf name for txnID — a sibling of the
// real index inside the admin dir (so the hard-link CAS stays on one filesystem), named by the
// transaction id so recovery re-derives the exact journalled name and two transactions can never
// collide.
func TargetIndexName(txnID string) string { return "index.claudex-target-" + txnID }

// adminRoot anchors every index-CAS operation inside the repository's own git metadata: an
// os.Root at <RepoDir> (the trust anchor), a real-directory .git opened from it, a
// symlink-refusing verified walk of the admin-relative components, and a SUB-ROOT handle
// opened at the admin directory itself whose identity is bound back to the verified directory
// object. Every subsequent operation is a single LEAF name on that pinned handle, so an
// intermediate path component later swapped for a symlink/reparse point cannot redirect the CAS
// anywhere — the handle still names the original directory object.
type adminRoot struct {
	git *os.Root // <RepoDir>/.git — held open so the verified chain stays pinned
	fs  *os.Root // the admin directory; every operation is a bare leaf name on this handle
	abs string   // the logical in-repo admin path (for GIT_INDEX_FILE only; barriers stay rooted)
}

// openAdminRoot resolves the worktree's admin directory, requires it to sit beneath the
// repository's own .git with every intermediate component a real (non-symlink) in-root
// directory, opens a pinned sub-root at it, and binds that sub-root's identity back to the
// verified directory object (closing the check/open race: an in-root swap between the walk and
// the open selects a different object, which the same-file bind then rejects). Any escape or
// redirect fails closed with the typed sentinel.
func (g *Git) openAdminRoot(ctx context.Context, repoDir, worktree string) (*adminRoot, error) {
	if repoDir == "" {
		return nil, fmt.Errorf("%w: the repository root is required", ErrIndexCAS)
	}
	// Anchor .git from an opened repository root, so .git itself being a symlink is refused
	// rather than silently followed — and bind the OPENED .git object back to the verified
	// directory (same check/open-race close as the admin sub-root below, one level up).
	repoRoot, err := os.OpenRoot(repoDir)
	if err != nil {
		return nil, err
	}
	gfi, err := repoRoot.Lstat(".git")
	if err != nil {
		return nil, errors.Join(err, repoRoot.Close())
	}
	if gfi.Mode()&os.ModeSymlink != 0 || !gfi.IsDir() {
		return nil, errors.Join(fmt.Errorf("%w: %s/.git is not a real directory", ErrIndexCAS, repoDir), repoRoot.Close())
	}
	if adminRootRaceHook != nil {
		adminRootRaceHook() // test seam: swap .git between its Lstat and the OpenRoot+bind
	}
	gitRoot, err := repoRoot.OpenRoot(".git")
	if err != nil {
		// os.Root refuses an in-root symlink component here on platforms that do not follow
		// it (e.g. Windows); platforms that DO follow it are caught by the identity bind
		// below. Either way it is a typed security refusal.
		return nil, errors.Join(fmt.Errorf("%w: cannot open .git as a real directory: %v", ErrIndexCAS, err), repoRoot.Close())
	}
	gitStat, err := gitRoot.Stat(".")
	if err != nil {
		return nil, errors.Join(err, gitRoot.Close(), repoRoot.Close())
	}
	// Re-Lstat .git at the bind point: it must STILL be a real directory (a symlink pointing
	// back at the same object would pass SameFile but violates symlink-refusal), and the opened
	// object must be exactly the verified one.
	reGfi, err := repoRoot.Lstat(".git")
	if err != nil {
		return nil, errors.Join(err, gitRoot.Close(), repoRoot.Close())
	}
	if reGfi.Mode()&os.ModeSymlink != 0 || !reGfi.IsDir() || !os.SameFile(gfi, gitStat) || !os.SameFile(reGfi, gitStat) {
		return nil, errors.Join(fmt.Errorf("%w: %s/.git changed identity during resolution", ErrIndexCAS, repoDir), gitRoot.Close(), repoRoot.Close())
	}
	if cerr := repoRoot.Close(); cerr != nil {
		return nil, errors.Join(cerr, gitRoot.Close())
	}

	common := filepath.Join(repoDir, ".git")
	admin, err := g.revParse(ctx, worktree, "--absolute-git-dir")
	if err != nil {
		return nil, errors.Join(err, gitRoot.Close())
	}
	rel, err := filepath.Rel(common, admin)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, errors.Join(fmt.Errorf("%w: the worktree admin dir %q escapes the repository git dir", ErrIndexCAS, admin), gitRoot.Close())
	}
	if rel != "." {
		// Prove every intermediate component is a real in-root directory, never a
		// symlink/reparse point, from the leaf back to the root.
		for prefix := rel; ; prefix = filepath.Dir(prefix) {
			fi, lerr := gitRoot.Lstat(prefix)
			if lerr != nil {
				return nil, errors.Join(lerr, gitRoot.Close())
			}
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				return nil, errors.Join(fmt.Errorf("%w: admin path component %q is not a real directory", ErrIndexCAS, prefix), gitRoot.Close())
			}
			if filepath.Dir(prefix) == "." {
				break
			}
		}
	}
	verifiedFI, err := gitRoot.Lstat(rel)
	if err != nil {
		return nil, errors.Join(err, gitRoot.Close())
	}
	adminFS, err := gitRoot.OpenRoot(rel)
	if err != nil {
		// As with .git above: an in-root symlink component os.Root refuses to traverse is a
		// typed security refusal (platforms that would follow it are caught by the bind below).
		return nil, errors.Join(fmt.Errorf("%w: cannot open the admin dir as a real directory: %v", ErrIndexCAS, err), gitRoot.Close())
	}
	openedFI, err := adminFS.Stat(".")
	if err != nil {
		return nil, errors.Join(err, adminFS.Close(), gitRoot.Close())
	}
	if !os.SameFile(verifiedFI, openedFI) {
		return nil, errors.Join(fmt.Errorf("%w: the admin directory changed identity during resolution", ErrIndexCAS), adminFS.Close(), gitRoot.Close())
	}
	return &adminRoot{git: gitRoot, fs: adminFS, abs: filepath.Join(common, rel)}, nil
}

func (a *adminRoot) Close() error { return errors.Join(a.fs.Close(), a.git.Close()) }

// digest hashes the regular file named leaf inside the pinned admin root with a
// symlink-refusing, race-free identity proof: the leaf must Lstat as a REGULAR file and the
// opened handle must be that same object (os.SameFile over Lstat/fstat), so a name swapped
// for a symlink — even one pointing at byte-identical contents — fails closed rather than
// hashing through the link. Identity and ownership bind to regular objects only.
func (a *adminRoot) digest(leaf string) (string, error) {
	fi, err := a.fs.Lstat(leaf)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", ErrIndexCAS, leaf)
	}
	f, err := a.fs.Open(leaf)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hfi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(fi, hfi) {
		return "", fmt.Errorf("%w: %s changed identity during the read", ErrIndexCAS, leaf)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sameLeaf reports whether two leaves in the pinned admin root are the same filesystem object,
// via Lstat so a symlink is never treated as the same file as a regular target. A missing leaf
// is (false, nil).
func (a *adminRoot) sameLeaf(x, y string) (bool, error) {
	fx, err := a.fs.Lstat(x)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	fy, err := a.fs.Lstat(y)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(fx, fy), nil
}

// proveOwnedLock requires BOTH the lock leaf and the private leaf to be REGULAR files that are
// the same filesystem object — ownership binds to that regular object, never to a symlink that
// redirects elsewhere.
func (a *adminRoot) proveOwnedLock(private, lock string) (bool, error) {
	lfi, err := a.fs.Lstat(lock)
	if err != nil {
		return false, err
	}
	if !lfi.Mode().IsRegular() {
		return false, fmt.Errorf("%w: index.lock is not a regular file", ErrIndexCAS)
	}
	pfi, err := a.fs.Lstat(private)
	if err != nil {
		return false, err
	}
	if !pfi.Mode().IsRegular() {
		return false, fmt.Errorf("%w: the txn-private target is not a regular file", ErrIndexCAS)
	}
	return os.SameFile(lfi, pfi), nil
}

// acquireOwnedLock hard-links the private leaf onto the lock leaf inside the pinned root. On
// success the two names are the same file. On EEXIST it adopts a leftover lock ONLY if same-file
// identity proves it is our private target; matching bytes without same-file identity are foreign
// (owned=false, the lock is untouched). Every owned outcome is re-proven as a REGULAR object, so
// a name swapped for a symlink under the link race can win the name but never ownership.
func (a *adminRoot) acquireOwnedLock(private, lock string) (owned bool, err error) {
	if err := a.fs.Link(private, lock); err == nil {
		return a.proveOwnedLock(private, lock)
	} else if !os.IsExist(err) {
		return false, err
	}
	same, err := a.sameLeaf(lock, private)
	if err != nil || !same {
		return false, err
	}
	return a.proveOwnedLock(private, lock)
}

// releaseOwnedLock removes the same-file-proven owned lock leaf (never a foreign one) and
// returns cause.
func (a *adminRoot) releaseOwnedLock(private, lock string, cause error) error {
	if same, err := a.sameLeaf(lock, private); err == nil && same {
		_ = a.fs.Remove(lock)
	}
	return cause
}

// syncLeaf forces a leaf's CONTENT and its directory ENTRY durable entirely through the pinned
// admin root (atomicfile's rooted machinery: a writable-handle FlushFileBuffers on Windows, an
// fsync on POSIX, plus the real rooted parent-entry barrier) — never a fresh absolute-path
// traversal that a later intermediate-component swap could redirect. A MISSING leaf is an error
// (the index and the private proof must exist through terminality), surfaced as ErrIndexCAS.
func (a *adminRoot) syncLeaf(leaf string) error {
	if err := atomicfile.SyncInRoot(a.fs, leaf); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s is missing", ErrIndexCAS, leaf)
		}
		return err
	}
	return nil
}

// probeHardLink proves hard-link support next to the private leaf inside the pinned root.
func (a *adminRoot) probeHardLink(near string) error {
	tmp := near + ".linkprobe"
	_ = a.fs.Remove(tmp)
	if err := a.fs.Link(near, tmp); err != nil {
		return err
	}
	return a.fs.Remove(tmp)
}

// BuildTargetIndex builds the target index (from Commit, into the txn-private leaf inside the
// pinned admin root), makes it durable, and PROVES hard-link support here — so a hard-link-
// incapable filesystem fails BEFORE the journal PREPARE rather than degrading to
// content-as-ownership. It runs pre-PREPARE and returns the digest to freeze; recovery
// afterwards validates and uses those exact bytes, never rebuilding.
func (g *Git) BuildTargetIndex(ctx context.Context, repoDir, worktree, commit, privateName string) (string, error) {
	ar, err := g.openAdminRoot(ctx, repoDir, worktree)
	if err != nil {
		return "", err
	}
	defer ar.Close()
	_ = ar.fs.Remove(privateName) // a pre-PREPARE retry rebuilds fresh
	if _, err := g.Run(ctx, worktree, map[string]string{"GIT_INDEX_FILE": filepath.Join(ar.abs, privateName)}, "read-tree", commit); err != nil {
		return "", err
	}
	if err := ar.probeHardLink(privateName); err != nil {
		return "", fmt.Errorf("%w: hard links unavailable on the index filesystem: %v", ErrIndexCAS, err)
	}
	// Content + entry durability, entirely through the pinned root.
	if err := ar.syncLeaf(privateName); err != nil {
		return "", err
	}
	return ar.digest(privateName)
}

// ObserveIndex classifies the real index through the pinned admin root. The txn-private target
// must still be the frozen REGULAR object (a vanished, redirected, or byte-drifted private is
// foreign interference — Indeterminate, never an ownership anchor to install or adopt); the
// worktree must still equal the frozen tree (a post-snapshot edit is foreign, not overwritten);
// the index is either the frozen pre-index (NotApplied), the frozen target with a clean worktree
// (Applied), or foreign (Indeterminate).
func (g *Git) ObserveIndex(ctx context.Context, t IndexTarget) (IndexState, error) {
	ar, err := g.openAdminRoot(ctx, t.RepoDir, t.Worktree)
	if err != nil {
		return IndexForeign, err
	}
	defer ar.Close()
	if d, derr := ar.digest(t.Private); derr != nil {
		return IndexForeign, derr
	} else if d != t.TargetDigest {
		return IndexForeign, nil
	}
	if head, err := g.revParse(ctx, t.Worktree, "--verify", "HEAD"); err != nil {
		return IndexForeign, err
	} else if head != t.Commit {
		return IndexForeign, nil
	}
	// The worktree content must still be exactly the frozen tree (independent of the real index).
	wtTree, err := g.snapshotTree(ctx, t.Worktree, t.Commit)
	if err != nil {
		return IndexForeign, err
	}
	if wtTree != t.Tree {
		return IndexForeign, nil // a post-snapshot worktree change — never overwrite it
	}
	live, err := ar.digest(indexLeaf)
	if err != nil {
		return IndexForeign, err
	}
	switch live {
	case t.PreDigest:
		return IndexAtOld, nil
	case t.TargetDigest:
		clean, err := g.worktreeClean(ctx, t.Worktree)
		if err != nil {
			return IndexForeign, err
		}
		if clean {
			return IndexAtTarget, nil
		}
		return IndexForeign, nil
	default:
		return IndexForeign, nil
	}
}

// ApplyIndex publishes the target index as the real index via a compare-and-swap, entirely on
// leaf names inside the pinned admin root: it acquires the standard index.lock by a no-replace
// hard link from the txn-private target (adopting a leftover lock only by same-file identity,
// never by matching bytes), rechecks the live index still has the frozen pre-identity, then
// atomically replaces the index with the owned lock. It never removes or overwrites a foreign
// lock or a divergent live index.
func (g *Git) ApplyIndex(ctx context.Context, t IndexTarget) error {
	ar, err := g.openAdminRoot(ctx, t.RepoDir, t.Worktree)
	if err != nil {
		return err
	}
	defer ar.Close()
	lockLeaf := indexLeaf + ".lock"

	// Re-prove non-foreign before touching anything.
	if head, err := g.revParse(ctx, t.Worktree, "--verify", "HEAD"); err != nil {
		return err
	} else if head != t.Commit {
		return fmt.Errorf("%w: HEAD is not the frozen commit", ErrIndexCAS)
	}
	if wtTree, err := g.snapshotTree(ctx, t.Worktree, t.Commit); err != nil {
		return err
	} else if wtTree != t.Tree {
		return fmt.Errorf("%w: worktree changed after the snapshot", ErrIndexCAS)
	}
	// The private target must be present with the frozen digest — recovery uses these exact bytes.
	if d, err := ar.digest(t.Private); err != nil {
		return fmt.Errorf("%w: target index: %v", ErrIndexCAS, err)
	} else if d != t.TargetDigest {
		return fmt.Errorf("%w: target index digest changed", ErrIndexCAS)
	}

	owned, err := ar.acquireOwnedLock(t.Private, lockLeaf)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("%w: index.lock is held by a foreign process", ErrIndexCAS)
	}

	live, err := ar.digest(indexLeaf)
	if err != nil {
		return ar.releaseOwnedLock(t.Private, lockLeaf, err)
	}
	if live == t.TargetDigest {
		// Already published (a crash after the replace) — idempotent success.
		return ar.releaseOwnedLock(t.Private, lockLeaf, nil)
	}
	if live != t.PreDigest {
		// A foreign staged change appeared — never overwrite it.
		return ar.releaseOwnedLock(t.Private, lockLeaf, fmt.Errorf("%w: the real index diverged from the frozen pre-identity", ErrIndexCAS))
	}
	// Atomically replace the index with the owned lock (the same file as the target), inside the
	// pinned root. Durability is established by ConfirmIndex's barriers before the journal
	// records this step's progress.
	if err := ar.fs.Rename(lockLeaf, indexLeaf); err != nil {
		return ar.releaseOwnedLock(t.Private, lockLeaf, fmt.Errorf("%w: publish: %v", ErrIndexCAS, err))
	}
	return nil
}

// confirmRaceHook is a test-only seam fired inside ConfirmIndex AFTER the standard index.lock
// is acquired and BEFORE the exact-target re-proof, so a test can inject an index replacement
// or a private deletion in that window and prove the locked proof still fails closed. Nil in
// production.
var confirmRaceHook func() error

// ConfirmIndex is the transaction's durable index proof, executed as ONE lock-protected rooted
// operation: it acquires the standard index.lock (by the same hard-link/same-file ownership as
// the CAS) so no concurrent git writer can replace the index underneath it, then — holding the
// lock — re-proves the EXACT frozen target (HEAD==commit, worktree==tree, clean, the real index
// digest == the frozen target, the txn-private proof present with the frozen digest) and forces
// the index content+entry and the private proof durable through the pinned admin root. A missing
// private, a replaced index, or a foreign lock all fail closed, so the journal never records the
// index step over an unconfirmed or foreign index. Idempotent and re-runnable on recovery.
func (g *Git) ConfirmIndex(ctx context.Context, t IndexTarget) error {
	ar, err := g.openAdminRoot(ctx, t.RepoDir, t.Worktree)
	if err != nil {
		return err
	}
	defer ar.Close()
	lockLeaf := indexLeaf + ".lock"

	// Acquire the standard index lock as exclusion: while held, a well-behaved git writer
	// cannot take the index lock to rewrite the index, and a foreign lock fails us closed.
	owned, err := ar.acquireOwnedLock(t.Private, lockLeaf)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("%w: index.lock is held by a foreign process", ErrIndexCAS)
	}
	// Remove the exclusion lock on every exit (it is a hard link to the private target, never
	// the published index, so removing it never disturbs the applied index).
	defer func() { _ = ar.releaseOwnedLock(t.Private, lockLeaf, nil) }()

	if confirmRaceHook != nil {
		if herr := confirmRaceHook(); herr != nil {
			return herr
		}
	}
	// Exact-target re-proof UNDER the lock. The rooted BYTE identities come first — they are
	// deterministic and typed, so a foreign index replacement or a deleted private fails closed
	// with ErrIndexCAS before a git subprocess would choke on the foreign index.
	if d, derr := ar.digest(indexLeaf); derr != nil {
		return fmt.Errorf("%w: index: %v", ErrIndexCAS, derr)
	} else if d != t.TargetDigest {
		return fmt.Errorf("%w: the real index is not the frozen target", ErrIndexCAS)
	}
	if d, derr := ar.digest(t.Private); derr != nil {
		return fmt.Errorf("%w: txn-private target: %v", ErrIndexCAS, derr)
	} else if d != t.TargetDigest {
		return fmt.Errorf("%w: the txn-private target changed", ErrIndexCAS)
	}
	// Then the git identity of the worktree the applied index belongs to.
	if head, herr := g.revParse(ctx, t.Worktree, "--verify", "HEAD"); herr != nil {
		return herr
	} else if head != t.Commit {
		return fmt.Errorf("%w: HEAD is not the frozen commit", ErrIndexCAS)
	}
	if wtTree, terr := g.snapshotTree(ctx, t.Worktree, t.Commit); terr != nil {
		return terr
	} else if wtTree != t.Tree {
		return fmt.Errorf("%w: worktree changed after the snapshot", ErrIndexCAS)
	}
	if clean, cerr := g.worktreeClean(ctx, t.Worktree); cerr != nil {
		return cerr
	} else if !clean {
		return fmt.Errorf("%w: worktree is not clean at the applied index", ErrIndexCAS)
	}
	// Durability, all through the pinned root; a missing leaf is an error.
	if serr := ar.syncLeaf(indexLeaf); serr != nil {
		return fmt.Errorf("gitx: confirm index: %w", serr)
	}
	if serr := ar.syncLeaf(t.Private); serr != nil {
		return fmt.Errorf("gitx: confirm target proof: %w", serr)
	}
	return nil
}

// ConfirmPreState re-proves the FULL frozen pre-transaction identity immediately
// before the journal PREPARE: the run worktree is still the registered linked worktree
// of this repository with SYMBOLIC HEAD on the frozen run branch at the frozen parent
// (the same validateRunWorktree proof the snapshot ran — OID equality alone would not
// catch a detach or a same-OID branch switch in the snapshot..prepare window), the
// worktree content is still exactly the frozen tree, the real checked-out index still
// has the frozen pre-digest I0, and the txn-private target still holds exactly the
// frozen target bytes — all identity reads through the pinned admin root. Any drift
// fails closed BEFORE any journal record or effect exists.
func (g *Git) ConfirmPreState(ctx context.Context, req SnapshotReq, tree, preDigest, privateName, targetDigest string) error {
	wt := runWorktreeAbs(req.RepoDir, req.RunID)
	branch := runBranch(req.RunID)
	if err := g.validateRunWorktree(ctx, req, wt, branch); err != nil {
		return err
	}
	wtTree, err := g.snapshotTree(ctx, wt, req.Parent)
	if err != nil {
		return err
	}
	if wtTree != tree {
		return fmt.Errorf("%w: the worktree changed after the snapshot", ErrIndexCAS)
	}
	ar, err := g.openAdminRoot(ctx, req.RepoDir, wt)
	if err != nil {
		return err
	}
	defer ar.Close()
	live, err := ar.digest(indexLeaf)
	if err != nil {
		return err
	}
	if live != preDigest {
		return fmt.Errorf("%w: the real index diverged from the frozen pre-identity before prepare", ErrIndexCAS)
	}
	if d, err := ar.digest(privateName); err != nil {
		return fmt.Errorf("%w: txn-private target: %v", ErrIndexCAS, err)
	} else if d != targetDigest {
		return fmt.Errorf("%w: the txn-private target changed before prepare", ErrIndexCAS)
	}
	return nil
}

// IndexDigest returns the digest of the worktree's real checked-out index through the pinned
// admin root — the I0 the transaction freezes before the index-cas step.
func (g *Git) IndexDigest(ctx context.Context, repoDir, worktree string) (string, error) {
	ar, err := g.openAdminRoot(ctx, repoDir, worktree)
	if err != nil {
		return "", err
	}
	defer ar.Close()
	return ar.digest(indexLeaf)
}

func (g *Git) worktreeClean(ctx context.Context, worktree string) (bool, error) {
	out, err := g.Run(ctx, worktree, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return len(strings.TrimRight(string(out), "\x00")) == 0, nil
}

// WorktreeClean reports whether the run worktree has no tracked, staged, or untracked
// changes (`.claudex` is git-ignored, so the run's own metadata never trips it). It is
// the single worktree-dirt definition shared by the index-CAS and the read-only-phase
// edit-policy gate.
func (g *Git) WorktreeClean(ctx context.Context, worktree string) (bool, error) {
	return g.worktreeClean(ctx, worktree)
}
