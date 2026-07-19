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
	// ErrDirtySubmodule means a submodule has uncommitted or untracked content.
	ErrDirtySubmodule = errors.New("gitx: dirty or untracked submodule content")
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

// SnapshotReq is the frozen input to a snapshot-commit: only frozen run FACTS and the run identity
// the worktree must match — never a free-form message, author, or timestamp.
type SnapshotReq struct {
	RepoDir          string // the repository root (its .git is the expected common dir)
	Worktree         string // the run worktree directory (a LINKED worktree of RepoDir)
	Branch           string // the run branch short name; HEAD must be a symbolic ref to it
	Parent           string // the parent commit OID (the branch tip / base for the first)
	RunID            string // frozen run fact, for the plan-agnostic ASCII message
	StartingRevision uint64 // frozen run fact, for the message and the deterministic timestamp
	CreatedUnix      int64  // frozen run fact (run-created time), for the deterministic timestamp
}

// SnapshotCommit is AUTHORITY-NEUTRAL: `git commit-tree` writes into the object database, but this
// moves no ref, no durable state, and never the real checked-out index. It captures the exact
// worktree snapshot in a throwaway index and creates a commit from it, returning the commit and
// tree OIDs (which 04.2 freezes; recovery observes those and never recomputes). It fails closed on
// a wrong run worktree/branch/HEAD, a staged-only index divergence, a dirty submodule, a racing
// edit, or a no-op tree — and re-validates the durable-state invariants at the post-capture
// barrier so an edit interposed at the seam cannot slip a divergent index/submodule past.
func (g *Git) SnapshotCommit(ctx context.Context, req SnapshotReq) (commit, tree string, err error) {
	if err := validateSnapshotInputs(req); err != nil {
		return "", "", err
	}
	if err := g.validateRunWorktree(ctx, req); err != nil {
		return "", "", err
	}

	tree, err = g.snapshotTree(ctx, req.Worktree, req.Parent)
	if err != nil {
		return "", "", err
	}
	if err := g.rejectStagedOnlyDivergence(ctx, req.Worktree, tree); err != nil {
		return "", "", err
	}
	if err := g.rejectDirtySubmodules(ctx, req.Worktree, tree); err != nil {
		return "", "", err
	}

	if snapshotRaceHook != nil {
		if herr := snapshotRaceHook(); herr != nil {
			return "", "", herr
		}
	}

	tree2, err := g.snapshotTree(ctx, req.Worktree, req.Parent)
	if err != nil {
		return "", "", err
	}
	if tree != tree2 {
		return "", "", ErrRacingEdit
	}

	// Post-capture barrier: revalidate every durable-state invariant, not just the tree bytes, so
	// a submodule dirtied or a divergent index staged at the seam (both leaving the superproject
	// tree unchanged) is caught before the object's id escapes.
	if err := g.validateRunWorktree(ctx, req); err != nil {
		return "", "", err
	}
	if err := g.rejectStagedOnlyDivergence(ctx, req.Worktree, tree); err != nil {
		return "", "", err
	}
	if err := g.rejectDirtySubmodules(ctx, req.Worktree, tree); err != nil {
		return "", "", err
	}

	parentTree, err := g.revParse(ctx, req.Worktree, req.Parent+"^{tree}")
	if err != nil {
		return "", "", err
	}
	if tree == parentTree {
		return "", "", ErrEmptySnapshot
	}

	commit, err = g.commitTree(ctx, req, tree)
	if err != nil {
		return "", "", err
	}
	return commit, tree, nil
}

