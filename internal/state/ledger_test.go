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
	r1 := mustInit(t, s)

	// An accepted turn must correspond to a real assigned agent turn, so move into
	// IMPLEMENT_STEP and assign t1 before accepting it.
	r1b, err := s.Mutate(r1.Revision, func(rev uint64, next *RunState) error {
		next.Phase = PhaseImplementStep
		next.Assignment = &Ref{ID: "t1", IssuedRevision: rev}
		return nil
	})
	if err != nil {
		t.Fatalf("assign t1: %v", err)
	}
	r2, err := s.Mutate(r1b.Revision, func(rev uint64, next *RunState) error {
		next.AcceptedTurns["t1"] = AcceptedTurn{
			ArtifactDigest: hex64("c"),
			Receipt:        Receipt{TurnID: "t1", Revision: rev, ArtifactDigest: hex64("c")},
			Phase:          PhaseImplementStep,
		}
		next.Assignment = &Ref{ID: "t2", IssuedRevision: rev} // issue the next agent turn
		return nil
	})
	if err != nil {
		t.Fatalf("accept t1: %v", err)
	}
	r3, err := s.Mutate(r2.Revision, func(rev uint64, next *RunState) error {
		next.AcceptedTurns["t2"] = AcceptedTurn{
			ArtifactDigest: hex64("d"),
			Receipt:        Receipt{TurnID: "t2", Revision: rev, ArtifactDigest: hex64("d")},
			Phase:          PhaseImplementStep, // == the pre-transition phase (r2)
		}
		next.Phase = PhaseTests // the resulting phase must not affect the ledger
		next.Assignment = nil
		return nil
	})
	if err != nil {
		t.Fatalf("accept t2: %v", err)
	}

	want := []LedgerEntry{
		{Revision: 3, TurnID: "t1", ArtifactDigest: hex64("c"), Phase: PhaseImplementStep},
		{Revision: 4, TurnID: "t2", ArtifactDigest: hex64("d"), Phase: PhaseImplementStep},
	}
	if got := Ledger(r3); !reflect.DeepEqual(got, want) {
		t.Fatalf("ledger = %+v, want %+v", got, want)
	}

	// Reconstruct from the persisted state: same accepted artifacts -> same ledger.
	loaded, _, _ := s.Load()
	if got := Ledger(loaded); !reflect.DeepEqual(got, want) {
		t.Fatalf("reconstructed ledger = %+v, want %+v", got, want)
	}

	// Purity: a state carrying only the accepted turns yields the same ledger as
	// the full run state, proving no other field contributes.
	bare := RunState{AcceptedTurns: r3.AcceptedTurns}
	if got := Ledger(bare); !reflect.DeepEqual(got, want) {
		t.Fatalf("ledger is not a pure projection of AcceptedTurns: %+v", got)
	}
}
