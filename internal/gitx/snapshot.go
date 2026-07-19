package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Snapshot-commit sentinels.
var (
	// ErrSnapshotWorktree means the worktree is not the expected run worktree at the parent commit.
	ErrSnapshotWorktree = errors.New("gitx: snapshot worktree validation failed")
	// ErrStagedDivergence means the real index holds a staged change that differs from the worktree
	// and would be silently discarded by the worktree snapshot; the snapshot fails closed.
	ErrStagedDivergence = errors.New("gitx: staged-only index divergence")
	// ErrDirtySubmodule means a submodule has uncommitted or untracked content that the
	// superproject tree cannot represent (it captures only a clean gitlink commit).
	ErrDirtySubmodule = errors.New("gitx: dirty or untracked submodule content")
	// ErrRacingEdit means the worktree changed between capture and re-verification.
	ErrRacingEdit = errors.New("gitx: worktree changed during snapshot")
	// ErrEmptySnapshot means the snapshot tree equals the parent tree, so there is nothing to commit.
	ErrEmptySnapshot = errors.New("gitx: snapshot tree equals the parent (empty implementation)")
)

// Fixed, tool-owned commit identity: no ambient user/email/date ever enters a claudex object.
const (
	snapshotAuthorName  = "claudex"
	snapshotAuthorEmail = "claudex@localhost"
)

// snapshotRaceHook is a test-only seam invoked between the two independent captures, so a racing
// edit can be injected deterministically to prove the re-verification rejects it. Nil in production.
var snapshotRaceHook func() error

// SnapshotReq is the frozen input to a snapshot-commit. It carries only frozen run FACTS — never a
// free-form message, author, or timestamp — so neither ambient git config nor plan text can enter
// the object, and the derived metadata is deterministic.
type SnapshotReq struct {
	Worktree         string // the run worktree directory (its HEAD must equal Parent)
	Parent           string // the parent commit OID (the run branch tip / base for the first)
	RunID            string // frozen run fact, for the plan-agnostic message
	StartingRevision uint64 // frozen run fact, for the message and the deterministic timestamp
	CreatedUnix      int64  // frozen run fact (run-created time), for the deterministic timestamp
}

