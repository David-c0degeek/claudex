package testgate

import (
	"errors"
	"maps"
	"strings"
	"testing"
)

// TestEveryDesignCrashRowReachesItsStatedRecovery walks section 9 row by row.
//
// The rows are named as the design names them, because the value of this table is not that some
// decision exists for each shape but that it is THE decision the design argued for. A test that only
// asserted "some action is produced" would pass against any table at all.
func TestEveryDesignCrashRowReachesItsStatedRecovery(t *testing.T) {
	for _, tc := range []struct {
		row     string
		residue Residue
		want    RecoveryAction
		start   bool
	}{
		{"before intent published",
			Residue{Completion: CompletionUnavailable}, ActionFreshAttempt, true},
		{"after intent, before arming containment",
			Residue{IntentPublished: true, Completion: CompletionUnavailable}, ActionFreshAttempt, true},
		// Nothing was bound, so no containment domain was ever attributed to this run and no proof is
		// owed - which is exactly why this row needs none on Windows.
		{"after arming, before active CAS",
			Residue{IntentPublished: true, Completion: CompletionUnavailable}, ActionFreshAttempt, true},
		{"after active CAS, before GO, on Linux",
			Residue{IntentPublished: true, ActiveAttempt: true, Completion: CompletionProven},
			ActionLedgerOnlyInterrupted, true},
		{"after active CAS, before GO, on Windows and other POSIX",
			Residue{IntentPublished: true, ActiveAttempt: true, Completion: CompletionUnavailable},
			ActionBlock, false},
		{"after GO, before process exit, on Linux",
			Residue{IntentPublished: true, ActiveAttempt: true, Completion: CompletionProven},
			ActionLedgerOnlyInterrupted, true},
		{"after GO, before process exit, with no readable fact",
			Residue{IntentPublished: true, ActiveAttempt: true, Completion: CompletionAbsent},
			ActionBlock, false},
		{"after exit, before the guard is reacquired",
			Residue{IntentPublished: true, ActiveAttempt: true, Completion: CompletionProven},
			ActionLedgerOnlyInterrupted, true},
		{"after final observation, before result published",
			Residue{IntentPublished: true, ActiveAttempt: true, Completion: CompletionProven},
			ActionLedgerOnlyInterrupted, true},
		{"after result published, before outcome CAS",
			Residue{IntentPublished: true, ActiveAttempt: true, ResultPublished: true, Completion: CompletionProven},
			ActionFinalizeFromResult, true},
		{"outcome visible, durability unconfirmed",
			Residue{IntentPublished: true, ActiveAttempt: true, FinalizedEntry: true, Completion: CompletionProven},
			ActionReconfirmOutcome, false},
	} {
		t.Run(tc.row, func(t *testing.T) {
			got, err := Recover(tc.residue)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != tc.want {
				t.Fatalf("action = %q, want %q (%s)", got.Action, tc.want, got.Reason)
			}
			if got.StartPermitted != tc.start {
				t.Fatalf("StartPermitted = %t, want %t (%s)", got.StartPermitted, tc.start, got.Reason)
			}
		})
	}
}

// TestTheTwoIndistinguishableCutsReachTheSameConclusion pins the reason this table cannot be read as a
// list of cuts.
//
// The design says it outright: a later process cannot DISTINGUISH the cut before GO from the cut after
// it, because the durable residue is identical. Anything that treated the earlier one as benign would be
// acting on an inference the state does not support, so the two must be one row here - and the only way
// to keep that true is to assert it.
func TestTheTwoIndistinguishableCutsReachTheSameConclusion(t *testing.T) {
	for _, c := range AllCompletionFacts() {
		beforeGO := Residue{IntentPublished: true, ActiveAttempt: true, Completion: c}
		afterGO := beforeGO // identical by construction; that IS the finding
		a, err := Recover(beforeGO)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		b, err := Recover(afterGO)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if a != b {
			t.Fatalf("completion %q: %+v vs %+v", c, a, b)
		}
	}
}

