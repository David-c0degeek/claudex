package gitx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
)

// ErrIndexCAS is the sentinel an index-CAS failure wraps.
var ErrIndexCAS = errors.New("gitx: index CAS failed")

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

// IndexTarget is the frozen identity of an index-CAS. Private is a txn-derived path on the real
// index's filesystem holding the deterministic target index; ownership of index.lock is proven by
// same-file identity with Private, never by content.
type IndexTarget struct {
	Worktree     string // the run worktree directory
	Commit       string // the frozen commit (the run branch is already at it)
	Tree         string // the commit's tree — the worktree must still match it
	PreDigest    string // I0: the frozen digest of the real index at snapshot time
	TargetDigest string // the frozen digest of the target index bytes at Private
	Private      string // the txn-private target-index path (on the real index's filesystem)
}

// BuildTargetIndex builds the target index (from Commit, into the txn-private path), makes it
// durable, and PROVES hard-link support here — so a hard-link-incapable filesystem fails BEFORE the
// journal PREPARE rather than degrading to content-as-ownership. It runs pre-PREPARE and returns the
// digest to freeze; recovery afterwards validates and uses those exact bytes, never rebuilding.
func (g *Git) BuildTargetIndex(ctx context.Context, worktree, commit, private string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(private), 0o700); err != nil {
		return "", err
	}
	_ = os.Remove(private) // a pre-PREPARE retry rebuilds fresh
	if _, err := g.Run(ctx, worktree, map[string]string{"GIT_INDEX_FILE": private}, "read-tree", commit); err != nil {
		return "", err
	}
	if err := probeHardLink(private); err != nil {
		return "", fmt.Errorf("%w: hard links unavailable on the index filesystem: %v", ErrIndexCAS, err)
	}
	if err := confirmFileBarrier(private); err != nil {
		return "", err
	}
	if err := atomicfile.ParentBarrier(private); err != nil {
		return "", err
	}
	return fileDigest(private)
}

