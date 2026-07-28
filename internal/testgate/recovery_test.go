package testgate

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

// Fixture values that satisfy the grammars PRODUCTION enforces.
//
// The first version used "intent-digest-7", which state rejects outright as a non-SHA-256 digest. Its
// reachability test therefore proved only that the fixture satisfied THIS package's local consistency
// rules - not that any admissible run state could hold it - which is the difference between a state
// that occurs and a state that merely type-checks.
const (
	theAttemptID = "attempt-7"
)

var (
	theIntentHash = strings.Repeat("1a", 32)
	theResultHash = strings.Repeat("2b", 32)
	theCommit     = strings.Repeat("a", 40)
	theTree       = strings.Repeat("b", 40)
)

func activeRef() *state.TestAttemptRef {
	return &state.TestAttemptRef{
		AttemptID: theAttemptID, StartRevision: 41,
		TestedCommit: theCommit, TestedTree: theTree,
		IntentDigest: theIntentHash,
	}
}

func ledgerEntry() *state.FinalizedAttempt {
	return &state.FinalizedAttempt{
		AttemptID: theAttemptID, StartRevision: 41, BoundRevision: 42,
		TestedCommit: theCommit, TestedTree: theTree,
		ResultDigest: theResultHash, Execution: state.TestExecutionOK,
		Identity: state.TestIdentityUnchanged, TerminalReason: "exited 0",
	}
}

func noArtifact() Artifact { return Artifact{State: ArtifactAbsent} }

func noResult() ResultEvidence { return ResultEvidence{State: ArtifactAbsent} }

func noStreams() StreamEvidence { return StreamEvidence{State: ArtifactAbsent} }

func intentBody() *IntentProjection {
	return &IntentProjection{
		ResolvedExecutable: "/usr/bin/go",
		ResolvedArgv:       []string{"go", "test", "./..."},
		EnvNames:           []string{"PATH"},
		EnvDigest:          strings.Repeat("5e", 32),
		Digest:             theIntentHash,
	}
}

// stagedStream builds staging evidence in the head+tail shape a truncated excerpt must have, with the
// digest over the WHOLE redacted stream - which is runner-attested and not recomputable from the
// excerpt, so it stays a fixed value here.
func stagedStream(head, tail string) StreamEvidence {
	retained := truncatedExcerpt(head, tail)
	return StreamEvidence{State: ArtifactValid, AttemptID: theAttemptID, Record: &StreamRecord{
		Present: true, SourceBytes: 4096, RedactedBytes: 4000,
		SHA256: strings.Repeat("3c", 32), Retained: retained, Truncated: true,
	}}
}

// fakeRecords is a record source rooted at one attempt. Open returns whatever it was given, so the
// verification in ReadVerifiedRecord is the only thing standing between a caller and wrong bytes.
type fakeRecords struct {
	id    string
	bytes map[string][]byte
	err   error
}

func (f *fakeRecords) AttemptID() string { return f.id }
func (f *fakeRecords) Open(digest string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bytes[digest], nil
}

func publishedResult() *ResultProjection {
	return &ResultProjection{
		AttemptID: theAttemptID, TestedCommit: theCommit, TestedTree: theTree,
		Execution: state.TestExecutionOK, Identity: state.TestIdentityUnchanged,
		TerminalReason: "exited 0", Digest: theResultHash,
	}
}

func provenFor(id string) CompletionEvidence {
	return CompletionEvidence{Fact: CompletionProven, AttemptID: id}
}

func completion(f CompletionFact) CompletionEvidence {
	if f == CompletionProven {
		return provenFor(theAttemptID)
	}
	return CompletionEvidence{Fact: f}
}

