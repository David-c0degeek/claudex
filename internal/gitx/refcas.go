package gitx

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrRefCAS is the sentinel a ref-CAS failure (foreign ref, bad commit object) wraps.
var ErrRefCAS = errors.New("gitx: ref CAS failed")

// RefState classifies the run branch against a frozen ref target.
type RefState int

const (
	// RefAtParent: the branch is still at the expected old head — the CAS has not run.
	RefAtParent RefState = iota
	// RefAtCommit: the branch is at the frozen new commit — the CAS is applied.
	RefAtCommit
	// RefForeign: the branch is missing or at some other commit — fail closed, never overwrite.
	RefForeign
)

func (s RefState) String() string {
	switch s {
	case RefAtParent:
		return "at-parent"
	case RefAtCommit:
		return "at-commit"
	default:
		return "foreign"
	}
}

// RefTarget is the frozen identity of a ref-CAS: move the run branch from Parent (the expected old
// head) to Commit, whose object must be an exact commit with tree Tree and sole parent Parent.
type RefTarget struct {
	Branch string // the run branch short name, e.g. "claudex/<id>"
	Parent string // the expected old head (the CAS old-value) and the commit's sole parent
	Tree   string // the commit's tree OID (re-proved, not trusted)
	Commit string // the new commit OID
}

// ObserveRef classifies the branch: at Commit (Applied), still at Parent (NotApplied), or anything
// else — missing or a different commit — foreign (fail closed).
func (g *Git) ObserveRef(ctx context.Context, repoDir string, t RefTarget) (RefState, error) {
	oid, err := g.branchOID(ctx, repoDir, t.Branch)
	if err != nil {
		return RefForeign, err
	}
	switch oid {
	case t.Commit:
		return RefAtCommit, nil
	case t.Parent:
		return RefAtParent, nil
	default:
		return RefForeign, nil
	}
}

// ApplyRef re-proves the frozen commit object, then CAS-moves the branch from Parent to Commit
// under the fsync contract. It is idempotent: a branch already at Commit is a no-op. It never moves
// a branch that is not exactly at Parent.
func (g *Git) ApplyRef(ctx context.Context, repoDir string, t RefTarget) error {
	oid, err := g.branchOID(ctx, repoDir, t.Branch)
	if err != nil {
		return err
	}
	if oid == t.Commit {
		return nil // already applied
	}
	if oid != t.Parent {
		return fmt.Errorf("%w: branch %q is at %s, not the expected parent %s", ErrRefCAS, t.Branch, shortOID(oid), shortOID(t.Parent))
	}
	if err := g.proveCommitObject(ctx, repoDir, t); err != nil {
		return err
	}
	// update-ref with the old value is the atomic CAS: it moves only if the branch is still Parent.
	if _, err := g.Run(ctx, repoDir, nil, fsyncArgs("update-ref", "refs/heads/"+t.Branch, t.Commit, t.Parent)...); err != nil {
		return fmt.Errorf("%w: %v", ErrRefCAS, err)
	}
	return nil
}

// ConfirmRef re-proves the branch is at the frozen commit and the object is exact, then forces the
// loose ref and its directory chain durable (a real ref durability barrier, not Observe).
func (g *Git) ConfirmRef(ctx context.Context, repoDir string, t RefTarget) error {
	oid, err := g.branchOID(ctx, repoDir, t.Branch)
	if err != nil {
		return err
	}
	if oid != t.Commit {
		return fmt.Errorf("%w: branch %q is %s, not the applied commit %s", ErrRefCAS, t.Branch, shortOID(oid), shortOID(t.Commit))
	}
	if err := g.proveCommitObject(ctx, repoDir, t); err != nil {
		return err
	}
	common, err := g.revParse(ctx, repoDir, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	refFile := filepath.Join(common, "refs", "heads", filepath.FromSlash(t.Branch))
	if err := confirmFileBarrier(refFile); err != nil { // loose ref content (a no-op if packed)
		return fmt.Errorf("gitx: confirm ref content %s: %w", refFile, err)
	}
	headsDir := filepath.Join(common, "refs", "heads")
	for p := refFile; ; p = filepath.Dir(p) {
		if err := confirmDirBarrier(p); err != nil {
			return fmt.Errorf("gitx: confirm ref entry %s: %w", p, err)
		}
		if samePath(filepath.Dir(p), headsDir) {
			break
		}
	}
	return nil
}

// proveCommitObject re-proves that Commit is an exact commit object whose tree is exactly Tree and
// whose parent list is exactly [Parent] — branch==Commit alone is not object evidence.
func (g *Git) proveCommitObject(ctx context.Context, repoDir string, t RefTarget) error {
	typ, err := g.Run(ctx, repoDir, nil, "cat-file", "-t", t.Commit)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefCAS, err)
	}
	if strings.TrimSpace(string(typ)) != "commit" {
		return fmt.Errorf("%w: %s is not a commit object", ErrRefCAS, shortOID(t.Commit))
	}
	tree, err := g.revParse(ctx, repoDir, t.Commit+"^{tree}")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefCAS, err)
	}
	if tree != t.Tree {
		return fmt.Errorf("%w: commit tree %s != frozen tree %s", ErrRefCAS, shortOID(tree), shortOID(t.Tree))
	}
	body, err := g.Run(ctx, repoDir, nil, "cat-file", "commit", t.Commit)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefCAS, err)
	}
	var parents []string
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" { // the header ends at the blank line before the message
			break
		}
		if rest, ok := strings.CutPrefix(line, "parent "); ok {
			parents = append(parents, strings.TrimSpace(rest))
		}
	}
	if len(parents) != 1 || parents[0] != t.Parent {
		return fmt.Errorf("%w: commit parents %v != exactly [%s]", ErrRefCAS, parents, shortOID(t.Parent))
	}
	return nil
}

// branchOID returns the exact OID of refs/heads/<branch>, or "" if the branch does not exist.
func (g *Git) branchOID(ctx context.Context, repoDir, branch string) (string, error) {
	out, err := g.Run(ctx, repoDir, nil, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", nil
	}
	return parseOID(s)
}