// ObserveIndex classifies the real index. The worktree must still equal the frozen tree (a
// post-snapshot edit is foreign, not overwritten); the index is either the frozen pre-index
// (NotApplied), the frozen target with a clean worktree (Applied), or foreign (Indeterminate).
func (g *Git) ObserveIndex(ctx context.Context, t IndexTarget) (IndexState, error) {
	indexPath, err := g.realIndexPath(ctx, t.Worktree)
	if err != nil {
		return IndexForeign, err
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
	live, err := fileDigest(indexPath)
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

// ApplyIndex publishes the target index as the real index via a compare-and-swap: it acquires the
// standard index.lock by a no-replace hard link from the txn-private target (adopting a leftover
// lock only by same-file identity, never by matching bytes), rechecks the live index still has the
// frozen pre-identity, then atomically replaces the index with the owned lock. It never removes or
// overwrites a foreign lock or a divergent live index.
func (g *Git) ApplyIndex(ctx context.Context, t IndexTarget) error {
	indexPath, err := g.realIndexPath(ctx, t.Worktree)
	if err != nil {
		return err
	}
	lockPath := indexPath + ".lock"

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
	if d, err := fileDigest(t.Private); err != nil {
		return fmt.Errorf("%w: target index: %v", ErrIndexCAS, err)
	} else if d != t.TargetDigest {
		return fmt.Errorf("%w: target index digest changed", ErrIndexCAS)
	}

	owned, err := acquireOwnedLock(t.Private, lockPath)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("%w: index.lock is held by a foreign process", ErrIndexCAS)
	}

	live, err := fileDigest(indexPath)
	if err != nil {
		return releaseOwnedLock(t.Private, lockPath, err)
	}
	if live == t.TargetDigest {
		// Already published (a crash after the replace) — idempotent success.
		return releaseOwnedLock(t.Private, lockPath, nil)
	}
	if live != t.PreDigest {
		// A foreign staged change appeared — never overwrite it.
		return releaseOwnedLock(t.Private, lockPath, fmt.Errorf("%w: the real index diverged from the frozen pre-identity", ErrIndexCAS))
	}
	// Atomically replace the index with the owned lock (which is the same file as the target).
	if err := atomicfile.Replace(lockPath, indexPath); err != nil {
		return releaseOwnedLock(t.Private, lockPath, fmt.Errorf("%w: publish: %v", ErrIndexCAS, err))
	}
	return nil
}

// ConfirmIndex re-proves the applied identity and forces the real index and its directory durable.
func (g *Git) ConfirmIndex(ctx context.Context, t IndexTarget) error {
	st, err := g.ObserveIndex(ctx, t)
	if err != nil {
		return err
	}
	if st != IndexAtTarget {
		return fmt.Errorf("%w: index is %s, not durably synced", ErrIndexCAS, st)
	}
	indexPath, err := g.realIndexPath(ctx, t.Worktree)
	if err != nil {
		return err
	}
	if err := confirmFileBarrier(indexPath); err != nil {
		return fmt.Errorf("gitx: confirm index content %s: %w", indexPath, err)
	}
	if err := confirmDirBarrier(indexPath); err != nil { // the admin dir entry
		return fmt.Errorf("gitx: confirm index entry %s: %w", indexPath, err)
	}
	// Keep the txn-private proof durable through terminality.
	if err := confirmFileBarrier(t.Private); err != nil {
		return fmt.Errorf("gitx: confirm target proof %s: %w", t.Private, err)
	}
	return nil
}

func (g *Git) realIndexPath(ctx context.Context, worktree string) (string, error) {
	admin, err := g.revParse(ctx, worktree, "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(admin, "index"), nil
}

// ConfirmPreState re-proves the frozen pre-transaction identity immediately before the
// journal PREPARE: the worktree HEAD is still at the frozen parent, the worktree
// content is still exactly the frozen tree, and the real checked-out index still has
// the frozen pre-digest I0. Any drift — a foreign staged change, a post-snapshot edit,
// a moved branch — fails closed BEFORE any journal record or effect exists, so a later
// index can never be adopted as the transaction's expected old identity.
func (g *Git) ConfirmPreState(ctx context.Context, worktree, parent, tree, preDigest string) error {
	head, err := g.revParse(ctx, worktree, "--verify", "HEAD")
	if err != nil {
		return err
	}
	if head != parent {
		return fmt.Errorf("%w: HEAD moved off the frozen parent before prepare", ErrIndexCAS)
	}
	wtTree, err := g.snapshotTree(ctx, worktree, parent)
	if err != nil {
		return err
	}
	if wtTree != tree {
		return fmt.Errorf("%w: the worktree changed after the snapshot", ErrIndexCAS)
	}
	indexPath, err := g.realIndexPath(ctx, worktree)
	if err != nil {
		return err
	}
	live, err := fileDigest(indexPath)
	if err != nil {
		return err
	}
	if live != preDigest {
		return fmt.Errorf("%w: the real index diverged from the frozen pre-identity before prepare", ErrIndexCAS)
	}
	return nil
}

// TargetIndexPath derives the txn-private target-index path for txnID: a sibling of the
// worktree's real index (so the hard-link CAS stays on one filesystem), named by the
// transaction id so recovery re-derives the exact journalled path and two transactions
// can never collide.
func (g *Git) TargetIndexPath(ctx context.Context, worktree, txnID string) (string, error) {
	indexPath, err := g.realIndexPath(ctx, worktree)
	if err != nil {
		return "", err
	}
	return indexPath + ".claudex-target-" + txnID, nil
}

// IndexDigest returns the digest of the worktree's real checked-out index — the I0 the
// transaction freezes before the index-cas step.
func (g *Git) IndexDigest(ctx context.Context, worktree string) (string, error) {
	indexPath, err := g.realIndexPath(ctx, worktree)
	if err != nil {
		return "", err
	}
	return fileDigest(indexPath)
}

func (g *Git) worktreeClean(ctx context.Context, worktree string) (bool, error) {
	out, err := g.Run(ctx, worktree, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return len(strings.TrimRight(string(out), "\x00")) == 0, nil
}

// acquireOwnedLock hard-links private onto lockPath. On success the two names are the same file. On
// EEXIST it adopts a leftover lock ONLY if same-file identity proves it is our private target;
// matching bytes without same-file identity are foreign (owned=false, the lock is untouched).
func acquireOwnedLock(private, lockPath string) (owned bool, err error) {
	if err := os.Link(private, lockPath); err == nil {
		return true, nil
	} else if !os.IsExist(err) {
		return false, err
	}
	same, err := sameFile(lockPath, private)
	if err != nil {
		return false, err
	}
	return same, nil
}

// releaseOwnedLock removes the same-file-proven owned lock (never a foreign one) and returns cause.
func releaseOwnedLock(private, lockPath string, cause error) error {
	if same, err := sameFile(lockPath, private); err == nil && same {
		_ = os.Remove(lockPath)
	}
	return cause
}

// sameFile reports whether two paths are the same filesystem object (dev+inode / file id), using
// Lstat so a symlink is never treated as the same file as a regular target.
func sameFile(a, b string) (bool, error) {
	fa, err := os.Lstat(a)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	fb, err := os.Lstat(b)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

func probeHardLink(near string) error {
	tmp := near + ".linkprobe"
	_ = os.Remove(tmp)
	if err := os.Link(near, tmp); err != nil {
		return err
	}
	return os.Remove(tmp)
}

func fileDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