// residueFor builds an ADMISSIBLE residue for one reachable observation.
//
// It exists so completeness can be measured against states that can actually be observed rather than
// against a Cartesian product of vocabularies. If a key cannot be given consistent evidence, it is not a
// state anything can find on disk, and a decision for it would be a decision about nothing.
func residueFor(o observation) Residue {
	r := Residue{Standing: o.standing, Completion: completion(o.completion), Intent: noArtifact(),
		Result: noResult(), Stdout: noStreams(), Stderr: noStreams()}
	switch o.standing {
	case StandingActive:
		r.Active = activeRef()
		r.Intent = Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: theIntentHash}
		r.IntentBody = intentBody()
	case StandingFinalized:
		r.Finalized = ledgerEntry()
		r.Intent = Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: theIntentHash}
		r.IntentBody = intentBody()
	}
	if o.hasResult {
		r.Result = ResultEvidence{State: ArtifactValid, Record: publishedResult()}
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
			Residue{Standing: StandingNone, Intent: noArtifact(), Result: noResult(), Stdout: noStreams(), Stderr: noStreams(), Completion: completion(CompletionUnavailable)}, ActionFreshAttempt},
		{"after intent, before arming containment",
			Residue{Standing: StandingNone, Intent: orphanIntent, IntentBody: intentBody(), Result: noResult(),
				Stdout: noStreams(), Stderr: noStreams(), Completion: completion(CompletionUnavailable)}, ActionFreshAttempt},
		// Nothing was bound, so no containment domain was ever attributed to this run and no proof is
		// owed - which is exactly why this row needs none on Windows.
		{"after arming, before active CAS",
			Residue{Standing: StandingNone, Intent: orphanIntent, IntentBody: intentBody(), Result: noResult(),
				Stdout: noStreams(), Stderr: noStreams(), Completion: completion(CompletionUnavailable)}, ActionFreshAttempt},
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
//
// WHAT THIS DOES AND DOES NOT PROVE. The fixtures satisfy the grammars production enforces, asserted
// below against the exported state predicates rather than against a local copy of the rules - an earlier
// version used digests state rejects outright, so its "admissible" evidence was admissible only to this
// package. It still stops short of constructing a whole RunState and putting it through the state
// validator, which is unexported; so this establishes grammar-conforming, locally consistent evidence
// arriving at each row, not full run-state admissibility. Said plainly here rather than implied to be
// more.
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
	// The fixtures must satisfy the rules PRODUCTION applies, through the production predicates.
	ref, entry, rec := activeRef(), ledgerEntry(), publishedResult()
	for _, f := range []struct {
		name string
		ok   bool
	}{
		{"active intent digest", state.IsSHA256Hex(ref.IntentDigest)},
		{"active tested commit", state.IsGitOID(ref.TestedCommit)},
		{"active tested tree", state.IsGitOID(ref.TestedTree)},
		{"ledger result digest", state.IsSHA256Hex(entry.ResultDigest)},
		{"ledger tested commit", state.IsGitOID(entry.TestedCommit)},
		{"ledger tested tree", state.IsGitOID(entry.TestedTree)},
		{"result digest", state.IsSHA256Hex(rec.Digest)},
		{"result tested commit", state.IsGitOID(rec.TestedCommit)},
		{"result tested tree", state.IsGitOID(rec.TestedTree)},
	} {
		if !f.ok {
			t.Errorf("the %s in the reachability fixtures is not what production accepts", f.name)
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
		{"unreadable", Artifact{State: ArtifactInvalid}, "could not be validated"},
		{"belonging to another attempt",
			Artifact{State: ArtifactValid, AttemptID: "somebody-else", Digest: theIntentHash}, "belongs to"},
		{"a different intent for this attempt",
			Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: "some-other-digest"}, "but the durable intent is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := residueFor(observation{StandingActive, false, CompletionProven})
			r.Intent = tc.intent
			if tc.intent.State != ArtifactValid {
				r.IntentBody = nil
			}

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
	if got.Result == nil {
		t.Fatal("the decision names no result to finalize from")
	}
	// A digest identifies bytes; it is not those bytes. Every field state requires in the ledger entry
	// must be here, or whoever carries this out has to find the record again by ambient re-read.
	if got.Result.Digest != theResultHash || got.Result.Execution != state.TestExecutionOK ||
		got.Result.Identity != state.TestIdentityUnchanged || got.Result.TerminalReason != "exited 0" ||
		got.Result.TestedCommit != theCommit || got.Result.TestedTree != theTree {
		t.Fatalf("the decision cannot be executed from what it carries: %+v", got.Result)
	}
	if got.Attempt.StartRevision != 41 || got.Attempt.TestedTree != theTree {
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
		result ResultEvidence
		want   string
	}{
		{"a result for another attempt", func() ResultEvidence {
			rec := publishedResult()
			rec.AttemptID = "somebody-else"
			return ResultEvidence{State: ArtifactValid, Record: rec}
		}(), "belongs to"},
		{"a result that could not be validated",
			ResultEvidence{State: ArtifactInvalid}, "could not be validated"},
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
	rec := publishedResult()
	rec.Digest = strings.Repeat("9", 64)
	r.Result.Record = rec

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
			Residue{Standing: StandingActive, Intent: noArtifact(), Result: noResult(), Stdout: noStreams(), Stderr: noStreams(), Completion: completion(CompletionProven)},
			"no reference was supplied"},
		{"finalized with no ledger entry",
			Residue{Standing: StandingFinalized, Intent: noArtifact(), Result: noResult(), Stdout: noStreams(), Stderr: noStreams(), Completion: completion(CompletionProven)},
			"no entry was supplied"},
		{"finalized with an active reference too",
			Residue{Standing: StandingFinalized, Finalized: ledgerEntry(), Active: activeRef(),
				Intent: noArtifact(), Result: noResult(), Stdout: noStreams(), Stderr: noStreams(), Completion: completion(CompletionProven)},
			"an active reference was supplied too"},
		{"nothing outstanding but a reference supplied",
			Residue{Standing: StandingNone, Active: activeRef(),
				Intent: noArtifact(), Result: noResult(), Stdout: noStreams(), Stderr: noStreams(), Completion: completion(CompletionProven)},
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
			Result:     ResultEvidence{State: ArtifactValid, Record: publishedResult()},
			Stdout:     noStreams(),
			Stderr:     noStreams(),
			Completion: completion(c),
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
		{"completion", func(r *Residue) { r.Completion.Fact = "probably-dead" }, "unknown completion fact"},
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
	bare, err := Recover(Residue{Standing: StandingNone, Intent: noArtifact(), Result: noResult(),
		Stdout: noStreams(), Stderr: noStreams(), Completion: completion(CompletionUnavailable)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	orphan, err := Recover(Residue{Standing: StandingNone, Result: noResult(),
		Stdout: noStreams(), Stderr: noStreams(), Completion: completion(CompletionUnavailable),
		Intent:     Artifact{State: ArtifactValid, AttemptID: theAttemptID, Digest: theIntentHash},
		IntentBody: intentBody()})
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
	r.Result = ResultEvidence{}
	if _, err := Recover(r); !errors.Is(err, ErrLifecycle) {
		t.Fatalf("err = %v, want a refusal for an unset result artifact", err)
	}
}

// TestTheCompletionProofMustBeAboutTHISAttempt.
//
// This is the most dangerous unbound value in the package: the completion fact is the single input that
// authorises consuming an attempt's reference and releasing a new command. The receipt carrying it is
// attempt-bound by design, so a stale one left by an earlier attempt, classified as proven, would settle
// a live attempt and start a second command beside its running processes - the exact outcome the fact
// exists to prevent.
func TestTheCompletionProofMustBeAboutTHISAttempt(t *testing.T) {
	for _, hasResult := range []bool{false, true} {
		r := residueFor(observation{StandingActive, hasResult, CompletionProven})
		r.Completion = provenFor("an-earlier-attempt")

		got, err := Recover(r)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if got.Action != ActionBlock {
			t.Fatalf("result=%t: a foreign completion proof settled the attempt: %+v", hasResult, got)
		}
		if !strings.Contains(got.Reason, "not the outstanding") {
			t.Fatalf("reason = %q, want it to name whose domain was actually proven", got.Reason)
		}
	}
	// An unproven fact carries no attempt, and that must not itself be read as a mismatch - the block
	// must come from the missing proof, with its own account.
	r := residueFor(observation{StandingActive, false, CompletionAbsent})
	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Action != ActionBlock || strings.Contains(got.Reason, "not the outstanding") {
		t.Fatalf("an absent fact was reported as a mismatched one: %+v", got)
	}
}

// TestAFinalizedAttemptWithoutItsResultIsInconsistent.
//
// Every ledger entry binds a required canonical result digest, and state references a result only once
// that record is durable. "Ledger-only" describes the OUTCOME - no edge, no budget - not an entry
// pointing at a record that is not there.
func TestAFinalizedAttemptWithoutItsResultIsInconsistent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result ResultEvidence
		want   string
	}{
		{"absent", noResult(), "no result is durable"},
		{"unreadable", ResultEvidence{State: ArtifactInvalid}, "could not be validated"},
		{"belonging to another attempt", func() ResultEvidence {
			rec := publishedResult()
			rec.AttemptID = "somebody-else"
			return ResultEvidence{State: ArtifactValid, Record: rec}
		}(), "belongs to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := residueFor(observation{StandingFinalized, true, CompletionProven})
			r.Result = tc.result

			got, err := Recover(r)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != ActionBlock || !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("%+v, want a block containing %q", got, tc.want)
			}
		})
	}
	// And the result-less finalized keys must not be listed reachable, or the table would carry a
	// decision for a state that cannot occur.
	for _, o := range reachableObservations() {
		if o.standing == StandingFinalized && !o.hasResult {
			t.Fatalf("%+v is listed reachable, but a finalized attempt always binds a durable result", o)
		}
	}
}

