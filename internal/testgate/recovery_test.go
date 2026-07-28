package testgate

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

const (
	theAttemptID  = "attempt-7"
	theIntentHash = "intent-digest-7"
	theResultHash = "result-digest-7"
)

func activeRef() *state.TestAttemptRef {
	return &state.TestAttemptRef{
		AttemptID: theAttemptID, StartRevision: 41,
		TestedCommit: strings.Repeat("a", 40), TestedTree: strings.Repeat("b", 40),
		IntentDigest: theIntentHash,
	}
}

func ledgerEntry() *state.FinalizedAttempt {
	return &state.FinalizedAttempt{
		AttemptID: theAttemptID, StartRevision: 41, BoundRevision: 42,
		TestedCommit: strings.Repeat("a", 40), TestedTree: strings.Repeat("b", 40),
		ResultDigest: theResultHash, Execution: state.TestExecutionOK,
		Identity: state.TestIdentityUnchanged, TerminalReason: "exited 0",
	}
}

func noArtifact() Artifact { return Artifact{State: ArtifactAbsent} }

// residueFor builds an ADMISSIBLE residue for one reachable observation.
//
// It exists so completeness can be measured against states that can actually be observed rather than
// against a Cartesian product of vocabularies. If a key cannot be given consistent evidence, it is not a
// state anything can find on disk, and a decision for it would be a decision about nothing.
func residueFor(o observation) Residue {
	r := Residue{Standing: o.standing, Completion: o.completion, Intent: noArtifact(), Result: noArtifact()}
	switch o.standing {
	case StandingActive:
		r.Active = activeRef()
		r.Intent = Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: theIntentHash}
	case StandingFinalized:
		r.Finalized = ledgerEntry()
		r.Intent = Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: theIntentHash}
	}
	if o.hasResult {
		r.Result = Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: theResultHash}
	}
	return r
}

// TestEveryDesignCrashRowReachesItsStatedRecovery walks section 9 row by row.
//
// The rows are named as the design names them, because the value of this table is not that some
// decision exists for each shape but that it is THE decision the design argued for. A test asserting
// only that "some action is produced" would pass against any table at all.
func TestEveryDesignCrashRowReachesItsStatedRecovery(t *testing.T) {
	orphanIntent := Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: theIntentHash}
	for _, tc := range []struct {
		row     string
		residue Residue
		want    RecoveryAction
	}{
		{"before intent published",
			Residue{Standing: StandingNone, Intent: noArtifact(), Result: noArtifact(), Completion: CompletionUnavailable}, ActionFreshAttempt},
		{"after intent, before arming containment",
			Residue{Standing: StandingNone, Intent: orphanIntent, Result: noArtifact(), Completion: CompletionUnavailable}, ActionFreshAttempt},
		// Nothing was bound, so no containment domain was ever attributed to this run and no proof is
		// owed - which is exactly why this row needs none on Windows.
		{"after arming, before active CAS",
			Residue{Standing: StandingNone, Intent: orphanIntent, Result: noArtifact(), Completion: CompletionUnavailable}, ActionFreshAttempt},
		{"after active CAS, before GO, on Linux",
			residueFor(observation{StandingActive, false, CompletionProven}), ActionLedgerOnlyInterrupted},
		{"after active CAS, before GO, on Windows and other POSIX",
			residueFor(observation{StandingActive, false, CompletionUnavailable}), ActionBlock},
		{"after GO, before process exit, on Linux",
			residueFor(observation{StandingActive, false, CompletionProven}), ActionLedgerOnlyInterrupted},
		{"after GO, before process exit, with no readable fact",
			residueFor(observation{StandingActive, false, CompletionAbsent}), ActionBlock},
		{"after exit, before the guard is reacquired",
			residueFor(observation{StandingActive, false, CompletionProven}), ActionLedgerOnlyInterrupted},
		{"after final observation, before result published",
			residueFor(observation{StandingActive, false, CompletionProven}), ActionLedgerOnlyInterrupted},
		{"after result published, before outcome CAS",
			residueFor(observation{StandingActive, true, CompletionProven}), ActionFinalizeFromResult},
		{"the outcome committed, so nothing is outstanding",
			residueFor(observation{StandingFinalized, true, CompletionProven}), ActionFreshAttempt},
	} {
		t.Run(tc.row, func(t *testing.T) {
			got, err := Recover(tc.residue)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != tc.want {
				t.Fatalf("action = %q, want %q (%s)", got.Action, tc.want, got.Reason)
			}
		})
	}
}

