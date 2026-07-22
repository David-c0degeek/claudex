package coordinator

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// dirtyWorktree writes an untracked file into the run worktree so the next read-only-phase
// submit observes a dirty tree.
func dirtyWorktree(t *testing.T, rn *Run) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rn.runWorktree(), "stray.txt"), []byte("read-only phase edit\n"), 0o600); err != nil {
		t.Fatalf("dirty the worktree: %v", err)
	}
}

func cleanWorktree(t *testing.T, rn *Run) {
	t.Helper()
	_ = os.Remove(filepath.Join(rn.runWorktree(), "stray.txt"))
}

// The read-only-phase edit policy (03.7/04.1b) refused end-to-end on a REAL linked worktree:
// a dirty worktree refuses a CHECKPOINT (pair) and the plan phases; a clean worktree accepts;
// IMPLEMENT/FIX still route through the git transaction; a replay of an already-accepted
// read-only turn returns its receipt even when the worktree is now dirty.
func TestReadOnlyPhaseEditPolicy(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	// PLAN_DRAFT (lead, read-only): a dirty worktree is refused, no artifact, no advance.
	rs := cur(t, rn)
	if rs.Phase != state.PhasePlanDraft {
		t.Fatalf("not at PLAN_DRAFT: %s", rs.Phase)
	}
	dirtyWorktree(t, rn)
	if _, err := rn.Submit(context.Background(), lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false)); !errors.Is(err, transport.ErrRepoMutationInReadOnlyPhase) {
		t.Fatalf("dirty PLAN_DRAFT err = %v, want ErrRepoMutationInReadOnlyPhase", err)
	}
	if after := cur(t, rn); after.Revision != rs.Revision {
		t.Fatalf("a refused PLAN_DRAFT advanced the run to %d", after.Revision)
	}
	// Cleaned, it accepts.
	cleanWorktree(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))

	// PLAN_CRITIQUE (pair, read-only): dirty refused, clean accepts (AGREE → IMPLEMENT_STEP).
	rs = cur(t, rn)
	if rs.Phase != state.PhasePlanCritique {
		t.Fatalf("not at PLAN_CRITIQUE: %s", rs.Phase)
	}
	dirtyWorktree(t, rn)
	if _, err := rn.Submit(context.Background(), pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil)); !errors.Is(err, transport.ErrRepoMutationInReadOnlyPhase) {
		t.Fatalf("dirty PLAN_CRITIQUE err = %v, want ErrRepoMutationInReadOnlyPhase", err)
	}
	cleanWorktree(t, rn)
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil))

	// IMPLEMENT_STEP (lead, EDITABLE): a worktree edit is the point — it snapshots and accepts
	// through the git transaction, unaffected by the read-only gate.
	rs = cur(t, rn)
	if rs.Phase != state.PhaseImplementStep {
		t.Fatalf("not at IMPLEMENT_STEP: %s", rs.Phase)
	}
	editWorktree(t, rn)
	implTurn := rs.Assignment.ID
	res := submitOK(t, rn, lead, implReport(t, implTurn, rs.Revision))
	if res.Idempotent {
		t.Fatal("the implement submit should be a fresh acceptance")
	}

	// CHECKPOINT (pair, read-only): after the transaction the worktree is clean, so a checkpoint
	// accepts; dirtying it first is refused.
	rs = cur(t, rn)
	if rs.Phase != state.PhaseCheckpoint {
		t.Fatalf("not at CHECKPOINT: %s", rs.Phase)
	}
	dirtyWorktree(t, rn)
	if _, err := rn.Submit(context.Background(), pair, checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", true, []string{"blocking"})); !errors.Is(err, transport.ErrRepoMutationInReadOnlyPhase) {
		t.Fatalf("dirty CHECKPOINT err = %v, want ErrRepoMutationInReadOnlyPhase", err)
	}
	if after := cur(t, rn); after.Revision != rs.Revision {
		t.Fatalf("a refused CHECKPOINT advanced the run")
	}
	cleanWorktree(t, rn)
	// A REVISE with a blocking finding routes to FIX (an editable lead turn).
	submitOK(t, rn, pair, checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "REVISE", true, []string{"blocking"}))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseFix {
		t.Fatalf("not at FIX: %s", rs.Phase)
	}

	// FIX (lead, EDITABLE): a worktree edit snapshots and accepts, unaffected by the gate.
	editWorktree(t, rn)
	submitOK(t, rn, lead, implReport(t, rs.Assignment.ID, rs.Revision))
	if cur(t, rn).Phase != state.PhaseCheckpoint {
		t.Fatalf("FIX did not return to CHECKPOINT")
	}
}

// An identical replay of an already-accepted read-only turn returns the original receipt even
// when the worktree is now dirty — the read-only gate is never consulted on a replay.
func TestReadOnlyReplayIgnoresLaterDirt(t *testing.T) {
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	rs := cur(t, rn)
	draft := planArtifact(t, rs.Assignment.ID, rs.Revision, false)
	first := submitOK(t, rn, lead, draft)
	_ = pair
	// The lost-response retry arrives after the worktree became dirty; the replay still returns
	// the durable receipt.
	dirtyWorktree(t, rn)
	replay, err := rn.Submit(context.Background(), lead, draft)
	if err != nil {
		t.Fatalf("replay after later dirt: %v", err)
	}
	if !replay.Idempotent || replay.Receipt != first.Receipt {
		t.Fatalf("replay = %+v (idem %v), want the original receipt %+v", replay.Receipt, replay.Idempotent, first.Receipt)
	}
}