// SnapshotCommit is AUTHORITY-NEUTRAL: `git commit-tree` writes into the object database, but this
// moves no ref, no state, and never the real checked-out index. It builds the exact worktree
// snapshot in a throwaway index and creates a commit from it, returning the commit and tree OIDs
// (which 04.2 freezes; recovery observes those frozen ids and never recomputes them). It fails
// closed on a staged-only index divergence, a dirty submodule, a racing edit, or a no-op tree.
func (g *Git) SnapshotCommit(ctx context.Context, req SnapshotReq) (commit, tree string, err error) {
	if req.Parent == "" || req.Worktree == "" {
		return "", "", fmt.Errorf("%w: worktree and parent are required", ErrSnapshotWorktree)
	}
	if err := g.validateSnapshotWorktree(ctx, req); err != nil {
		return "", "", err
	}
	if err := g.rejectStagedOnlyDivergence(ctx, req.Worktree); err != nil {
		return "", "", err
	}
	if err := g.rejectDirtySubmodules(ctx, req.Worktree); err != nil {
		return "", "", err
	}

	// Capture the worktree twice through independent throwaway indexes; a racing edit between them
	// changes the content-addressed tree, so requiring equality is a deterministic race gate.
	tree, err = g.snapshotTree(ctx, req.Worktree, req.Parent)
	if err != nil {
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

	// Reject a no-op tree: an implementation commit that changes nothing is meaningless and would
	// wedge the phase machine.
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

// validateSnapshotWorktree proves the worktree is a git worktree ROOT and its HEAD is exactly the
// expected parent, so a snapshot never runs against a subdirectory, a foreign checkout, or a
// worktree that has moved off the parent.
func (g *Git) validateSnapshotWorktree(ctx context.Context, req SnapshotReq) error {
	top, err := g.revParse(ctx, req.Worktree, "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if !samePath(top, req.Worktree) {
		return fmt.Errorf("%w: %s is not the worktree root (%s)", ErrSnapshotWorktree, req.Worktree, top)
	}
	head, err := g.Run(ctx, req.Worktree, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotWorktree, err)
	}
	if strings.TrimSpace(string(head)) != req.Parent {
		return fmt.Errorf("%w: HEAD %s is not the expected parent %s", ErrSnapshotWorktree, strings.TrimSpace(string(head)), req.Parent)
	}
	return nil
}

// snapshotTree builds the exact worktree snapshot in a fresh THROWAWAY index (a temp directory
// with a NONEXISTENT index path — a pre-created zero-byte index is invalid) and returns its tree
// OID. It seeds from the parent with read-tree BEFORE `add -A` so parent knowledge (notably
// tracked-but-ignored entries and index metadata) is preserved, and never touches the real index.
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
	if _, err := g.Run(ctx, worktree, env, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	out, err := g.Run(ctx, worktree, env, "write-tree")
	if err != nil {
		return "", err
	}
	return parseOID(string(out))
}

// commitTree writes the commit object from tree and parent with the fixed tool identity, a
// plan-agnostic ASCII message derived from the starting revision, and one deterministic UTC
// timestamp (author == committer). Hooks and signing are already disabled by the hardened handle.
func (g *Git) commitTree(ctx context.Context, req SnapshotReq, tree string) (string, error) {
	date := fmt.Sprintf("@%d +0000", req.CreatedUnix+int64(req.StartingRevision))
	env := map[string]string{
		"GIT_AUTHOR_NAME": snapshotAuthorName, "GIT_AUTHOR_EMAIL": snapshotAuthorEmail,
		"GIT_COMMITTER_NAME": snapshotAuthorName, "GIT_COMMITTER_EMAIL": snapshotAuthorEmail,
		"GIT_AUTHOR_DATE": date, "GIT_COMMITTER_DATE": date,
	}
	msg := fmt.Sprintf("claudex snapshot: run %s revision %d", req.RunID, req.StartingRevision)
	out, err := g.Run(ctx, req.Worktree, env, "commit-tree", tree, "-p", req.Parent, "-m", msg)
	if err != nil {
		return "", err
	}
	return parseOID(string(out))
}

// rejectStagedOnlyDivergence fails closed when the real index holds a staged change (index != HEAD)
// at a path whose worktree content also differs from the index — that staged version would be
// silently discarded by the worktree snapshot, so it must not proceed.
func (g *Git) rejectStagedOnlyDivergence(ctx context.Context, worktree string) error {
	staged, err := g.Run(ctx, worktree, nil, "diff-index", "--cached", "--name-only", "-z", "HEAD")
	if err != nil {
		return err
	}
	unstaged, err := g.Run(ctx, worktree, nil, "diff-files", "--name-only", "-z")
	if err != nil {
		return err
	}
	stagedSet := map[string]struct{}{}
	for _, p := range splitZ(staged) {
		stagedSet[p] = struct{}{}
	}
	for _, p := range splitZ(unstaged) {
		if _, ok := stagedSet[p]; ok {
			return fmt.Errorf("%w: %s", ErrStagedDivergence, p)
		}
	}
	return nil
}

// rejectDirtySubmodules fails closed on any submodule with uncommitted or untracked content, so a
// snapshot only ever records a clean gitlink commit. An uninitialized submodule has no content to
// be dirty.
func (g *Git) rejectDirtySubmodules(ctx context.Context, worktree string) error {
	out, err := g.Run(ctx, worktree, nil, "submodule", "status")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		indicator := line[0]
		if indicator == '-' { // uninitialized: no worktree contents
			continue
		}
		fields := strings.Fields(line[1:])
		if len(fields) < 2 {
			continue
		}
		sub := filepath.Join(worktree, filepath.FromSlash(fields[1]))
		st, serr := g.Run(ctx, sub, nil, "status", "--porcelain", "--untracked-files=all")
		if serr != nil {
			return serr
		}
		if strings.TrimSpace(string(st)) != "" {
			return fmt.Errorf("%w: %s", ErrDirtySubmodule, fields[1])
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

// splitZ splits NUL-separated git output into non-empty fields.
func splitZ(b []byte) []string {
	s := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	if len(s) == 1 && s[0] == "" {
		return nil
	}
	return s
}