// TestSettlingAnAttemptRequiresItsDomainToBeProvenDead is section 9's closing rule, applied where it
// SURVIVES the action rather than only at the moment of deciding.
//
// An earlier version answered this with a separate "a fresh start is permitted" flag. That flag was
// only ever about the current moment: settling an attempt consumes the active reference, so the very
// next observation showed a run with nothing outstanding and permitted the start that had just been
// refused. The prohibition has to be enforced where it cannot be erased - by not settling the attempt
// at all until the fact is proven.
func TestSettlingAnAttemptRequiresItsDomainToBeProvenDead(t *testing.T) {
	for _, hasResult := range []bool{false, true} {
		for _, c := range AllCompletionFacts() {
			got, err := Recover(residueFor(observation{StandingActive, hasResult, c}))
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			settles := got.Action == ActionFinalizeFromResult || got.Action == ActionLedgerOnlyInterrupted
			if settles != (c == CompletionProven) {
				t.Fatalf("result=%t completion=%q produced %q; an attempt may be settled only when its domain is proven dead",
					hasResult, c, got.Action)
			}
		}
	}
}

// TestASettledAttemptCannotBeFollowedByAnUnprovenStart is the same rule seen from the other side.
//
// This is the sequence the split output got wrong: finalize with the fact unproven, then look again.
// Because settling now requires the proof, no reachable finalized state can have an unproven domain
// behind it, and the follow-on observation is safe by construction rather than by memory.
func TestASettledAttemptCannotBeFollowedByAnUnprovenStart(t *testing.T) {
	for _, c := range []CompletionFact{CompletionAbsent, CompletionUnavailable} {
		before, err := Recover(residueFor(observation{StandingActive, true, c}))
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if before.Action != ActionBlock {
			t.Fatalf("completion %q settled the attempt, so the state it leaves behind permits a start: %+v", c, before)
		}
		// And the state it WOULD have left behind is one that permits a fresh start, which is precisely
		// why the refusal has to happen above rather than here.
		after, err := Recover(residueFor(observation{StandingFinalized, true, c}))
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if after.Action != ActionFreshAttempt {
			t.Fatalf("a settled attempt did not permit a fresh start: %+v", after)
		}
	}
}

// TestTheTwoIndistinguishableCutsReachTheSameConclusion pins the reason this table cannot be read as a
// list of cuts.
//
// The design says it outright: a later process cannot DISTINGUISH the cut before GO from the cut after
// it, because the durable residue is identical. Anything treating the earlier one as benign would act on
// an inference the state does not support, so the two must be one row - and the only way to keep that
// true is to assert it.
func TestTheTwoIndistinguishableCutsReachTheSameConclusion(t *testing.T) {
	for _, c := range AllCompletionFacts() {
		beforeGO := residueFor(observation{StandingActive, false, c})
		afterGO := residueFor(observation{StandingActive, false, c}) // identical by construction; that IS the finding
		a, err := Recover(beforeGO)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		b, err := Recover(afterGO)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if a.Action != b.Action || a.Reason != b.Reason {
			t.Fatalf("completion %q: %+v vs %+v", c, a, b)
		}
	}
}

