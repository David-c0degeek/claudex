package coordinator

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// A post-snapshot edit is one that lands AFTER the implementation commit and index sync completed —
// so the accepted commit does not contain it — and BEFORE the next boundary. Nothing may
// legitimately have edited the repository in that window: TESTS is ownerless and every phase between
// carries a read-only turn. Left undetected the edit would be folded into the NEXT commit and
// attributed to work it was never part of, which is the "silently absorbed" outcome this box forbids.
//
// The refusal must bind nothing, so an operator can resolve the worktree and retry: same revision, no
// FIX assignment, run still at TESTS — and the edit itself untouched, because the coordinator has no
// business discarding a human's uncommitted work.
func TestPostSnapshotEditRefusesTheTestsBoundary(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	tests := driveToTests(t, rn, lead, pair)

	// The edit races the completed snapshot: the accepted commit is already made and the index is
	// synced, so `git status` is clean until this write lands.
	raced := filepath.Join(rn.runWorktree(), "raced.txt")
	const racedBody = "an edit that was never part of the accepted commit\n"
	writeRepoFile(t, raced, racedBody)

	_, err = rn.SubmitTestOutcome(context.Background(), false, strings.Repeat("e", 64), tests.Revision)
	if !errors.Is(err, transport.ErrPostSnapshotEdit) {
		t.Fatalf("outcome over a raced edit = %v, want ErrPostSnapshotEdit", err)
	}

	after := cur(t, rn)
	if after.Revision != tests.Revision {
		t.Fatalf("revision moved to %d, want the unchanged %d", after.Revision, tests.Revision)
	}
	if after.Phase != state.PhaseTests {
		t.Fatalf("phase = %s, want the run still at TESTS", after.Phase)
	}
	if after.Assignment != nil {
		t.Fatalf("a FIX turn was issued over a raced edit: %+v", after.Assignment)
	}
	if after.Lifecycle != state.LifecycleRunning {
		t.Fatalf("lifecycle = %s, want a recoverable running run", after.Lifecycle)
	}
	// The human's work is preserved, not reverted or absorbed.
	got, rerr := os.ReadFile(raced)
	if rerr != nil {
		t.Fatalf("the raced edit was removed: %v", rerr)
	}
	if string(got) != racedBody {
		t.Fatalf("the raced edit was rewritten: %q", got)
	}

	// Resolving the worktree lets the SAME boundary proceed — the refusal was recoverable.
	if rmErr := os.Remove(raced); rmErr != nil {
		t.Fatalf("clean up the raced edit: %v", rmErr)
	}
	res, err := rn.SubmitTestOutcome(context.Background(), false, strings.Repeat("e", 64), tests.Revision)
	if err != nil {
		t.Fatalf("retry after resolving the worktree: %v", err)
	}
	if res.Revision <= tests.Revision {
		t.Fatalf("retry revision %d did not advance past %d", res.Revision, tests.Revision)
	}
	if healed := cur(t, rn); healed.Phase != state.PhaseFix || healed.Assignment == nil {
		t.Fatalf("retry should issue FIX: phase=%s assignment=%+v", healed.Phase, healed.Assignment)
	}
}

// The ordinary path is unaffected: a clean post-commit worktree issues FIX normally.
func TestCleanPostCommitWorktreeIssuesFix(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	tests := driveToTests(t, rn, lead, pair)
	res, oerr := rn.SubmitTestOutcome(context.Background(), false, strings.Repeat("e", 64), tests.Revision)
	if oerr != nil {
		t.Fatalf("clean boundary: %v", oerr)
	}
	if res.Revision <= tests.Revision {
		t.Fatalf("revision %d did not advance past %d", res.Revision, tests.Revision)
	}
	if after := cur(t, rn); after.Phase != state.PhaseFix || after.Assignment == nil {
		t.Fatalf("clean FAIL should issue FIX: phase=%s assignment=%+v", after.Phase, after.Assignment)
	}
}