// TestNoAttemptBecomesRetryableWithoutTheCompletionFact is the section 9 closing rule, asserted over
// EVERY shape rather than the rows that happened to be written with it in mind.
//
// The rule is about starting a new command, not about settling the old attempt, so it is checked
// against StartPermitted alone. A bound attempt whose containment domain is unproven may still be
// finalized from its durable result - what it may not do is let a second command run against the same
// worktree while the first one's descendants might still be alive.
func TestNoAttemptBecomesRetryableWithoutTheCompletionFact(t *testing.T) {
	for _, res := range []bool{false, true} {
		for _, fin := range []bool{false, true} {
			for _, c := range AllCompletionFacts() {
				if c == CompletionProven {
					continue
				}
				got, err := Recover(Residue{ActiveAttempt: true, ResultPublished: res, FinalizedEntry: fin, Completion: c})
				if err != nil {
					t.Fatalf("Recover: %v", err)
				}
				if got.StartPermitted {
					t.Fatalf("a bound attempt with completion %q permitted a fresh start: %+v", c, got)
				}
			}
		}
	}
}

// TestTheTableIsExactlyTheCrossProduct asserts completeness in BOTH directions.
//
// Growing either vocabulary must fail here until the new combinations are decided, and a row for a shape
// that cannot be expressed is dead weight that will be read as a decision somebody made.
func TestTheTableIsExactlyTheCrossProduct(t *testing.T) {
	want := map[observation]bool{}
	for _, active := range []bool{false, true} {
		for _, res := range []bool{false, true} {
			for _, fin := range []bool{false, true} {
				for _, c := range AllCompletionFacts() {
					want[observation{active, res, fin, c}] = true
				}
			}
		}
	}
	for key := range want {
		if _, ok := recoveryTable[key]; !ok {
			t.Errorf("no recovery is decided for %+v", key)
		}
	}
	for key := range recoveryTable {
		if !want[key] {
			t.Errorf("the table decides %+v, which no observation can produce", key)
		}
	}
	if len(recoveryTable) != len(want) {
		t.Fatalf("the table has %d rows, the cross product has %d", len(recoveryTable), len(want))
	}
}

// TestAnUndecidedShapeIsRefusedRatherThanGuessed reaches the no-row branch directly.
//
// It cannot be reached through the public surface while the table is complete, which is the point: the
// branch exists so that a vocabulary that grows without its rows fails loudly instead of falling through
// to whatever a default would have produced. Restoring the row afterwards keeps the rest of the file
// honest.
func TestAnUndecidedShapeIsRefusedRatherThanGuessed(t *testing.T) {
	key := observation{true, true, false, CompletionProven}
	saved := maps.Clone(recoveryTable)
	delete(recoveryTable, key)
	t.Cleanup(func() {
		recoveryTable = saved
	})

	_, err := Recover(Residue{ActiveAttempt: true, ResultPublished: true, Completion: CompletionProven})
	if !errors.Is(err, ErrLifecycle) {
		t.Fatalf("err = %v, want ErrLifecycle", err)
	}
	if !strings.Contains(err.Error(), "no recovery is decided") {
		t.Fatalf("err = %v, want a refusal naming the undecided shape", err)
	}
}

func TestAnUnknownCompletionFactIsRefused(t *testing.T) {
	for _, c := range []CompletionFact{"", "probably-dead", "PROVEN"} {
		_, err := Recover(Residue{ActiveAttempt: true, Completion: c})
		if !errors.Is(err, ErrLifecycle) {
			t.Fatalf("completion %q: err = %v, want ErrLifecycle", c, err)
		}
		if !strings.Contains(err.Error(), "unknown completion fact") {
			t.Fatalf("completion %q: err = %v", c, err)
		}
	}
}

