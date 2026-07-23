package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// The lead owns the PLAN_DRAFT turn, so a lead wait wakes immediately with the assignment,
// resolved coherently through the aggregate bracket (no lock held).
func TestWaitLeadWakesOnOwnAssignment(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	ev, err := Wait(context.Background(), repo, runID, lead, 0, time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != transport.WaitAssignment || ev.TurnID == nil || ev.Revision != 2 {
		t.Fatalf("ev = %+v, want a lead assignment at revision 2", ev)
	}
}

// The pair does not own the PLAN_DRAFT (lead) turn, so a pair wait blocks and returns unchanged
// at the deadline.
func TestWaitPairUnchangedOnTimeout(t *testing.T) {
	repo := t.TempDir()
	runID, _, pair := newPairedRun(t, repo)
	ev, err := Wait(context.Background(), repo, runID, pair, 2, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if ev.Kind != transport.WaitUnchanged || ev.Revision != 2 {
		t.Fatalf("ev = %+v, want unchanged at revision 2", ev)
	}
}

// An unknown session fails closed with a value-free session-view error, immediately.
func TestWaitUnknownSessionFailsClosed(t *testing.T) {
	repo := t.TempDir()
	runID, _, _ := newPairedRun(t, repo)
	_, err := Wait(context.Background(), repo, runID, "sess-does-not-exist", 0, time.Second)
	if !errors.Is(err, transport.ErrSessionView) {
		t.Fatalf("err = %v, want ErrSessionView", err)
	}
	if strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("wait leaked the session id: %v", err)
	}
}

// A commit-txn journal that stays nonterminal for the whole wait persists to the deadline and
// surfaces as recovery-required (the same sentinel Status uses) — never a torn or ordinary event.
func TestWaitPersistentRecoverySurfaces(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// A pending (never-completed) commit-txn head: Run halts on the erroring Apply, leaving a
	// stable nonterminal head that every poll observes.
	plan := commitTxnPlan("ctxn-"+strings.Repeat("d", 32), runID, false)
	runCommitTxn(t, loc, plan, false)

	_, err = Wait(context.Background(), repo, runID, lead, 0, 150*time.Millisecond)
	if !errors.Is(err, ErrReadRecoveryRequired) {
		t.Fatalf("wait over a persistent nonterminal aggregate = %v, want ErrReadRecoveryRequired", err)
	}
}
