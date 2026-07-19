package gitx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// snapshotRun provisions a real linked run worktree for the given (coherent) run id.
func snapshotRun(t *testing.T, runID string) (repo string, g *Git, wt, base string) {
	repo, g = initRepo(t)
	base = oidOf(t, g, repo, "refs/heads/main")
	spec := WorktreeSpec{RelPath: runWorktreeRel(runID), Branch: runBranch(runID), BaseCommit: base, OwnerToken: "boot-token"}
	if err := NewWorktree(g).Apply(context.Background(), repo, spec); err != nil {
		t.Fatalf("provision run worktree: %v", err)
	}
	return repo, g, runWorktreeAbs(repo, runID), base
}

func snapReq(repo, base, runID string) SnapshotReq {
	return SnapshotReq{RepoDir: repo, Parent: base, RunID: runID, StartingRevision: 5, CreatedUnix: 1700000000}
}

// A snapshot of the linked run worktree captures its content, commits off the parent, moves no ref,
// never touches the real index, and reproduces its OID deterministically.
func TestSnapshotCommitHappyPath(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	writeFile(t, filepath.Join(wt, "b.txt"), "new")

	commit, tree, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !isHex40or64(commit) || !isHex40or64(tree) {
		t.Fatalf("bad ids commit=%q tree=%q", commit, tree)
	}
	if p := oidOf(t, g, wt, commit+"^"); p != base {
		t.Fatalf("commit parent %q, want base %q", p, base)
	}
	names := string(mustRun(t, g, wt, nil, "ls-tree", "--name-only", tree))
	if !strings.Contains(names, "a.txt") || !strings.Contains(names, "b.txt") {
		t.Fatalf("tree contents = %q, want a.txt + b.txt", names)
	}
	if h := oidOf(t, g, wt, "HEAD"); h != base {
		t.Fatalf("HEAD moved to %q", h)
	}
	if st := strings.TrimSpace(string(mustRun(t, g, wt, nil, "diff", "--cached", "--name-only"))); st != "" {
		t.Fatalf("real index changed: %q", st)
	}
	commit2, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1"))
	if err != nil || commit2 != commit {
		t.Fatalf("non-deterministic commit: %q vs %q (err %v)", commit2, commit, err)
	}
}

func TestSnapshotRejectsNoOp(t *testing.T) {
	repo, g, _, base := snapshotRun(t, "r1")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrEmptySnapshot) {
		t.Fatalf("clean worktree = %v, want ErrEmptySnapshot", err)
	}
}

// A run id whose DERIVED worktree/branch do not exist (labelling one run while snapshotting another
// or nothing) fails closed — the identity is derived, not a caller claim.
func TestSnapshotRejectsWrongDerivedRunID(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	// Provisioned run is r1; ask to snapshot r2 (whose derived worktree does not exist).
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r2")); !errors.Is(err, ErrSnapshotWorktree) {
		t.Fatalf("wrong derived run id = %v, want ErrSnapshotWorktree", err)
	}
}

// The derived worktree path on a DIFFERENT branch than the derived run branch fails closed.
func TestSnapshotRejectsWrongBranchAtDerivedPath(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	mustRun(t, g, wt, nil, "checkout", "-b", "other") // move the run worktree off claudex/r1
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrSnapshotWorktree) {
		t.Fatalf("wrong branch at derived path = %v, want ErrSnapshotWorktree", err)
	}
}

// A detached HEAD (no run branch) is rejected.
func TestSnapshotRejectsDetachedHead(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	mustRun(t, g, wt, nil, "checkout", "--detach")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrSnapshotWorktree) {
		t.Fatalf("detached HEAD = %v, want ErrSnapshotWorktree", err)
	}
}

// A non-canonical run id is rejected before any git runs.
func TestSnapshotRejectsNonCanonicalRunID(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	for _, bad := range []string{"", ".", "..", ".hidden", "-lead", "has space", "non\nascii"} {
		if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, bad)); !errors.Is(err, ErrSnapshotInput) {
			t.Errorf("run id %q = %v, want ErrSnapshotInput", bad, err)
		}
	}
}

func TestSnapshotStagedEditDivergence(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "staged-version")
	mustRun(t, g, wt, commitEnv(), "add", "a.txt")
	writeFile(t, filepath.Join(wt, "a.txt"), "worktree-version-differs")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrStagedDivergence) {
		t.Fatalf("staged edit divergence = %v, want ErrStagedDivergence", err)
	}
}

// A staged DELETION with the worktree file retained is discarded by the snapshot and fails closed.
func TestSnapshotStagedDeletionDivergence(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	mustRun(t, g, wt, commitEnv(), "rm", "--cached", "a.txt")
	writeFile(t, filepath.Join(wt, "b.txt"), "make it non-empty")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrStagedDivergence) {
		t.Fatalf("staged deletion = %v, want ErrStagedDivergence", err)
	}
}

// A staged divergence on a filename with pathspec metacharacters is compared exactly (the map
// lookup uses the literal byte path, not a pathspec).
func TestSnapshotStagedMetacharFilename(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	name := "a[b].txt"
	writeFile(t, filepath.Join(wt, name), "staged")
	mustRun(t, g, wt, commitEnv(), "add", "--", name)
	writeFile(t, filepath.Join(wt, name), "worktree-differs")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrStagedDivergence) {
		t.Fatalf("metacharacter staged divergence = %v, want ErrStagedDivergence", err)
	}
}