// TestTheDecisionCannotBeRewrittenThroughTheCallersPointers.
//
// The decision authorises consuming a reference and applying a result. If the caller keeps a handle into
// the values that were validated, it can rewrite the attempt id or the bound digests between the
// decision and the action - which is the same class slice 3b had to close at every collaborator
// boundary, and a validated value is only validated while nobody else can reach it.
func TestTheDecisionCannotBeRewrittenThroughTheCallersPointers(t *testing.T) {
	r := residueFor(observation{StandingActive, true, CompletionProven})
	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	r.Active.AttemptID = "MUTATED"
	r.Active.StartRevision = 999
	r.Active.IntentDigest = "MUTATED"
	r.Active.TestedTree = "MUTATED"
	r.Result.Record.Digest = "MUTATED"
	r.Result.Record.Execution = state.TestExecutionTimeout
	r.Result.Record.TerminalReason = "MUTATED"

	if got.Attempt.AttemptID != theAttemptID || got.Attempt.StartRevision != 41 ||
		got.Attempt.IntentDigest != theIntentHash || got.Attempt.TestedTree != theTree {
		t.Fatalf("the caller rewrote the attempt the decision was about: %+v", got.Attempt)
	}
	if got.Result.Digest != theResultHash || got.Result.Execution != state.TestExecutionOK ||
		got.Result.TerminalReason != "exited 0" {
		t.Fatalf("the caller rewrote the result the decision applies: %+v", got.Result)
	}
}

