package coordinator

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/state"
)

// TestGitSubmitTransactionEvidence proves the 04.2 acceptance contract on a REAL
// linked worktree, for both an IMPLEMENT_STEP and a FIX transaction:
//   - the accepted turn carries the git-commit evidence tuple, chained
//     (first parent == BaseCommit, next parent == the previous accepted commit);
//   - the run branch is at the accepted commit and the real checked-out index is
//     clean WITHOUT the worktree bytes having been rewritten;
//   - an identical replay returns the same receipt, moves no ref, and writes no
//     second commit.
func TestGitSubmitTransactionEvidence(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	g, err := gitx.New()
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	branchOID := func() string {
		return strings.TrimSpace(string(mustGit(t, g, repo, nil, "rev-parse", "--verify", "refs/heads/claudex/"+runID)))
	}
	worktreeClean := func() bool {
		out := mustGit(t, g, rn.runWorktree(), nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
		return len(strings.TrimRight(string(out), "\x00")) == 0
	}

	// Plan negotiation to IMPLEMENT_STEP.
	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))
	rs = cur(t, rn)
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseImplementStep {
		t.Fatalf("not at IMPLEMENT_STEP: %s", rs.Phase)
	}

	// IMPLEMENT transaction over a real edit.
	editWorktree(t, rn)
	editedPath := filepath.Join(rn.runWorktree(), "work.txt")
	editedBytes, err := os.ReadFile(editedPath)
	if err != nil {
		t.Fatalf("read edit: %v", err)
	}
	implTurn := rs.Assignment.ID
	implRaw := implReport(t, implTurn, rs.Revision)
	res1 := submitOK(t, rn, lead, implRaw)

	after := cur(t, rn)
	acc, ok := after.AcceptedTurns[implTurn]
	if !ok || acc.GitCommit == nil {
		t.Fatalf("implement acceptance has no git evidence: %+v", acc)
	}
	ev1 := *acc.GitCommit
	if ev1.Parent != after.BaseCommit {
		t.Fatalf("first evidence parent %s != base commit %s", ev1.Parent, after.BaseCommit)
	}
	if got := branchOID(); got != ev1.Commit {
		t.Fatalf("run branch at %s, want the accepted commit %s", got, ev1.Commit)
	}
	if !worktreeClean() {
		t.Fatal("worktree/index not clean after the implement transaction")
	}
	if now, err := os.ReadFile(editedPath); err != nil || string(now) != string(editedBytes) {
		t.Fatalf("the transaction rewrote worktree bytes: %q -> %q (err %v)", editedBytes, now, err)
	}

	// Identical replay: same receipt, no ref move, no state advance.
	res2, err := rn.Submit(context.Background(), lead, implRaw)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !res2.Idempotent || res2.Receipt != res1.Receipt {
		t.Fatalf("replay receipt = %+v (idem %v), want the original %+v", res2.Receipt, res2.Idempotent, res1.Receipt)
	}
	if got := branchOID(); got != ev1.Commit {
		t.Fatalf("replay moved the run branch to %s", got)
	}
	if again := cur(t, rn); again.Revision != after.Revision {
		t.Fatalf("replay advanced the run state to %d", again.Revision)
	}

	// CHECKPOINT REVISE (blocking) -> FIX, then the FIX transaction chains on ev1.
	rs = cur(t, rn)
	submitOK(t, rn, pair, checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", true, []string{"blocking"}))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseFix {
		t.Fatalf("not at FIX: %s", rs.Phase)
	}
	editWorktree(t, rn)
	fixTurn := rs.Assignment.ID
	submitOK(t, rn, lead, implReport(t, fixTurn, rs.Revision))

	final := cur(t, rn)
	fixAcc, ok := final.AcceptedTurns[fixTurn]
	if !ok || fixAcc.GitCommit == nil {
		t.Fatalf("fix acceptance has no git evidence: %+v", fixAcc)
	}
	ev2 := *fixAcc.GitCommit
	if ev2.Parent != ev1.Commit {
		t.Fatalf("fix evidence parent %s != previous accepted commit %s", ev2.Parent, ev1.Commit)
	}
	if got := branchOID(); got != ev2.Commit {
		t.Fatalf("run branch at %s, want the fix commit %s", got, ev2.Commit)
	}
	if !worktreeClean() {
		t.Fatal("worktree/index not clean after the fix transaction")
	}
	if latest, ok := state.LatestGitCommit(final); !ok || *latest != ev2 {
		t.Fatalf("LatestGitCommit = %+v ok=%v, want %+v", latest, ok, ev2)
	}
}
