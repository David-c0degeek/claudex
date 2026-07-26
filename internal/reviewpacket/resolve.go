package reviewpacket

import (
	"context"
	"fmt"
	"os"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/state"
)

// ErrResolve means a packet recipe could not be resolved deterministically from the run: a frozen
// snapshot is unreadable, the source object cannot be proven, or a declared selector is absent from
// the source tree. Every one of these is an AUTHORING failure, decided before any durable effect, so
// the caller can refuse the transition with the Registry, journal, and RunState untouched.
var ErrResolve = fmt.Errorf("reviewpacket: cannot resolve the review-evidence recipe")

// maxSnapshotBytes bounds a frozen input snapshot read. It matches the task/policy contract ceiling
// (config.MaxContractBytes), so a snapshot that would not have parsed as a contract is refused
// before it is read into memory rather than after.
const maxSnapshotBytes = config.MaxContractBytes

// The in-manifest logical paths recorded for the frozen run inputs. These are not repository blobs,
// so they live in evidence.ReservedContextPrefix — the one namespace IsSelectorPath refuses, which is
// what makes a collision with a task-declared selector impossible by construction rather than a
// duplicate discovered late inside packet validation.
const (
	taskContextPath   = evidence.ReservedContextPrefix + "task.json"
	policyContextPath = evidence.ReservedContextPrefix + "policy.json"
)

// contextMode is the mode recorded for a materialized frozen input. They are ordinary file content,
// so they carry the ordinary regular-file mode.
const contextMode = "100644"

// Deps are the read-only sources a resolution reads. None is mutated.
type Deps struct {
	// RunDir is the absolute run directory; frozen input snapshots are read from it confined via
	// os.Root, so a forged relative path in state cannot escape the run.
	RunDir string
	// RepoDir is the repository whose COMMITTED objects supply repository content.
	RepoDir string
	// Git runs the object reads. It is never used to touch the worktree.
	Git *gitx.Git
	// Artifacts reads the accepted artifacts a review turn must see (the plan under critique, the
	// agreed plan, the implementation report). Required for every phase past PLAN_DRAFT.
	Artifacts ArtifactReader
}

// Resolve builds the fully-resolved, deterministic recipe for the read-only turn a transition is
// about to issue. It reads only committed Git objects and frozen run inputs — never the live
// worktree — so the resulting packet is byte-stable regardless of what the lead edits afterwards.
//
// Every failure it can return is deterministic and repeatable for a given run state: a caller runs
// it during locked authorization, BEFORE opening a transaction, so a bad selection refuses the turn
// rather than stranding a half-applied one.
func Resolve(ctx context.Context, d Deps, rs state.RunState, turnID string, phase state.Phase, pending *Pending) (evidence.Recipe, error) {
	src, err := resolveSource(ctx, d, rs)
	if err != nil {
		return evidence.Recipe{}, err
	}
	return ResolveAt(ctx, d, rs, turnID, phase, src, pending)
}

// ResolveAt is Resolve with the source object stated explicitly. The git commit transaction needs
// it: the commit a CHECKPOINT turn must review is the one that transaction is about to record, which
// is not yet in the run's accepted history, so deriving the source from state would bind the packet
// to the PREVIOUS accepted commit and show the reviewer the wrong tree.
func ResolveAt(ctx context.Context, d Deps, rs state.RunState, turnID string, phase state.Phase, src evidence.SourceObject, pending *Pending) (evidence.Recipe, error) {
	if state.RepoEditPhase(phase) {
		return evidence.Recipe{}, fmt.Errorf("%w: %s carries a worktree, not an evidence packet", ErrResolve, phase)
	}
	// The packet binds the COMPLETE source identity, so the pair must be PROVEN consistent, not
	// merely two well-formed ids: content is read from src.Commit while src.Tree is what the manifest
	// records, so an unchecked pair could publish bytes from one commit while binding another's tree.
	if err := proveSourcePair(ctx, d, src); err != nil {
		return evidence.Recipe{}, err
	}
	entries, err := phaseEntries(ctx, d, rs, phase, src, pending)
	if err != nil {
		return evidence.Recipe{}, err
	}

	lim := rs.EffectivePolicy.Limits
	return evidence.Recipe{
		RunID:   rs.RunID,
		TurnID:  turnID,
		Phase:   string(phase),
		Source:  src,
		Entries: entries,
		Bounds: evidence.Bounds{
			MaxTotalBytes: lim.EvidenceMaxTotalBytes,
			MaxFileBytes:  lim.EvidenceMaxFileBytes,
			MaxRequests:   lim.EvidenceMaxRequests,
		},
	}, nil
}

// ExpectedSource is the complete, PROVEN {commit, tree} a packet for this run state must be cut
// from. Before the first accepted implementation it is the run's BaseCommit; afterwards it is the
// latest ACCEPTED commit, so a reviewer always sees the state the run actually agreed to.
//
// It is exported because pull re-derives the same expectation when it re-verifies a bound packet.
// The proof is part of what is shared: the accepted tuple is persisted state, and persisted state is
// not evidence that the objects still exist and still form a pair. Without proving here, pull would
// accept a packet whose asserted source has since gone missing or become inconsistent in the object
// store, and Expectation only checks object-id grammar.
func ExpectedSource(ctx context.Context, d Deps, rs state.RunState) (evidence.SourceObject, error) {
	src, err := resolveSource(ctx, d, rs)
	if err != nil {
		return evidence.SourceObject{}, err
	}
	if err := proveSourcePair(ctx, d, src); err != nil {
		return evidence.SourceObject{}, err
	}
	return src, nil
}