// TestAVisibleFinalizationIsAlwaysReconfirmedNeverReapplied.
//
// The design's rule for this row is "re-confirm; never double-apply", and it holds whether or not the
// attempt also has a durable result. Deciding the shape by the result would re-apply an outcome that
// already committed, binding a SECOND verdict to one attempt - and the presence of the result is exactly
// the evidence that makes re-applying look safe.
func TestAVisibleFinalizationIsAlwaysReconfirmedNeverReapplied(t *testing.T) {
	for _, result := range []bool{false, true} {
		for _, c := range AllCompletionFacts() {
			got, err := Recover(Residue{ActiveAttempt: true, ResultPublished: result, FinalizedEntry: true, Completion: c})
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != ActionReconfirmOutcome {
				t.Fatalf("result=%t completion=%q: action = %q, want %q", result, c, got.Action, ActionReconfirmOutcome)
			}
			if got.StartPermitted {
				t.Fatalf("result=%t completion=%q: a fresh start was permitted before the finalization was confirmed", result, c)
			}
		}
	}
}

// TestADurableResultWithNothingBoundToItBlocks.
//
// Results are published under the guard while the reference is active, so no cut in the design produces
// this shape. That makes it evidence the state is not what this code believes, and inventing a plausible
// action for it would be the same mistake as an outcome function that answers every input.
func TestADurableResultWithNothingBoundToItBlocks(t *testing.T) {
	for _, c := range AllCompletionFacts() {
		got, err := Recover(Residue{ResultPublished: true, Completion: c})
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if got.Action != ActionBlock || got.StartPermitted {
			t.Fatalf("completion %q: %+v, want a block", c, got)
		}
	}
}

// TestAnOrphanIntentChangesTheAccountButNotTheAction.
//
// The design lists "nothing" and "an orphan intent" as separate rows reaching the same conclusion. They
// must stay one conclusion - an orphan intent is superseded, never resumed - while still reading
// differently to whoever has to act on them.
func TestAnOrphanIntentChangesTheAccountButNotTheAction(t *testing.T) {
	bare, err := Recover(Residue{Completion: CompletionUnavailable})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	orphan, err := Recover(Residue{IntentPublished: true, Completion: CompletionUnavailable})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if orphan.Action != bare.Action || orphan.StartPermitted != bare.StartPermitted {
		t.Fatalf("an orphan intent changed the decision: %+v vs %+v", orphan, bare)
	}
	if orphan.Reason == bare.Reason {
		t.Fatalf("an orphan intent is not mentioned in the reason: %q", orphan.Reason)
	}
	if !strings.Contains(orphan.Reason, "superseded") {
		t.Fatalf("the reason does not say what happens to the orphan: %q", orphan.Reason)
	}
	// A BOUND attempt is being resumed, not superseded, so the orphan note must not follow it there.
	bound, err := Recover(Residue{IntentPublished: true, ActiveAttempt: true, Completion: CompletionProven})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if strings.Contains(bound.Reason, "orphan") {
		t.Fatalf("a bound attempt was described as an orphan: %q", bound.Reason)
	}
}

// TestEveryRowStatesWhyItDecidedWhatItDid.
//
// A recovery decision is read by an operator who was not here when it was made. An empty or duplicated
// account tells them the shape was never really considered.
func TestEveryRowStatesWhyItDecidedWhatItDid(t *testing.T) {
	seen := map[string][]observation{}
	for key, dec := range recoveryTable {
		if strings.TrimSpace(dec.Reason) == "" {
			t.Fatalf("%+v decides %q with no account of why", key, dec.Action)
		}
		seen[dec.Reason] = append(seen[dec.Reason], key)
	}
	// The completion fact is irrelevant to some rows, so a reason legitimately repeats across its three
	// values - but never across shapes that differ in what is actually bound.
	for reason, keys := range seen {
		for _, k := range keys {
			if k.active != keys[0].active || k.result != keys[0].result || k.finalized != keys[0].finalized {
				t.Fatalf("the account %q is shared by materially different shapes %+v and %+v", reason, keys[0], k)
			}
		}
	}
}