// TestSettlingAnInterruptedAttemptCarriesTheRecordItMustPublish.
//
// This row consumes the attempt reference precisely when no result exists - and every ledger entry binds
// a required canonical result digest, so settling it means WRITING a record first. A decision that named
// the action without carrying that record would be naming something nobody can perform: there would be
// nothing for the entry to point at, and whoever executed it would have to invent the execution, the
// identity and the terminal reason, which are the facts nobody is entitled to invent after a crash.
func TestSettlingAnInterruptedAttemptCarriesTheRecordItMustPublish(t *testing.T) {
	r := residueFor(observation{StandingActive, false, CompletionProven})
	r.Stdout = stagedStream("head", "tail")
	r.Stderr = stagedStream("out", "err")

	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Action != ActionLedgerOnlyInterrupted {
		t.Fatalf("action = %q", got.Action)
	}
	if got.Publish == nil {
		t.Fatal("the decision settles the attempt but carries no record to publish first")
	}
	rec := got.Publish.Record
	// It must be PUBLISHABLE. Carrying a record that its own validator would refuse is the same defect
	// as carrying a description of one.
	if err := rec.validate(); err != nil {
		t.Fatalf("the planned record cannot be published: %v", err)
	}
	if rec.AttemptID != theAttemptID || rec.TestedCommit != theCommit || rec.TestedTree != theTree {
		t.Fatalf("the record does not name the attempt or the code it is about: %+v", rec)
	}
	// The command and the environment identity come from the published intent. They cannot be
	// re-derived: resolving now would describe THIS process's environment, not the one the attempt was
	// authorised against.
	if rec.ResolvedExecutable != "/usr/bin/go" || !slices.Equal(rec.ResolvedArgv, []string{"go", "test", "./..."}) {
		t.Fatalf("the record does not say which command ran: %+v", rec)
	}
	if !slices.Equal(rec.EnvNames, []string{"PATH"}) || rec.EnvDigest != strings.Repeat("5e", 32) {
		t.Fatalf("the record does not bind the environment identity: %+v", rec)
	}
	if rec.Execution != state.TestExecutionInterrupted || rec.Identity != state.TestIdentityUnobserved {
		t.Fatalf("the record claims something about the code: %+v", rec)
	}
	// The account is attributed to whoever actually wrote it.
	if rec.TerminalAuthor != state.TerminalByRecovery {
		t.Fatalf("recovery prose was attributed to the runner: %+v", rec)
	}
	// The retained bytes travel, per stream and in full - metadata alone cannot be embedded in a record.
	if !bytes.Equal(rec.Stdout.Retained, truncatedExcerpt("head", "tail")) || !rec.Stdout.Truncated {
		t.Fatalf("stdout evidence did not survive: %+v", rec.Stdout)
	}
	if !bytes.Equal(rec.Stderr.Retained, truncatedExcerpt("out", "err")) || !rec.Stderr.Truncated {
		t.Fatalf("stderr evidence did not survive: %+v", rec.Stderr)
	}
	if rec.Stdout.SourceBytes == rec.Stdout.RedactedBytes {
		t.Fatal("the fixture does not distinguish source bytes from redacted bytes")
	}
	// And the verdict it plans claims nothing about the code.
	outcome, err := state.Outcome(rec.Execution, rec.Identity)
	if err != nil {
		t.Fatalf("Outcome: %v", err)
	}
	if outcome == state.OutcomePass || outcome == state.OutcomeFail {
		t.Fatalf("an interrupted attempt routes to %q", outcome)
	}
	// Two recoveries of the same attempt must plan the same bytes, or the record digest is not a
	// function of the attempt.
	again, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if again.Publish.Record.TerminalReason != rec.TerminalReason {
		t.Fatalf("the authored account is not deterministic: %q vs %q", again.Publish.Record.TerminalReason, rec.TerminalReason)
	}
	// The other settling row publishes nothing new - it has a record already.
	fin, err := Recover(residueFor(observation{StandingActive, true, CompletionProven}))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if fin.Publish != nil {
		t.Fatalf("finalizing from an existing result also planned a publication: %+v", fin.Publish)
	}
}

