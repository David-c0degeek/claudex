package gitx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Snapshot-commit sentinels.
var (
	// ErrSnapshotWorktree means the worktree is not the expected run worktree (repo/branch/HEAD).
	ErrSnapshotWorktree = errors.New("gitx: snapshot worktree validation failed")
	// ErrSnapshotInput means a frozen input (run id or timestamp derivation) is not canonical.
	ErrSnapshotInput = errors.New("gitx: snapshot input validation failed")
	// ErrStagedDivergence means the real index holds a staged change the worktree snapshot would
	// discard (a staged edit differing from the worktree, or a staged deletion of a retained file).
	ErrStagedDivergence = errors.New("gitx: staged-only index divergence")
	// ErrDirtySubmodule means a submodule has uncommitted or untracked content, or a gitlink path
	// is occupied by content that is not a clean submodule checkout.
	ErrDirtySubmodule = errors.New("gitx: dirty or unexpected submodule content")
	// ErrRacingEdit means the worktree changed between capture and re-verification.
	ErrRacingEdit = errors.New("gitx: worktree changed during snapshot")
	// ErrEmptySnapshot means the snapshot tree equals the parent tree, so there is nothing to commit.
	ErrEmptySnapshot = errors.New("gitx: snapshot tree equals the parent (empty implementation)")
)

// Fixed, tool-owned commit identity: no ambient user/email/date/encoding ever enters an object.
const (
	snapshotAuthorName  = "claudex"
	snapshotAuthorEmail = "claudex@localhost"
	// maxSnapshotUnix bounds the timestamp derivation well within git's supported range and makes
	// the uint64->int64 conversion and the addition overflow-free.
	maxSnapshotUnix = int64(1) << 42
)

// snapshotRaceHook is a test-only seam invoked between the two independent captures, so a racing
// edit / staged change / submodule dirt can be injected deterministically to prove the barrier
// rejects it. Nil in production.
var snapshotRaceHook func() error

// SnapshotReq is the frozen input to a snapshot-commit. The run worktree path and run branch are
// DERIVED from RunID + RepoDir, never accepted as independent claims, so a caller cannot snapshot
// one run's worktree while labelling the object as another.
type SnapshotReq struct {
	RepoDir          string // the repository root
	Parent           string // the parent commit OID (the run branch tip / base for the first)
	RunID            string // frozen run identity; derives the worktree path and branch and the message
	StartingRevision uint64 // frozen run fact, for the message and the deterministic timestamp
	CreatedUnix      int64  // frozen run fact (run-created time), for the deterministic timestamp
}

// runBranch and runWorktreeRel are the canonical derivations shared with the attach layout; gitx
// enforces them so the snapshot identity cannot be cross-wired.
func runBranch(runID string) string      { return "claudex/" + runID }
func runWorktreeRel(runID string) string { return ".claudex/runs/" + runID + "/worktree" }
func runWorktreeAbs(repo, runID string) string {
	return filepath.Join(repo, filepath.FromSlash(runWorktreeRel(runID)))
}