// A racing edit that changes the tree between captures is rejected.
func TestSnapshotRacingEdit(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	orig := snapshotRaceHook
	defer func() { snapshotRaceHook = orig }()
	snapshotRaceHook = func() error {
		return os.WriteFile(filepath.Join(wt, "a.txt"), []byte("raced-in edit"), 0o600)
	}
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrRacingEdit) {
		t.Fatalf("racing edit = %v, want ErrRacingEdit", err)
	}
}

// The barrier catches a staged divergence interposed at the seam even when both trees match.
func TestSnapshotBarrierCatchesStagedInjection(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "worktree")
	orig := snapshotRaceHook
	defer func() { snapshotRaceHook = orig }()
	snapshotRaceHook = func() error {
		writeFile(t, filepath.Join(wt, "a.txt"), "alternate-staged")
		if _, err := g.Run(context.Background(), wt, commitEnv(), "add", "a.txt"); err != nil {
			return err
		}
		writeFile(t, filepath.Join(wt, "a.txt"), "worktree")
		return nil
	}
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrStagedDivergence) {
		t.Fatalf("barrier staged injection = %v, want ErrStagedDivergence", err)
	}
}

// A symlink is captured as a symlink entry (mode 120000), not dereferenced.
func TestSnapshotCapturesSymlink(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	if err := os.Symlink("a.txt", filepath.Join(wt, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, tree, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	entries := string(mustRun(t, g, wt, nil, "ls-tree", "-r", tree))
	if !strings.Contains(entries, "120000") || !strings.Contains(entries, "link") {
		t.Fatalf("tree %q did not capture the symlink as mode 120000", entries)
	}
}

// Object-writing commands run under the fsync contract so a frozen object cannot be lost.
func TestSnapshotObjectWritersFsynced(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	orig := runObserver
	defer func() { runObserver = orig }()
	var seen []string
	runObserver = func(argv []string) { seen = append(seen, strings.Join(argv, "\x00")) }
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, want := range []string{"add", "write-tree", "commit-tree"} {
		found := false
		for _, line := range seen {
			f := strings.Split(line, "\x00")
			if containsField(f, want) && containsField(f, "core.fsync=all") {
				found = true
			}
		}
		if !found {
			t.Errorf("%q was not run with core.fsync=all", want)
		}
	}
}

// The frozen timestamp contract is enforced (overflow/range checked).
func TestSnapshotInputValidation(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	base0 := snapReq(repo, base, "r1")
	mut := func(f func(*SnapshotReq)) SnapshotReq { r := base0; f(&r); return r }
	bad := []SnapshotReq{
		mut(func(r *SnapshotReq) { r.StartingRevision = math.MaxUint64 }),
		mut(func(r *SnapshotReq) { r.CreatedUnix = -1 }),
		mut(func(r *SnapshotReq) { r.CreatedUnix = 0 }),
	}
	for i, req := range bad {
		if _, _, err := g.SnapshotCommit(context.Background(), req); !errors.Is(err, ErrSnapshotInput) {
			t.Errorf("bad input %d = %v, want ErrSnapshotInput", i, err)
		}
	}
}

// An ambient repo commitEncoding does not leak an encoding header into the object.
func TestSnapshotForcesCommitEncoding(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	mustRun(t, g, wt, nil, "config", "i18n.commitEncoding", "ISO-8859-1")
	writeFile(t, filepath.Join(wt, "a.txt"), "edited")
	commit, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	body := string(mustRun(t, g, wt, nil, "cat-file", "commit", commit))
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "encoding ") {
			t.Fatalf("commit carries an ambient encoding header: %q", line)
		}
	}
}

// A large captured tree (its ls-tree inventory exceeds the plain-Run 1 MiB cap) is read in full, so
// a divergent record beyond the cap is still caught rather than silently truncated away.
func TestSnapshotLargeTreeNotTruncated(t *testing.T) {
	repo, g, wt, base := snapshotRun(t, "r1")
	pad := strings.Repeat("p", 120)
	if err := os.MkdirAll(filepath.Join(wt, "big"), 0o700); err != nil {
		t.Fatalf("mkdir big: %v", err)
	}
	// ~6500 entries at ~178 bytes each puts the inventory well over 1 MiB before "zzz-retained".
	for i := 0; i < 6500; i++ {
		writeFile(t, filepath.Join(wt, "big", fmt.Sprintf("%s-%05d", pad, i)), "x")
	}
	writeFile(t, filepath.Join(wt, "zzz-retained"), "tracked") // sorts LAST, after the 1 MiB point
	mustRun(t, g, wt, commitEnv(), "add", "-A")
	mustRun(t, g, wt, commitEnv(), "commit", "-m", "big")
	base = oidOf(t, g, wt, "HEAD")

	writeFile(t, filepath.Join(wt, "ordinary.txt"), "edit") // make the snapshot non-empty
	mustRun(t, g, wt, commitEnv(), "rm", "--cached", "zzz-retained")
	if _, _, err := g.SnapshotCommit(context.Background(), snapReq(repo, base, "r1")); !errors.Is(err, ErrStagedDivergence) {
		t.Fatalf("large-tree trailing divergence = %v, want ErrStagedDivergence (truncated inventory?)", err)
	}
}

func containsField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

func isHex40or64(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