// TestTheTableCoversExactlyTheREACHABLEObservations.
//
// Completeness over a Cartesian product of vocabularies would prove only that the table is full. The
// product contains combinations the state authority makes unrepresentable - an attempt cannot be active
// and finalized at once - and a row for one of those reads later as a decision somebody made about a
// state that can occur. So every key here must be REACHABLE: constructible as admissible evidence that
// actually arrives at that row.
func TestTheTableCoversExactlyTheREACHABLEObservations(t *testing.T) {
	want := map[observation]bool{}
	for _, o := range reachableObservations() {
		want[o] = true
		if _, ok := recoveryTable[o]; !ok {
			t.Errorf("no recovery is decided for the reachable observation %+v", o)
			continue
		}
		// Reachability is PROVED, not asserted: admissible evidence must exist for the key and must
		// survive the consistency checks to arrive at it.
		r := residueFor(o)
		if bad := r.inconsistency(); bad != "" {
			t.Errorf("%+v is listed reachable but its evidence is inconsistent: %s", o, bad)
		}
		if got, err := Recover(r); err != nil {
			t.Errorf("%+v is listed reachable but Recover refused it: %v", o, err)
		} else if got.Action != recoveryTable[o].Action {
			t.Errorf("%+v arrived at %q, not its own row %q", o, got.Action, recoveryTable[o].Action)
		}
	}
	for key := range recoveryTable {
		if !want[key] {
			t.Errorf("the table decides %+v, which no admissible observation can produce", key)
		}
	}
	if len(recoveryTable) != len(want) {
		t.Fatalf("the table has %d rows, the reachable set has %d", len(recoveryTable), len(want))
	}
}

// TestAnAttemptCannotBeActiveAndFinalizedAtOnce.
//
// State enforces this: finalization MOVES the reference into the ledger rather than copying it, and a
// record holding an attempt in both places is refused. Modelling the two as independent booleans
// invented a combination nothing can produce and then gave it an action.
func TestAnAttemptCannotBeActiveAndFinalizedAtOnce(t *testing.T) {
	r := residueFor(observation{StandingActive, false, CompletionProven})
	r.Finalized = ledgerEntry()

	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Action != ActionBlock {
		t.Fatalf("action = %q, want a block", got.Action)
	}
	if !strings.Contains(got.Reason, "which state cannot hold") {
		t.Fatalf("reason = %q, want it to say the state is impossible", got.Reason)
	}
}

// TestAnActiveAttemptWithoutItsIntentIsInconsistentNotHealthy.
//
// The active reference binds an exact intent digest, and state may reference an intent only once that
// canonical record is durable. So a missing, unreadable, foreign or mismatched intent under an active
// reference is a reference to evidence that is not there. Only an UNBOUND intent is the ignorable orphan
// the design describes; treating a bound one the same way would run an attempt whose authorisation
// nobody can read.
func TestAnActiveAttemptWithoutItsIntentIsInconsistentNotHealthy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		intent Artifact
		want   string
	}{
		{"absent", Artifact{State: ArtifactAbsent}, "not durable"},
		{"unreadable", Artifact{State: ArtifactInvalid, AttemptID: theAttemptID}, "could not be validated"},
		{"belonging to another attempt",
			Artifact{State: ArtifactValid, AttemptID: "somebody-else", Digest: theIntentHash}, "belongs to"},
		{"a different intent for this attempt",
			Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: "some-other-digest"}, "but the durable intent is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := residueFor(observation{StandingActive, false, CompletionProven})
			r.Intent = tc.intent

			got, err := Recover(r)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != ActionBlock {
				t.Fatalf("action = %q, want a block (%s)", got.Action, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("reason = %q, want it to contain %q", got.Reason, tc.want)
			}
		})
	}
}

// TestTheDecisionCarriesTheEvidenceItWasMadeFrom.
//
// "Finalize the same attempt from its durable result" is not executable from a boolean. Without the
// identity and the digest travelling with the decision, whoever carries it out has to find the decisive
// record again by ambient re-read - and nothing would then establish that the result it finds is the one
// this decision was about.
func TestTheDecisionCarriesTheEvidenceItWasMadeFrom(t *testing.T) {
	got, err := Recover(residueFor(observation{StandingActive, true, CompletionProven}))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Action != ActionFinalizeFromResult {
		t.Fatalf("action = %q", got.Action)
	}
	if got.Attempt == nil || got.Attempt.AttemptID != theAttemptID {
		t.Fatalf("the decision does not name the attempt it is about: %+v", got.Attempt)
	}
	if got.ResultDigest != theResultHash {
		t.Fatalf("ResultDigest = %q, want the digest of the record to finalize from", got.ResultDigest)
	}
	if got.Attempt.StartRevision != 41 || got.Attempt.TestedTree != strings.Repeat("b", 40) {
		t.Fatalf("the reference lost the identity the attempt is a statement about: %+v", got.Attempt)
	}
}