// SnapshotCommit is AUTHORITY-NEUTRAL: `git commit-tree` writes into the object database, but this
// moves no ref, no durable state, and never the real checked-out index. It captures the exact
// worktree snapshot in a throwaway index and creates a commit from it, returning the commit and
// tree OIDs (which 04.2 freezes; recovery observes those and never recomputes). It fails closed on
// a wrong run worktree/branch/HEAD, a staged-only index divergence, a dirty/unexpected submodule, a
// racing edit, or a no-op tree — re-validating the durable-state invariants at the post-capture
// barrier so an edit interposed at the seam cannot slip a divergent index/submodule past.
func (g *Git) SnapshotCommit(ctx context.Context, req SnapshotReq) (commit, tree string, err error) {
	if err := validateSnapshotInputs(req); err != nil {
		return "", "", err
	}
	wt := runWorktreeAbs(req.RepoDir, req.RunID)
	branch := runBranch(req.RunID)
	if err := g.validateRunWorktree(ctx, req, wt, branch); err != nil {
		return "", "", err
	}

	tree, err = g.snapshotTree(ctx, wt, req.Parent)
	if err != nil {
		return "", "", err
	}
	treeMap, err := g.captureTreeEntries(ctx, wt, tree)
	if err != nil {
		return "", "", err
	}
	if err := g.rejectStagedOnlyDivergence(ctx, wt, treeMap); err != nil {
		return "", "", err
	}
	if err := g.rejectDirtySubmodules(ctx, wt, treeMap); err != nil {
		return "", "", err
	}

	if snapshotRaceHook != nil {
		if herr := snapshotRaceHook(); herr != nil {
			return "", "", herr
		}
	}

	tree2, err := g.snapshotTree(ctx, wt, req.Parent)
	if err != nil {
		return "", "", err
	}
	if tree != tree2 {
		return "", "", ErrRacingEdit
	}

	// Post-capture barrier: revalidate every durable-state invariant (identity, staged index,
	// submodules) against the captured tree, so a submodule dirtied or a divergent index staged at
	// the seam — both leaving the superproject tree unchanged — is caught before the id escapes.
	if err := g.validateRunWorktree(ctx, req, wt, branch); err != nil {
		return "", "", err
	}
	if err := g.rejectStagedOnlyDivergence(ctx, wt, treeMap); err != nil {
		return "", "", err
	}
	if err := g.rejectDirtySubmodules(ctx, wt, treeMap); err != nil {
		return "", "", err
	}

	parentTree, err := g.revParse(ctx, wt, req.Parent+"^{tree}")
	if err != nil {
		return "", "", err
	}
	if tree == parentTree {
		return "", "", ErrEmptySnapshot
	}

	commit, err = g.commitTree(ctx, req, wt, tree)
	if err != nil {
		return "", "", err
	}
	return commit, tree, nil
}

// validateSnapshotInputs enforces the frozen-metadata contract before it reaches git: a canonical
// run id (state's grammar: alphanumeric-first, no dot-name) that is also single-line ASCII, and a
// timestamp derivation whose conversion, addition, and range are all checked.
func validateSnapshotInputs(req SnapshotReq) error {
	if req.RepoDir == "" || req.Parent == "" {
		return fmt.Errorf("%w: repo and parent are required", ErrSnapshotInput)
	}
	if !isCanonicalRunID(req.RunID) {
		return fmt.Errorf("%w: run id is not canonical", ErrSnapshotInput)
	}
	if req.CreatedUnix <= 0 || req.CreatedUnix > maxSnapshotUnix {
		return fmt.Errorf("%w: created_unix out of range", ErrSnapshotInput)
	}
	if req.StartingRevision > uint64(maxSnapshotUnix) {
		return fmt.Errorf("%w: starting_revision out of range", ErrSnapshotInput)
	}
	ts := req.CreatedUnix + int64(req.StartingRevision)
	if ts < req.CreatedUnix || ts > math.MaxInt64-1 {
		return fmt.Errorf("%w: derived timestamp overflow", ErrSnapshotInput)
	}
	return nil
}