// validateSnapshotInputs enforces the frozen-metadata contract before it reaches git: a canonical
// single-line ASCII run id, and a timestamp derivation whose conversion, addition, and range are
// all checked (no uint64->int64 or signed-add overflow, and a git-supported value).
func validateSnapshotInputs(req SnapshotReq) error {
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

func isCanonicalRunID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// validateRunWorktree proves the supplied path is the EXPECTED run worktree: a LINKED worktree of
// RepoDir (not the main worktree, not a foreign repository), whose HEAD is a symbolic ref to the
// run branch, and whose HEAD and branch both resolve to exactly Parent.
func (g *Git) validateRunWorktree(ctx context.Context, req SnapshotReq) error {
	if req.RepoDir == "" || req.Worktree == "" || req.Branch == "" || req.Parent == "" {
		return fmt.Errorf("%w: repo, worktree, branch, and parent are required", ErrSnapshotWorktree)
	}
	top, err := g.revParse(ctx, req.Worktree, "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if !samePath(top, req.Worktree) {
		return fmt.Errorf("%w: %s is not the worktree root (%s)", ErrSnapshotWorktree, req.Worktree, top)
	}
	common, err := g.revParse(ctx, req.Worktree, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if !samePath(common, filepath.Join(req.RepoDir, ".git")) {
		return fmt.Errorf("%w: worktree belongs to a different repository (%s)", ErrSnapshotWorktree, common)
	}
	gitDir, err := g.revParse(ctx, req.Worktree, "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if samePath(gitDir, common) {
		return fmt.Errorf("%w: %s is the main worktree, not a linked run worktree", ErrSnapshotWorktree, req.Worktree)
	}
	sym, err := g.revParse(ctx, req.Worktree, "--symbolic-full-name", "HEAD")
	if err != nil {
		return fmt.Errorf("%w: HEAD is detached: %v", ErrSnapshotWorktree, err)
	}
	if sym != "refs/heads/"+req.Branch {
		return fmt.Errorf("%w: HEAD is %s, not the run branch %s", ErrSnapshotWorktree, sym, req.Branch)
	}
	head, err := g.revParse(ctx, req.Worktree, "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if head != req.Parent {
		return fmt.Errorf("%w: HEAD %s is not the expected parent %s", ErrSnapshotWorktree, head, req.Parent)
	}
	branchOID, err := g.revParse(ctx, req.Worktree, "--verify", "refs/heads/"+req.Branch)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if branchOID != req.Parent {
		return fmt.Errorf("%w: branch %s is at %s, not the expected parent %s", ErrSnapshotWorktree, req.Branch, branchOID, req.Parent)
	}
	return nil
}

// snapshotTree builds the exact worktree snapshot in a fresh THROWAWAY index (a temp directory
// with a NONEXISTENT index path) and returns its tree OID. It seeds from the parent with read-tree
// BEFORE `add -A` so parent knowledge (notably tracked-but-ignored entries) is preserved, runs the
// object-writing steps under the fsync contract so a frozen blob/tree cannot be lost, and never
// touches the real index.
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
// contract. Hooks and signing are already disabled by the hardened handle.
func (g *Git) commitTree(ctx context.Context, req SnapshotReq, tree string) (string, error) {
	date := fmt.Sprintf("@%d +0000", req.CreatedUnix+int64(req.StartingRevision))
	env := map[string]string{
		"GIT_AUTHOR_NAME": snapshotAuthorName, "GIT_AUTHOR_EMAIL": snapshotAuthorEmail,
		"GIT_COMMITTER_NAME": snapshotAuthorName, "GIT_COMMITTER_EMAIL": snapshotAuthorEmail,
		"GIT_AUTHOR_DATE": date, "GIT_COMMITTER_DATE": date,
	}
	msg := fmt.Sprintf("claudex snapshot: run %s revision %d", req.RunID, req.StartingRevision)
	args := fsyncArgs("-c", "i18n.commitEncoding=UTF-8", "commit-tree", tree, "-p", req.Parent, "-m", msg)
	out, err := g.Run(ctx, req.Worktree, env, args...)
	if err != nil {
		return "", err
	}
	return parseOID(string(out))
}

// rejectStagedOnlyDivergence fails closed when a staged change (real index != HEAD) would be
// discarded by the worktree snapshot. It compares, for every path changed HEAD -> real index, the
// index-side entry (mode+oid, or absence) against the captured tree's entry, so a staged edit that
// differs from the worktree AND a staged deletion of a retained file both fail. Rename/copy staging
// is rejected conservatively.
func (g *Git) rejectStagedOnlyDivergence(ctx context.Context, worktree, tree string) error {
	raw, err := g.Run(ctx, worktree, nil, "diff-index", "--cached", "-z", "HEAD")
	if err != nil {
		return err
	}
	toks := splitZ(raw)
	for i := 0; i < len(toks); {
		meta := toks[i]
		fields := strings.Fields(meta) // ":<om> <nm> <os> <ns> <status>"
		if len(fields) < 5 || !strings.HasPrefix(meta, ":") {
			return fmt.Errorf("%w: unparseable diff-index record %q", ErrStagedDivergence, meta)
		}
		dstMode, dstOID, status := fields[1], fields[3], fields[4]
		if status != "" && (status[0] == 'R' || status[0] == 'C') {
			return fmt.Errorf("%w: staged rename/copy %q", ErrStagedDivergence, status)
		}
		if i+1 >= len(toks) {
			return fmt.Errorf("%w: diff-index record missing path", ErrStagedDivergence)
		}
		path := toks[i+1]
		i += 2
		treeMode, treeOID, err := g.treeEntry(ctx, worktree, tree, path)
		if err != nil {
			return err
		}
		indexAbsent := dstMode == "000000"
		if indexAbsent {
			if treeMode != "" { // staged deletion, but the snapshot re-added the retained file
				return fmt.Errorf("%w: staged deletion of %s discarded", ErrStagedDivergence, path)
			}
			continue
		}
		if treeMode != dstMode || treeOID != dstOID {
			return fmt.Errorf("%w: staged %s (%s %s) not captured (%s %s)", ErrStagedDivergence, path, dstMode, dstOID, treeMode, treeOID)
		}
	}
	return nil
}

// treeEntry returns the (mode, oid) of path in tree, or ("","") if absent.
func (g *Git) treeEntry(ctx context.Context, worktree, tree, path string) (string, string, error) {
	out, err := g.Run(ctx, worktree, nil, "ls-tree", "-z", tree, "--", path)
	if err != nil {
		return "", "", err
	}
	rec := strings.TrimRight(string(out), "\x00")
	if rec == "" {
		return "", "", nil
	}
	// "<mode> <type> <oid>\t<path>"
	meta, _, _ := strings.Cut(rec, "\t")
	f := strings.Fields(meta)
	if len(f) < 3 {
		return "", "", fmt.Errorf("%w: unparseable ls-tree record %q", ErrStagedDivergence, rec)
	}
	return f[0], f[2], nil
}

// rejectDirtySubmodules fails closed on any submodule (a gitlink in the captured tree) whose
// worktree has uncommitted or untracked content. The gitlink paths come from the captured tree via
// NUL-safe `ls-tree -r -z`, so a path containing spaces or newlines is handled exactly.
func (g *Git) rejectDirtySubmodules(ctx context.Context, worktree, tree string) error {
	raw, err := g.Run(ctx, worktree, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return err
	}
	for _, rec := range splitZ(raw) {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			return fmt.Errorf("%w: unparseable ls-tree record %q", ErrDirtySubmodule, rec)
		}
		f := strings.Fields(meta)
		if len(f) < 3 {
			return fmt.Errorf("%w: unparseable ls-tree record %q", ErrDirtySubmodule, rec)
		}
		if f[0] != "160000" { // only gitlinks are submodules
			continue
		}
		sub := filepath.Join(worktree, filepath.FromSlash(path))
		st, serr := g.Run(ctx, sub, nil, "status", "--porcelain", "--untracked-files=all")
		if serr != nil {
			// An uninitialized submodule has no worktree to be dirty; treat a non-repo path as clean.
			if errors.Is(serr, ErrGit) {
				continue
			}
			return serr
		}
		if strings.TrimSpace(string(st)) != "" {
			return fmt.Errorf("%w: %s", ErrDirtySubmodule, path)
		}
	}
	return nil
}

// splitZ splits NUL-separated git output into non-empty fields.
func splitZ(b []byte) []string {
	s := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	if len(s) == 1 && s[0] == "" {
		return nil
	}
	return s
}

// revParse runs `git -C dir rev-parse <args...>` and returns the trimmed single-line output.
func (g *Git) revParse(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := g.Run(ctx, dir, nil, append([]string{"rev-parse"}, args...)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