// TestAResultBelongingToAnotherAttemptIsRefusedAsEvidence.
//
// The whole point of carrying identity is that "a result is published" must mean "the matching result
// for THIS attempt". A foreign record satisfies the boolean and would be finalized as though it were
// this attempt's own verdict.
func TestAResultBelongingToAnotherAttemptIsRefusedAsEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result Artifact
		want   string
	}{
		{"a result for another attempt",
			Artifact{State: ArtifactValid, AttemptID: "somebody-else", Digest: theResultHash}, "belongs to"},
		{"a result that could not be validated",
			Artifact{State: ArtifactInvalid, AttemptID: theAttemptID}, "could not be validated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := residueFor(observation{StandingActive, true, CompletionProven})
			r.Result = tc.result

			got, err := Recover(r)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != ActionBlock {
				t.Fatalf("action = %q, want a block (%s)", got.Action, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("reason = %q, want it to contain %q", got.Reason, tc.want)
			}
		})
	}
}

// TestAFinalizedAttemptMustAgreeWithTheResultOnDisk.
//
// The ledger entry names the result digest it was authorised by. A durable result that is a DIFFERENT
// record is evidence the two disagree about what this attempt did.
func TestAFinalizedAttemptMustAgreeWithTheResultOnDisk(t *testing.T) {
	r := residueFor(observation{StandingFinalized, true, CompletionProven})
	r.Result.Digest = "a-different-record"

	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Action != ActionBlock || !strings.Contains(got.Reason, "is finalized against result") {
		t.Fatalf("%+v, want a block naming the disagreement", got)
	}
}

// TestMissingCounterpartsForAStandingAreRefusedAsEvidence.
func TestMissingCounterpartsForAStandingAreRefusedAsEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		residue Residue
		want    string
	}{
		{"active with no reference",
			Residue{Standing: StandingActive, Intent: noArtifact(), Result: noArtifact(), Completion: CompletionProven},
			"no reference was supplied"},
		{"finalized with no ledger entry",
			Residue{Standing: StandingFinalized, Intent: noArtifact(), Result: noArtifact(), Completion: CompletionProven},
			"no entry was supplied"},
		{"finalized with an active reference too",
			Residue{Standing: StandingFinalized, Finalized: ledgerEntry(), Active: activeRef(),
				Intent: noArtifact(), Result: noArtifact(), Completion: CompletionProven},
			"an active reference was supplied too"},
		{"nothing outstanding but a reference supplied",
			Residue{Standing: StandingNone, Active: activeRef(),
				Intent: noArtifact(), Result: noArtifact(), Completion: CompletionProven},
			"names no attempt but a reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Recover(tc.residue)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != ActionBlock || !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("%+v, want a block containing %q", got, tc.want)
			}
		})
	}
}

// TestADurableResultWithNothingOutstandingBlocks.
//
// Results are published under the guard while a reference is active, so no cut in the design produces
// this shape. That makes it evidence the state is not what this code believes, and inventing a plausible
// action for it would be the same mistake as an outcome function that answers every input.
func TestADurableResultWithNothingOutstandingBlocks(t *testing.T) {
	for _, c := range AllCompletionFacts() {
		got, err := Recover(Residue{
			Standing:   StandingNone,
			Intent:     noArtifact(),
			Result:     Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: theResultHash},
			Completion: c,
		})
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if got.Action != ActionBlock {
			t.Fatalf("completion %q: %+v, want a block", c, got)
		}
	}
}