func resolveSource(ctx context.Context, d Deps, rs state.RunState) (evidence.SourceObject, error) {
	if latest, ok := state.LatestGitCommit(rs); ok {
		// Returned as-is; ResolveAt proves the pair against the object store, so the accepted tuple
		// gets exactly the same proof a caller-supplied source does.
		return evidence.SourceObject{Commit: latest.Commit, Tree: latest.Tree}, nil
	}
	if rs.BaseCommit == "" {
		return evidence.SourceObject{}, fmt.Errorf("%w: run has no base commit", ErrResolve)
	}
	// Read the base commit's tree from the object store rather than inferring it; ResolveAt then
	// re-proves the pair, so this path and the accepted-tuple path are held to one rule.
	tree, err := commitTree(ctx, d, rs.BaseCommit)
	if err != nil {
		return evidence.SourceObject{}, err
	}
	return evidence.SourceObject{Commit: rs.BaseCommit, Tree: tree}, nil
}

// proveSourcePair requires src.Tree to be exactly the tree src.Commit points at. Both halves are
// checked for grammar first so a malformed id is never handed to git.
func proveSourcePair(ctx context.Context, d Deps, src evidence.SourceObject) error {
	if !evidence.IsObjectID(src.Commit) || !evidence.IsObjectID(src.Tree) {
		return fmt.Errorf("%w: source is not a pair of object ids", ErrResolve)
	}
	tree, err := commitTree(ctx, d, src.Commit)
	if err != nil {
		return err
	}
	if tree != src.Tree {
		return fmt.Errorf("%w: source commit %s names tree %s, not the bound %s", ErrResolve, src.Commit, tree, src.Tree)
	}
	return nil
}

// commitTree resolves a commit's tree from the object store.
func commitTree(ctx context.Context, d Deps, commit string) (string, error) {
	out, err := d.Git.Run(ctx, d.RepoDir, nil, "rev-parse", "--verify", "--quiet", commit+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("%w: commit %s has no readable tree: %v", ErrResolve, commit, err)
	}
	tree := string(out)
	if !evidence.IsObjectID(tree) {
		return "", fmt.Errorf("%w: commit %s resolved to a malformed tree id", ErrResolve, commit)
	}
	return tree, nil
}

// resolveBlob resolves one declared selector to its blob id and original mode in the source tree.
// It uses `ls-tree` rather than `rev-parse <commit>:<path>` so the ORIGINAL mode is read from the
// tree instead of assumed, and so a selector naming a directory is refused as the non-leaf it is.
func resolveBlob(ctx context.Context, d Deps, commit, sel string) (oid, mode string, err error) {
	// -z: NUL-terminated records, so a path with a newline or a quotable byte is returned verbatim
	// instead of being C-quoted. --full-tree: paths are root-relative regardless of cwd.
	out, rerr := d.Git.RunRaw(ctx, d.RepoDir, nil, "ls-tree", "-z", "--full-tree", commit, "--", sel)
	if rerr != nil {
		return "", "", fmt.Errorf("%w: reading the source tree: %v", ErrResolve, rerr)
	}
	rec, rerr := soleTreeRecord(out)
	if rerr != nil {
		// The selector is caller-supplied task content, so it is described by position, not echoed.
		return "", "", fmt.Errorf("%w: a declared relevant_repo_paths selector %v", ErrResolve, rerr)
	}
	if rec.objType != "blob" {
		return "", "", fmt.Errorf("%w: a declared relevant_repo_paths selector names a %s, not a file", ErrResolve, rec.objType)
	}
	if !evidence.IsObjectID(rec.oid) {
		return "", "", fmt.Errorf("%w: the source tree returned a malformed object id", ErrResolve)
	}
	if rec.path != sel {
		return "", "", fmt.Errorf("%w: a declared relevant_repo_paths selector did not resolve to an exact path", ErrResolve)
	}
	return rec.oid, rec.mode, nil
}

// readSnapshot reads a frozen run input and proves it is the EXACT bytes the run froze.
//
// SnapshotRef is defined as "relative local path plus the exact digest of the bytes", so the digest
// is the authority and the path alone is not: without the check, replacing a task snapshot with
// different but still-valid v2 JSON would change the declared selectors and therefore the packet,
// while immutable run state still named the old digest. The packet would be internally consistent
// and bound to a task the run never agreed to.
//
// The read is regular-only and bounded (atomicfile.ReadInRoot), not a plain rooted Open: an in-root
// symlink would otherwise be followed to another file, and a FIFO could block the resolution
// indefinitely.
func readSnapshot(runDir string, ref state.SnapshotRef, what string) ([]byte, error) {
	if ref.RelPath == "" {
		return nil, fmt.Errorf("%w: run has no %s snapshot", ErrResolve, what)
	}
	if !state.IsHex64(ref.Digest) {
		return nil, fmt.Errorf("%w: the %s snapshot digest is not a sha256", ErrResolve, what)
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// One byte past the ceiling, so an oversize snapshot is detected rather than silently truncated
	// into a digest mismatch that reads like tampering.
	data, err := atomicfile.ReadInRoot(root, ref.RelPath, maxSnapshotBytes+1)
	if err != nil {
		return nil, fmt.Errorf("%w: %s snapshot: %v", ErrResolve, what, err)
	}
	if len(data) > maxSnapshotBytes {
		return nil, fmt.Errorf("%w: %s snapshot exceeds %d bytes", ErrResolve, what, maxSnapshotBytes)
	}
	if got := config.Hash(data); got != ref.Digest {
		return nil, fmt.Errorf("%w: the %s snapshot does not match the digest the run froze", ErrResolve, what)
	}
	return data, nil
}