// TestStagingFromAnotherAttemptCannotBeEmbedded.
//
// The staging leaves are per-attempt, and a summary from attempt A embedded in attempt B's record would
// attest bytes attempt B never produced. The two streams are also independent: a crash can leave one
// readable and the other not, which one combined state cannot express.
func TestStagingFromAnotherAttemptCannotBeEmbedded(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Residue)
		want string
	}{
		{"stdout from another attempt", func(r *Residue) {
			ev := stagedStream("h", "t")
			ev.AttemptID = "somebody-else"
			r.Stdout = ev
		}, "stdout staging belongs to"},
		{"stderr from another attempt", func(r *Residue) {
			ev := stagedStream("h", "t")
			ev.AttemptID = "somebody-else"
			r.Stderr = ev
		}, "stderr staging belongs to"},
		{"stdout unreadable", func(r *Residue) { r.Stdout = StreamEvidence{State: ArtifactInvalid} },
			"stdout staging that could not be validated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := residueFor(observation{StandingActive, false, CompletionProven})
			tc.set(&r)

			got, err := Recover(r)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != ActionBlock || !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("%+v, want a block containing %q", got, tc.want)
			}
		})
	}
	// One stream present and the other absent is an ordinary crash, not an inconsistency.
	r := residueFor(observation{StandingActive, false, CompletionProven})
	r.Stdout = stagedStream("h", "t")
	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Action != ActionLedgerOnlyInterrupted {
		t.Fatalf("one readable stream and one absent was refused: %+v", got)
	}
	if got.Publish.Record.Stderr.Present {
		t.Fatalf("an absent stream was recorded as present: %+v", got.Publish.Record.Stderr)
	}
}

