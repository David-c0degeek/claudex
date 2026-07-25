package state

import (
	"reflect"
	"testing"
)

// The ledger is a projection of the accepted artifacts only: it reconstructs
// from AcceptedTurns alone, in receipt-revision order, and is independent of
// every other state field.
func TestLedgerProjectsAcceptedArtifactsInOrder(t *testing.T) {
	s := newStore(t)

	// Drive the real negotiation (which accepts the plan and critique turns) to an
	// agreed 1-step plan, then implement and test — each accepted turn joins the
	// ledger in receipt-revision order regardless of the phase it advanced into.
	final := driveToAgreedImplement(t, s, mustInit(t, s))
	final = assignAt(t, s, final, "t1")
	final = acceptTurnAdvance(t, s, final, "t1", hex64("c"), func(rev uint64, next *RunState) {
		next.Assignment = &Ref{ID: "t2", IssuedRevision: rev} // issue the next agent turn
		// The run stays at IMPLEMENT_STEP, an edit phase: the turn carries a worktree, not a packet.
	})
	final = acceptTurnAdvance(t, s, final, "t2", hex64("d"), func(_ uint64, next *RunState) {
		idx := 1
		next.StepIndex = &idx   // the single step is done
		next.Phase = PhaseTests // the resulting phase must not affect the ledger
	})

	// The ledger is the full accepted history, in strict receipt-revision order.
	wantOrder := []struct {
		turn  string
		phase Phase
	}{
		{planTurnID, PhasePlanDraft},
		{critTurnID, PhasePlanCritique},
		{"t1", PhaseImplementStep},
		{"t2", PhaseImplementStep},
	}
	assertLedger := func(label string, led []LedgerEntry) {
		if len(led) != len(wantOrder) {
			t.Fatalf("%s ledger has %d entries, want %d: %+v", label, len(led), len(wantOrder), led)
		}
		for i, w := range wantOrder {
			if led[i].TurnID != w.turn || led[i].Phase != w.phase {
				t.Fatalf("%s ledger[%d] = %+v, want turn %q phase %s", label, i, led[i], w.turn, w.phase)
			}
			if i > 0 && led[i-1].Revision >= led[i].Revision {
				t.Fatalf("%s ledger is not revision-ordered at %d: %+v", label, i, led)
			}
		}
	}

	led := Ledger(final)
	assertLedger("direct", led)

	// Reconstruct from the persisted state: same accepted artifacts -> same ledger.
	loaded, _, _ := s.Load()
	if got := Ledger(loaded); !reflect.DeepEqual(got, led) {
		t.Fatalf("reconstructed ledger = %+v, want %+v", got, led)
	}

	// Purity: a state carrying only the accepted turns yields the same ledger as
	// the full run state, proving no other field contributes.
	bare := RunState{AcceptedTurns: final.AcceptedTurns}
	if got := Ledger(bare); !reflect.DeepEqual(got, led) {
		t.Fatalf("ledger is not a pure projection of AcceptedTurns: %+v", got)
	}
}
