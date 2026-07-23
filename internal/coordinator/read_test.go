package coordinator

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// commitTxnPlan builds a one-step commit-txn journal plan whose payload names payloadRunID and
// whose single step's Status returns the given terminal status: StatusApplied drives the txn to
// COMPLETE (a terminal head), while StatusNotApplied + an erroring Apply leaves it PENDING.
func commitTxnPlan(txnID, payloadRunID string, alwaysApplied bool) txn.Plan {
	status := txn.StatusNotApplied
	if alwaysApplied {
		status = txn.StatusApplied
	}
	return txn.Plan{
		Intent: txn.Intent{
			Version:               txn.IntentVersion,
			Kind:                  attach.CommitTxnIntentKind,
			TxnID:                 txnID,
			ExpectedStateRevision: 1,
			Payload:               []byte(fmt.Sprintf(`{"run_id":%q}`, payloadRunID)),
		},
		Steps: []txn.Step{{
			Name:           "step",
			Status:         func() (txn.StepStatus, error) { return status, nil },
			Apply:          func() error { return errors.New("halt") },
			ConfirmDurable: func() error { return nil },
		}},
	}
}

func runCommitTxn(t *testing.T, loc attach.RunLocation, plan txn.Plan, wantComplete bool) {
	t.Helper()
	g, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil || !ok {
		t.Fatalf("acquire run lock: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	rec, rerr := txn.Open(loc.CommitTxnDir, loc.RunLock).Run(g, plan)
	if wantComplete {
		if rerr != nil || !rec.Complete {
			t.Fatalf("commit-txn did not complete: rec=%+v err=%v", rec, rerr)
		}
	} else if rerr == nil {
		t.Fatalf("commit-txn unexpectedly completed")
	}
}

func abortCommitTxn(t *testing.T, loc attach.RunLocation, plan txn.Plan) {
	t.Helper()
	g, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil || !ok {
		t.Fatalf("acquire run lock: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	if _, aerr := txn.Open(loc.CommitTxnDir, loc.RunLock).Abort(g, plan); aerr != nil {
		t.Fatalf("abort commit-txn: %v", aerr)
	}
}

// A stable ABORTED commit-txn head is recovery-required: the domain classifier treats aborted as
// NonTerminal even though generic txn.Terminal() is true — a Status projection must refuse it.
func TestReadRefusesAbortedJournalHead(t *testing.T) {
	repo := t.TempDir()
	runID, _, _ := newPairedRun(t, repo)
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Prepare a PENDING transaction (Run halts on the erroring Apply), then abort it — leaving a
	// stable aborted head. A one-step plan whose step is NotApplied is abortable.
	plan := commitTxnPlan("ctxn-"+strings.Repeat("a", 32), runID, false)
	runCommitTxn(t, loc, plan, false)
	abortCommitTxn(t, loc, plan)

	if _, err := Status(repo, runID); !errors.Is(err, ErrReadRecoveryRequired) {
		t.Fatalf("status over an aborted commit-txn head = %v, want ErrReadRecoveryRequired", err)
	}
}

// A stable COMPLETE but MISBOUND commit-txn head (wrong run id in the payload) is recovery-
// required: the domain classifier fails the identity bind even though the record is complete.
func TestReadRefusesMisboundTerminalHead(t *testing.T) {
	repo := t.TempDir()
	runID, _, _ := newPairedRun(t, repo)
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Complete the txn with a payload naming a DIFFERENT run id — a terminal, misbound head.
	wrong := "run-" + strings.Repeat("b", 32)
	plan := commitTxnPlan("ctxn-"+strings.Repeat("c", 32), wrong, true)
	runCommitTxn(t, loc, plan, true)

	if _, err := Status(repo, runID); !errors.Is(err, ErrReadRecoveryRequired) {
		t.Fatalf("status over a misbound terminal head = %v, want ErrReadRecoveryRequired", err)
	}
}

// Persistent bracket churn (a run mutating faster than the bracket can read) is classified
// recovery-required, not a generic error — the injected hook advances the state between every
// pair of snapshots so no bracket is ever coherent.
func TestReadChurnExhaustionIsRecoveryRequired(t *testing.T) {
	t.Cleanup(func() { readBetweenSnapsHook = nil })
	repo := t.TempDir()
	runID, _, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()

	// Advance the state on every bracket so s1 != s2 forever: re-issue the live assignment at the
	// resulting revision (a valid, revision-bumping no-op transition, like a re-pull).
	readBetweenSnapsHook = func() {
		cur, ok, err := rn.state.Load()
		if err != nil || !ok || cur.Assignment == nil {
			return
		}
		if _, err := rn.state.Mutate(cur.Revision, func(nextRevision uint64, next *state.RunState) error {
			next.Assignment = &state.Ref{ID: cur.Assignment.ID, IssuedRevision: nextRevision}
			return nil
		}); err != nil {
			t.Errorf("churn mutation failed: %v", err)
		}
	}
	if _, err := Status(repo, runID); !errors.Is(err, ErrReadRecoveryRequired) {
		t.Fatalf("status under persistent churn = %v, want ErrReadRecoveryRequired", err)
	}
}