// TestALedgerEntryMustAGREEWithTheRecordItNames.
//
// Naming the right record is not the same as copying it correctly. The entry holds its own copies of the
// tested identity and the verdict, so it can point at the real record and still disagree with it about
// what happened - and state cannot catch that, because the record is external to it. Same defect slice
// 3b closed on the other side of this boundary.
func TestALedgerEntryMustAGREEWithTheRecordItNames(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*state.FinalizedAttempt)
		want   string
	}{
		{"execution", func(e *state.FinalizedAttempt) { e.Execution = state.TestExecutionNonzero }, "execution"},
		{"identity", func(e *state.FinalizedAttempt) { e.Identity = state.TestIdentityChanged }, "identity"},
		{"terminal reason", func(e *state.FinalizedAttempt) { e.TerminalReason = "something else" }, "terminal reason"},
		{"tested commit", func(e *state.FinalizedAttempt) { e.TestedCommit = strings.Repeat("c", 40) }, "tested commit"},
		{"tested tree", func(e *state.FinalizedAttempt) { e.TestedTree = strings.Repeat("d", 40) }, "tested tree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := residueFor(observation{StandingFinalized, true, CompletionProven})
			entry := ledgerEntry()
			tc.break_(entry)
			r.Finalized = entry

			got, err := Recover(r)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != ActionBlock || !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("%+v, want a block naming the %s", got, tc.want)
			}
		})
	}
}

// TestAResultAboutDifferentCodeCannotFinalizeThisAttempt.
//
// A result is a statement ABOUT a particular tree. Finalizing from one computed against different code
// would publish a verdict about something this attempt never ran.
func TestAResultAboutDifferentCodeCannotFinalizeThisAttempt(t *testing.T) {
	for _, mutate := range []func(*ResultProjection){
		func(rec *ResultProjection) { rec.TestedCommit = strings.Repeat("c", 40) },
		func(rec *ResultProjection) { rec.TestedTree = strings.Repeat("d", 40) },
	} {
		r := residueFor(observation{StandingActive, true, CompletionProven})
		rec := publishedResult()
		mutate(rec)
		r.Result.Record = rec

		got, err := Recover(r)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if got.Action != ActionBlock || !strings.Contains(got.Reason, "is a statement about") {
			t.Fatalf("%+v, want a block naming the disagreement", got)
		}
	}
}

// TestEachEvidenceStateHasExactlyOneShape.
//
// A fact with two representations is a fact two readers can disagree about. "Absent" carrying a record
// and "unproven" carrying an attempt id are the same defect the exit code had, moved into the evidence:
// whether the extra value is trusted then depends on each reader checking the state field first.
func TestEachEvidenceStateHasExactlyOneShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*Residue)
		want   string
	}{
		{"proven with no attempt", func(r *Residue) { r.Completion = CompletionEvidence{Fact: CompletionProven} },
			"proven but names no attempt"},
		{"absent still naming an attempt", func(r *Residue) {
			r.Completion = CompletionEvidence{Fact: CompletionAbsent, AttemptID: theAttemptID}
		}, "still names attempt"},
		{"unavailable still naming an attempt", func(r *Residue) {
			r.Completion = CompletionEvidence{Fact: CompletionUnavailable, AttemptID: theAttemptID}
		}, "still names attempt"},
		{"absent result carrying a record", func(r *Residue) {
			r.Result = ResultEvidence{State: ArtifactAbsent, Record: publishedResult()}
		}, "still carries a record"},
		{"invalid result carrying a record", func(r *Residue) {
			r.Result = ResultEvidence{State: ArtifactInvalid, Record: publishedResult()}
		}, "still carries a record"},
		{"valid result carrying no record", func(r *Residue) {
			r.Result = ResultEvidence{State: ArtifactValid}
		}, "no record behind it"},
		{"absent intent carrying a digest", func(r *Residue) {
			r.Intent = Artifact{State: ArtifactAbsent, Digest: theIntentHash}
		}, "still carries an identity or digest"},
		{"absent stdout staging carrying a record", func(r *Residue) {
			r.Stdout = StreamEvidence{State: ArtifactAbsent, Record: &StreamRecord{Present: true}}
		}, "still carries a record or an identity"},
		{"valid stderr staging with nothing behind it", func(r *Residue) {
			r.Stderr = StreamEvidence{State: ArtifactValid, AttemptID: theAttemptID}
		}, "no record behind it"},
		{"a valid intent with no body", func(r *Residue) { r.IntentBody = nil }, "its body is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := residueFor(observation{StandingActive, false, CompletionProven})
			tc.break_(&r)

			got, err := Recover(r)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got.Action != ActionBlock || !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("%+v, want a block containing %q", got, tc.want)
			}
		})
	}
}

