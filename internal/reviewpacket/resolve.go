package reviewpacket

import (
	"context"
	"fmt"
	"io"
	"os"

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

// contextPath is the in-manifest repository path recorded for a frozen run input. These are not
// repository blobs, so they get reserved paths under a namespace no real selector can collide with:
// a selector is validated by evidence.IsSelectorPath, which rejects the leading dot segment that
// makes these names distinct.
const (
	taskContextPath   = ".claudex/task.json"
	policyContextPath = ".claudex/policy.json"
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
}

// Resolve builds the fully-resolved, deterministic recipe for the read-only turn a transition is
// about to issue. It reads only committed Git objects and frozen run inputs — never the live
// worktree — so the resulting packet is byte-stable regardless of what the lead edits afterwards.
//
// Every failure it can return is deterministic and repeatable for a given run state: a caller runs
// it during locked authorization, BEFORE opening a transaction, so a bad selection refuses the turn
// rather than stranding a half-applied one.
func Resolve(ctx context.Context, d Deps, rs state.RunState, turnID string, phase state.Phase) (evidence.Recipe, error) {
	src, err := resolveSource(ctx, d, rs)
	if err != nil {
		return evidence.Recipe{}, err
	}
	return ResolveAt(ctx, d, rs, turnID, phase, src)
}

// ResolveAt is Resolve with the source object stated explicitly. The git commit transaction needs
// it: the commit a CHECKPOINT turn must review is the one that transaction is about to record, which
// is not yet in the run's accepted history, so deriving the source from state would bind the packet
// to the PREVIOUS accepted commit and show the reviewer the wrong tree.
func ResolveAt(ctx context.Context, d Deps, rs state.RunState, turnID string, phase state.Phase, src evidence.SourceObject) (evidence.Recipe, error) {
	if state.RepoEditPhase(phase) {
		return evidence.Recipe{}, fmt.Errorf("%w: %s carries a worktree, not an evidence packet", ErrResolve, phase)
	}
	if !evidence.IsObjectID(src.Commit) || !evidence.IsObjectID(src.Tree) {
		return evidence.Recipe{}, fmt.Errorf("%w: source is not a pair of proven object ids", ErrResolve)
	}
	taskBytes, err := readSnapshot(d.RunDir, rs.TaskSnapshot, "task")
	if err != nil {
		return evidence.Recipe{}, err
	}
	policyBytes, err := readSnapshot(d.RunDir, rs.PolicySnapshot, "policy")
	if err != nil {
		return evidence.Recipe{}, err
	}
	tc, err := config.ParseTaskContract(taskBytes)
	if err != nil {
		return evidence.Recipe{}, fmt.Errorf("%w: frozen task snapshot: %v", ErrResolve, err)
	}

	// The frozen inputs come first: they define what the turn is FOR, and they are materialized
	// inline because they are already durable run content, not repository objects.
	entries := []evidence.RecipeEntry{
		{GitPath: taskContextPath, Mode: contextMode, Kind: evidence.EntryFile, Source: evidence.BlobSource{Inline: taskBytes}},
		{GitPath: policyContextPath, Mode: contextMode, Kind: evidence.EntryFile, Source: evidence.BlobSource{Inline: policyBytes}},
	}
	// Then the task-declared repository selection, read from the PROVEN source object. A selector the
	// source tree does not contain fails closed: silently dropping it would hand the reviewer a packet
	// that is quietly missing what the task said mattered.
	for _, sel := range tc.RelevantRepoPaths {
		oid, mode, rerr := resolveBlob(ctx, d, src.Commit, sel)
		if rerr != nil {
			return evidence.Recipe{}, rerr
		}
		entries = append(entries, evidence.RecipeEntry{
			GitPath: sel, Mode: mode, Kind: evidence.EntryFile,
			Source: evidence.BlobSource{CommitBlobOID: oid},
		})
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

// resolveSource proves the complete {commit, tree} the packet is cut from. Before the first accepted
// implementation the source is the run's BaseCommit; afterwards it is the latest ACCEPTED commit, so
// a reviewer always sees the state the run actually agreed to, never an unaccepted one.
func resolveSource(ctx context.Context, d Deps, rs state.RunState) (evidence.SourceObject, error) {
	if latest, ok := state.LatestGitCommit(rs); ok {
		if !evidence.IsObjectID(latest.Commit) || !evidence.IsObjectID(latest.Tree) {
			return evidence.SourceObject{}, fmt.Errorf("%w: accepted git evidence is not a pair of object ids", ErrResolve)
		}
		return evidence.SourceObject{Commit: latest.Commit, Tree: latest.Tree}, nil
	}
	if rs.BaseCommit == "" {
		return evidence.SourceObject{}, fmt.Errorf("%w: run has no base commit", ErrResolve)
	}
	// Resolve AND prove the base commit's tree rather than assuming it: the packet binds the complete
	// source identity, so the tree is read from the object store, not inferred.
	tree, err := d.Git.Run(ctx, d.RepoDir, nil, "rev-parse", "--verify", "--quiet", rs.BaseCommit+"^{tree}")
	if err != nil {
		return evidence.SourceObject{}, fmt.Errorf("%w: base commit %s has no readable tree: %v", ErrResolve, rs.BaseCommit, err)
	}
	src := evidence.SourceObject{Commit: rs.BaseCommit, Tree: string(tree)}
	if !evidence.IsObjectID(src.Commit) || !evidence.IsObjectID(src.Tree) {
		return evidence.SourceObject{}, fmt.Errorf("%w: base source is not a pair of object ids", ErrResolve)
	}
	return src, nil
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

// readSnapshot reads a frozen run input, confined to the run directory and bounded. It only asserts
// presence and size; the packet binds the exact bytes, and state independently owns the digest.
func readSnapshot(runDir string, ref state.SnapshotRef, what string) ([]byte, error) {
	if ref.RelPath == "" {
		return nil, fmt.Errorf("%w: run has no %s snapshot", ErrResolve, what)
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(ref.RelPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s snapshot: %v", ErrResolve, what, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s snapshot: %v", ErrResolve, what, err)
	}
	if len(data) > maxSnapshotBytes {
		return nil, fmt.Errorf("%w: %s snapshot exceeds %d bytes", ErrResolve, what, maxSnapshotBytes)
	}
	return data, nil
}