// isCanonicalRunID matches state's run-id grammar: 1..128 bytes, not "." or "..", the first byte
// alphanumeric, and every byte in [A-Za-z0-9._-].
func isCanonicalRunID(s string) bool {
	if len(s) == 0 || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	if c := s[0]; !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// validateRunWorktree proves the DERIVED run worktree/branch match the checked-out git structure:
// a LINKED worktree of RepoDir (not the main worktree, not a foreign repository) at the derived
// path, whose HEAD is a symbolic ref to the derived run branch, and whose HEAD and branch both
// resolve to exactly Parent.
func (g *Git) validateRunWorktree(ctx context.Context, req SnapshotReq, wt, branch string) error {
	top, err := g.revParse(ctx, wt, "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if !samePath(top, wt) {
		return fmt.Errorf("%w: %s is not the derived run worktree root (%s)", ErrSnapshotWorktree, wt, top)
	}
	common, err := g.revParse(ctx, wt, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if !samePath(common, filepath.Join(req.RepoDir, ".git")) {
		return fmt.Errorf("%w: worktree belongs to a different repository (%s)", ErrSnapshotWorktree, common)
	}
	gitDir, err := g.revParse(ctx, wt, "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if samePath(gitDir, common) {
		return fmt.Errorf("%w: %s is the main worktree, not a linked run worktree", ErrSnapshotWorktree, wt)
	}
	sym, err := g.revParse(ctx, wt, "--symbolic-full-name", "HEAD")
	if err != nil {
		return fmt.Errorf("%w: HEAD is detached: %v", ErrSnapshotWorktree, err)
	}
	if sym != "refs/heads/"+branch {
		return fmt.Errorf("%w: HEAD is %s, not the run branch %s", ErrSnapshotWorktree, sym, branch)
	}
	head, err := g.revParse(ctx, wt, "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if head != req.Parent {
		return fmt.Errorf("%w: HEAD %s is not the expected parent %s", ErrSnapshotWorktree, head, req.Parent)
	}
	branchOID, err := g.revParse(ctx, wt, "--verify", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if branchOID != req.Parent {
		return fmt.Errorf("%w: branch %s is at %s, not the expected parent %s", ErrSnapshotWorktree, branch, branchOID, req.Parent)
	}
	return nil
}

// snapshotTree builds the exact worktree snapshot in a fresh THROWAWAY index (a temp directory
// with a NONEXISTENT index path) and returns its tree OID. It seeds from the parent with read-tree
// BEFORE `add -A` so parent knowledge is preserved, runs the object-writing steps under the fsync
// contract so a frozen blob/tree cannot be lost, and never touches the real index.
func (g *Git) snapshotTree(ctx context.Context, worktree, parent string) (string, error) {
	tmp, err := os.MkdirTemp("", "claudex-snap-idx-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := map[string]string{"GIT_INDEX_FILE": filepath.Join(tmp, "index")} // not created: read-tree makes it
	if _, err := g.Run(ctx, worktree, env, "read-tree", parent); err != nil {
		return "", err
	}
	if _, err := g.Run(ctx, worktree, env, fsyncArgs("add", "-A", "--", ".")...); err != nil {
		return "", err
	}
	out, err := g.Run(ctx, worktree, env, fsyncArgs("write-tree")...)
	if err != nil {
		return "", err
	}
	return parseOID(string(out))
}

// commitTree writes the commit object with the fixed tool identity, a plan-agnostic ASCII message
// derived from the starting revision, one deterministic checked UTC timestamp (author==committer),
// a forced UTF-8 commit encoding (so ambient i18n.commitEncoding adds no header), and the fsync
// contract.
func (g *Git) commitTree(ctx context.Context, req SnapshotReq, worktree, tree string) (string, error) {
	date := fmt.Sprintf("@%d +0000", req.CreatedUnix+int64(req.StartingRevision))
	env := map[string]string{
		"GIT_AUTHOR_NAME": snapshotAuthorName, "GIT_AUTHOR_EMAIL": snapshotAuthorEmail,
		"GIT_COMMITTER_NAME": snapshotAuthorName, "GIT_COMMITTER_EMAIL": snapshotAuthorEmail,
		"GIT_AUTHOR_DATE": date, "GIT_COMMITTER_DATE": date,
	}
	msg := fmt.Sprintf("claudex snapshot: run %s revision %d", req.RunID, req.StartingRevision)
	args := fsyncArgs("-c", "i18n.commitEncoding=UTF-8", "commit-tree", tree, "-p", req.Parent, "-m", msg)
	out, err := g.Run(ctx, worktree, env, args...)
	if err != nil {
		return "", err
	}
	return parseOID(string(out))
}

// treeEnt is a captured tree entry.
type treeEnt struct{ mode, oid string }

// captureTreeEntries parses the full captured tree into an exact byte-path -> entry map via a
// STREAMED `ls-tree -r -z` (no output cap, so a large tree is never silently truncated). The map
// is looked up by exact path, avoiding both a per-path subprocess and pathspec metacharacter
// misinterpretation.
func (g *Git) captureTreeEntries(ctx context.Context, worktree, tree string) (map[string]treeEnt, error) {
	recs, err := g.RunNulRecords(ctx, worktree, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, err
	}
	m := make(map[string]treeEnt, len(recs))
	for _, rec := range recs {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			return nil, fmt.Errorf("gitx: unparseable ls-tree record %q", rec)
		}
		f := strings.Fields(meta) // "<mode> <type> <oid>"
		if len(f) < 3 {
			return nil, fmt.Errorf("gitx: unparseable ls-tree record %q", rec)
		}
		m[path] = treeEnt{mode: f[0], oid: f[2]}
	}
	return m, nil
}

// rejectStagedOnlyDivergence fails closed when a staged change (real index != HEAD) would be
// discarded by the worktree snapshot. For every path changed HEAD -> real index it compares the
// index-side entry (mode+oid, or absence) against the captured tree's entry (an exact byte-path
// lookup), so a staged edit differing from the worktree AND a staged deletion of a retained file
// both fail. Rename/copy staging is rejected conservatively.
func (g *Git) rejectStagedOnlyDivergence(ctx context.Context, worktree string, treeMap map[string]treeEnt) error {
	recs, err := g.RunNulRecords(ctx, worktree, nil, "diff-index", "--cached", "-z", "HEAD")
	if err != nil {
		return err
	}
	for i := 0; i < len(recs); {
		meta := recs[i]
		f := strings.Fields(meta) // ":<om> <nm> <os> <ns> <status>"
		if len(f) < 5 || !strings.HasPrefix(meta, ":") {
			return fmt.Errorf("%w: unparseable diff-index record %q", ErrStagedDivergence, meta)
		}
		dstMode, dstOID, status := f[1], f[3], f[4]
		if status != "" && (status[0] == 'R' || status[0] == 'C') {
			return fmt.Errorf("%w: staged rename/copy %q", ErrStagedDivergence, status)
		}
		if i+1 >= len(recs) {
			return fmt.Errorf("%w: diff-index record missing path", ErrStagedDivergence)
		}
		path := recs[i+1]
		i += 2
		ent, present := treeMap[path]
		if dstMode == "000000" { // staged deletion: the captured tree must also lack the path
			if present {
				return fmt.Errorf("%w: staged deletion of %s discarded", ErrStagedDivergence, path)
			}
			continue
		}
		if !present || ent.mode != dstMode || ent.oid != dstOID {
			return fmt.Errorf("%w: staged %s (%s %s) not captured (%s %s)", ErrStagedDivergence, path, dstMode, dstOID, ent.mode, ent.oid)
		}
	}
	return nil
}

// rejectDirtySubmodules fails closed on any gitlink whose worktree path holds uncommitted or
// untracked content, or content that is not a clean submodule checkout. Ownership is decided
// STRUCTURALLY, never by collapsing a generic git error into "uninitialized": an absent path or an
// exact empty directory is a clean, uninitialized mount; a non-directory node, or a populated path
// for which `git status` fails or is non-empty, fails closed.
func (g *Git) rejectDirtySubmodules(ctx context.Context, worktree string, treeMap map[string]treeEnt) error {
	for path, ent := range treeMap {
		if ent.mode != "160000" {
			continue
		}
		sub := filepath.Join(worktree, filepath.FromSlash(path))
		fi, err := os.Lstat(sub)
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent: uninitialized, nothing to be dirty
			}
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrDirtySubmodule, path)
		}
		entries, err := os.ReadDir(sub)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			continue // empty mount point: uninitialized
		}
		// Populated: it MUST be a clean submodule checkout — a status failure (not a repository,
		// corrupt metadata) or a non-empty status both fail closed.
		st, serr := g.Run(ctx, sub, nil, "status", "--porcelain", "--untracked-files=all")
		if serr != nil {
			return fmt.Errorf("%w: %s: not a clean submodule checkout: %v", ErrDirtySubmodule, path, serr)
		}
		if strings.TrimSpace(string(st)) != "" {
			return fmt.Errorf("%w: %s", ErrDirtySubmodule, path)
		}
	}
	return nil
}

// revParse runs `git -C dir rev-parse <args...>` and returns the trimmed single-line output.
func (g *Git) revParse(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := g.Run(ctx, dir, nil, append([]string{"rev-parse"}, args...)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