// TestABlockedDecisionAlsoCannotBeRewrittenThroughTheCallersPointer.
//
// The clone contract on Recovery.Attempt is unconditional. An operator-facing block that names an
// attempt the caller can rename afterwards is worse than one naming none: it is a report that quietly
// becomes about something else.
func TestABlockedDecisionAlsoCannotBeRewrittenThroughTheCallersPointer(t *testing.T) {
	r := residueFor(observation{StandingActive, false, CompletionProven})
	r.Intent = Artifact{State: ArtifactAbsent}

	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Action != ActionBlock {
		t.Fatalf("action = %q, want a block", got.Action)
	}
	r.Active.AttemptID = "MUTATED"
	if got.Attempt == nil || got.Attempt.AttemptID != theAttemptID {
		t.Fatalf("the caller renamed the attempt the block is about: %+v", got.Attempt)
	}
}

// TestTheRecordCapabilityVerifiesRatherThanPromises.
//
// An interface that only says "give me the bytes for this digest" is satisfied by an implementation that
// returns whatever is at a path - which is exactly the ambient re-read this package refuses, with the
// verification living in a comment. The check has to BE the function, so a caller cannot obtain bytes
// without it having run.
func TestTheRecordCapabilityVerifiesRatherThanPromises(t *testing.T) {
	good, digest, err := canonicalValidRecord()
	if err != nil {
		t.Fatalf("encoding the fixture: %v", err)
	}
	src := &fakeRecords{id: theAttemptID, bytes: map[string][]byte{digest: good}}

	got, err := ReadVerifiedRecord(src, theAttemptID, digest)
	if err != nil {
		t.Fatalf("a matching record was refused: %v", err)
	}
	if got.Record.AttemptID != theAttemptID {
		t.Fatalf("the verified record is not the one asked for: %+v", got.Record)
	}
	if !bytes.Equal(got.Canonical, good) {
		t.Fatalf("the canonical bytes were not preserved")
	}

	t.Run("tampered bytes", func(t *testing.T) {
		tampered := &fakeRecords{id: theAttemptID, bytes: map[string][]byte{digest: append([]byte(nil), append(good, ' ')...)}}
		if _, err := ReadVerifiedRecord(tampered, theAttemptID, digest); err == nil ||
			!strings.Contains(err.Error(), "read back as") {
			t.Fatalf("err = %v, want a digest mismatch refusal", err)
		}
	})

	t.Run("a source rooted at another attempt", func(t *testing.T) {
		foreign := &fakeRecords{id: "somebody-else", bytes: map[string][]byte{digest: good}}
		if _, err := ReadVerifiedRecord(foreign, theAttemptID, digest); err == nil ||
			!strings.Contains(err.Error(), "rooted at attempt") {
			t.Fatalf("err = %v, want a rooting refusal", err)
		}
	})

	// A digest-correct blob that is not a result. Returning bytes made ArtifactValid something the
	// CALLER asserted; this is the case that proves it is now established by code.
	t.Run("digest-correct bytes that are not a result", func(t *testing.T) {
		junk := []byte("not a result")
		jd := sha256Hex(junk)
		bad := &fakeRecords{id: theAttemptID, bytes: map[string][]byte{jd: junk}}
		if _, err := ReadVerifiedRecord(bad, theAttemptID, jd); err == nil ||
			!strings.Contains(err.Error(), "not JSON") {
			t.Fatalf("err = %v, want a decode refusal", err)
		}
	})

	t.Run("a well-formed record from another schema version", func(t *testing.T) {
		rec := validRecord()
		rec.SchemaVersion = ResultRecordVersion + 1
		raw, _ := json.Marshal(rec)
		d := sha256Hex(raw)
		src := &fakeRecords{id: theAttemptID, bytes: map[string][]byte{d: raw}}
		if _, err := ReadVerifiedRecord(src, theAttemptID, d); err == nil ||
			!strings.Contains(err.Error(), "schema version") {
			t.Fatalf("err = %v, want version remediation", err)
		}
	})

	t.Run("a record that is missing reads back as a mismatch", func(t *testing.T) {
		empty := &fakeRecords{id: theAttemptID, bytes: map[string][]byte{}}
		if _, err := ReadVerifiedRecord(empty, theAttemptID, digest); err == nil {
			t.Fatal("absent bytes were accepted")
		}
	})

	t.Run("a digest that is not a digest", func(t *testing.T) {
		if _, err := ReadVerifiedRecord(src, theAttemptID, "not-a-digest"); err == nil ||
			!strings.Contains(err.Error(), "is not a record digest") {
			t.Fatalf("err = %v, want a grammar refusal", err)
		}
	})

	t.Run("no source at all", func(t *testing.T) {
		if _, err := ReadVerifiedRecord(nil, theAttemptID, digest); err == nil {
			t.Fatal("a nil source was accepted")
		}
	})

	// The source hands over its own backing array, so it can rewrite those bytes after the hash check.
	// A verification that held only for an instant is not a verification.
	t.Run("the source mutating its bytes after the check", func(t *testing.T) {
		shared := append([]byte(nil), good...)
		mut := &fakeRecords{id: theAttemptID, bytes: map[string][]byte{digest: shared}}
		v, err := ReadVerifiedRecord(mut, theAttemptID, digest)
		if err != nil {
			t.Fatalf("ReadVerifiedRecord: %v", err)
		}
		for i := range shared {
			shared[i] = 'X'
		}
		if sha256Hex(v.Canonical) != digest {
			t.Fatal("the source rewrote the bytes the caller had verified")
		}
		if v.Record.AttemptID != theAttemptID {
			t.Fatalf("the decoded record changed underneath the caller: %+v", v.Record)
		}
	})
}