// TestAnUndecidedShapeIsRefusedRatherThanGuessed reaches the no-row branch directly.
//
// It cannot be reached through the public surface while the table is complete, which is the point: the
// branch exists so a vocabulary that grows without its rows fails loudly instead of falling through to
// whatever a default would have produced.
func TestAnUndecidedShapeIsRefusedRatherThanGuessed(t *testing.T) {
	key := observation{StandingActive, true, CompletionProven}
	saved := maps.Clone(recoveryTable)
	delete(recoveryTable, key)
	t.Cleanup(func() { recoveryTable = saved })

	_, err := Recover(residueFor(key))
	if !errors.Is(err, ErrLifecycle) {
		t.Fatalf("err = %v, want ErrLifecycle", err)
	}
	if !strings.Contains(err.Error(), "no recovery is decided") {
		t.Fatalf("err = %v, want a refusal naming the undecided shape", err)
	}
}

func TestUnknownVocabularyValuesAreRefused(t *testing.T) {
	base := residueFor(observation{StandingActive, false, CompletionProven})
	for _, tc := range []struct {
		name  string
		mutex func(*Residue)
		want  string
	}{
		{"standing", func(r *Residue) { r.Standing = "probably-running" }, "unknown attempt standing"},
		{"completion", func(r *Residue) { r.Completion = "probably-dead" }, "unknown completion fact"},
		{"intent artifact", func(r *Residue) { r.Intent.State = "probably-there" }, "unknown intent artifact state"},
		{"result artifact", func(r *Residue) { r.Result.State = "probably-there" }, "unknown result artifact state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutex(&r)
			_, err := Recover(r)
			if !errors.Is(err, ErrLifecycle) {
				t.Fatalf("err = %v, want ErrLifecycle", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestAnOrphanIntentChangesTheAccountButNotTheAction.
//
// The design lists "nothing" and "an orphan intent" as separate rows reaching the same conclusion. They
// must stay one conclusion - an orphan intent is superseded, never resumed - while still reading
// differently to whoever has to act on them. It must NOT follow a bound attempt, which is being resumed.
func TestAnOrphanIntentChangesTheAccountButNotTheAction(t *testing.T) {
	bare, err := Recover(Residue{Standing: StandingNone, Intent: noArtifact(), Result: noArtifact(),
		Completion: CompletionUnavailable})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	orphan, err := Recover(Residue{Standing: StandingNone, Result: noArtifact(), Completion: CompletionUnavailable,
		Intent: Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: theIntentHash}})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if orphan.Action != bare.Action {
		t.Fatalf("an orphan intent changed the decision: %+v vs %+v", orphan, bare)
	}
	if orphan.Reason == bare.Reason || !strings.Contains(orphan.Reason, "superseded") {
		t.Fatalf("the reason does not say what happens to the orphan: %q", orphan.Reason)
	}
	bound, err := Recover(residueFor(observation{StandingActive, false, CompletionProven}))
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
	// A reason legitimately repeats across facts it does not depend on - a settled attempt reads the
	// same whether or not its result is still on disk - but never across shapes that differ in what is
	// outstanding.
	for reason, keys := range seen {
		for _, k := range keys {
			if k.standing != keys[0].standing {
				t.Fatalf("the account %q is shared by materially different shapes %+v and %+v", reason, keys[0], k)
			}
		}
	}
}

// TestAZeroResidueIsRefusedRatherThanReadAsNothingFound.
//
// The zero value of every vocabulary here is deliberately NOT a member of it. A caller who forgot to
// populate the struct would otherwise be indistinguishable from one who looked and found nothing, and
// the answer to "nothing found" is to start a fresh command - so an uninitialised read would authorise
// exactly the thing the completion fact exists to prevent. This is the same rule that gave the exit code
// one shape for absent, applied to the evidence rather than to the outcome.
func TestAZeroResidueIsRefusedRatherThanReadAsNothingFound(t *testing.T) {
	_, err := Recover(Residue{})
	if !errors.Is(err, ErrLifecycle) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	// And a residue that is fully populated except for one forgotten artifact is refused too, since
	// that is the shape a partial migration actually produces.
	r := residueFor(observation{StandingActive, false, CompletionProven})
	r.Result = Artifact{}
	if _, err := Recover(r); !errors.Is(err, ErrLifecycle) {
		t.Fatalf("err = %v, want a refusal for an unset result artifact", err)
	}
}