// TestTheDecisionCarriesTheSourceForWhatItOmits.
//
// The projections are deliberately partial, so the decision has to hand over the means of reaching the
// rest. Making the caller find a source would put them back in the position of doing an ambient lookup.
func TestTheDecisionCarriesTheSourceForWhatItOmits(t *testing.T) {
	r := residueFor(observation{StandingActive, true, CompletionProven})
	r.Records = &fakeRecords{id: theAttemptID}

	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Records == nil || got.Records.AttemptID() != theAttemptID {
		t.Fatalf("the decision carries no record source: %+v", got.Records)
	}

	// A source rooted at the wrong attempt is inconsistent evidence, not something to be discovered
	// later by whoever tries to use it.
	r.Records = &fakeRecords{id: "somebody-else"}
	blocked, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if blocked.Action != ActionBlock || !strings.Contains(blocked.Reason, "record source is rooted at") {
		t.Fatalf("%+v, want a block naming the rooting", blocked)
	}
}

// TestTheIntentBodyMustBeTheIntentTheAttemptBINDS.
//
// The record's command and environment identity come from the intent body, and the active reference binds
// an exact intent digest. A body read from somewhere else would put a different command into the record
// while every other check still passed.
func TestTheIntentBodyMustBeTheIntentTheAttemptBINDS(t *testing.T) {
	r := residueFor(observation{StandingActive, false, CompletionProven})
	body := intentBody()
	body.Digest = strings.Repeat("7f", 32)
	r.IntentBody = body

	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Action != ActionBlock || !strings.Contains(got.Reason, "intent body read back as") {
		t.Fatalf("%+v, want a block naming the mismatch", got)
	}
}

// TestThePlannedRecordDoesNotShareTheCallersBytes.
//
// The retained excerpt travels into the plan, and it is bytes. If the plan aliased the staging the caller
// supplied, the record about to be written could change between the decision and the write.
func TestThePlannedRecordDoesNotShareTheCallersBytes(t *testing.T) {
	r := residueFor(observation{StandingActive, false, CompletionProven})
	r.Stdout = stagedStream("h", "t")

	got, err := Recover(r)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.Publish == nil {
		t.Fatal("no plan")
	}
	retained := r.Stdout.Record.Retained
	for i := range retained {
		retained[i] = 'Z'
	}
	r.IntentBody.ResolvedArgv[0] = "MUTATED"
	if !bytes.Equal(got.Publish.Record.Stdout.Retained, truncatedExcerpt("h", "t")) {
		t.Fatalf("the planned record shares the caller's stream bytes: %q", got.Publish.Record.Stdout.Retained)
	}
	if got.Publish.Record.ResolvedArgv[0] != "go" {
		t.Fatalf("the planned record shares the caller's argv: %q", got.Publish.Record.ResolvedArgv)
	}
}

// canonicalValidRecord encodes the shared fixture through the real boundary, so these tests exercise
// bytes production would actually produce rather than a hand-rolled approximation of them.
func canonicalValidRecord() ([]byte, string, error) {
	rec := validRecord()
	rec.SchemaVersion = ResultRecordVersion
	return rec.Encode()
}
