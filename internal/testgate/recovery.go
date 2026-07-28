package testgate

import (
	"fmt"

	"github.com/David-c0degeek/claudex/internal/state"
)

// Recovery is the decision a process makes when it finds a run whose test attempt did not obviously
// finish - after a crash, or after a fault that left durable residue behind.
//
// It is deliberately a function of what is READABLE FROM DURABLE STORAGE and nothing else. A
// recovering process shares no memory with the one that crashed: it cannot know which line the other
// process died on, only what that process had managed to write. Design section 9 makes the same point
// in the one place it matters most - the cut before GO and the cut after it leave IDENTICAL residue, so
// "nothing had started" is an inference the state does not support and the two rows must therefore
// reach the same conclusion.

// CompletionFact is the section 1 proof that the containment domain of an attempt is dead.
//
// Absent and unavailable both refuse to settle the attempt, and they are still kept apart: absent means
// the platform CAN publish the fact and none is readable, which an operator can investigate; unavailable
// means the platform has no mechanism at all, so the same operator would otherwise be sent looking for a
// receipt that can never exist.
type CompletionFact string

const (
	CompletionProven      CompletionFact = "proven"
	CompletionAbsent      CompletionFact = "absent"
	CompletionUnavailable CompletionFact = "unavailable"
)

// AllCompletionFacts is the closed vocabulary, exported so tests enumerate the production values
// rather than a copy that can drift out of step with them.
func AllCompletionFacts() []CompletionFact {
	return []CompletionFact{CompletionProven, CompletionAbsent, CompletionUnavailable}
}

// AttemptStanding is what the run state says about the most recent attempt.
//
// It is ONE closed value rather than independent "active" and "finalized" flags, because the store
// hands out one atomic RunState and the two conditions are mutually exclusive within it: finalization
// MOVES the reference into the ledger, and state refuses a record holding an attempt in both places.
// Modelling them as separate booleans invented a fourth combination that no observation can produce,
// and then required a decision for it.
//
// The design's "outcome visible, durability unconfirmed" row is deliberately NOT here. That uncertainty
// belongs to the process performing the append, which resolves it under the guard through
// ConfirmFinalize. A generation either committed or it did not, so a LATER reader sees one standing or
// the other and never an ambiguity between them.
type AttemptStanding string

const (
	// StandingNone means the run state names no attempt at all.
	StandingNone AttemptStanding = "none"
	// StandingActive means the run state names an attempt as active.
	StandingActive AttemptStanding = "active"
	// StandingFinalized means the most recent attempt is settled in the ledger.
	StandingFinalized AttemptStanding = "finalized"
)

// AllAttemptStandings is the closed vocabulary.
func AllAttemptStandings() []AttemptStanding {
	return []AttemptStanding{StandingNone, StandingActive, StandingFinalized}
}

// ArtifactState is what a recovering process found when it went looking for a durable record.
type ArtifactState string

const (
	// ArtifactAbsent means nothing is there.
	ArtifactAbsent ArtifactState = "absent"
	// ArtifactValid means the canonical record was read and validated.
	ArtifactValid ArtifactState = "valid"
	// ArtifactInvalid means something is there that could not be validated - unreadable or malformed.
	// It is distinct from absent because "I cannot read it" is not evidence that it does not exist, and
	// acting as though it were would discard what may be the only account of what happened.
	ArtifactInvalid ArtifactState = "invalid"
)

// AllArtifactStates is the closed vocabulary.
func AllArtifactStates() []ArtifactState {
	return []ArtifactState{ArtifactAbsent, ArtifactValid, ArtifactInvalid}
}

// Artifact is one durable record and WHOSE it is.
//
// The identity travels with it because the decisions below act on specific records: finalizing "from
// the durable result" is sound only if that result belongs to the attempt being finalized, and a bare
// boolean cannot establish it. Without this the executor would have to find the decisive record again
// through an ambient re-read, which is the untyped side channel this package exists to remove.
type Artifact struct {
	State     ArtifactState
	AttemptID string
	Digest    string
}

// Residue is the durable evidence a recovering process can read.
//
// The attempt's start revision is deliberately not a separate fact here. State refuses an attempt whose
// start revision is not the revision that created it, and refuses it before the record is serialized,
// so an attempt whose durable revision disagrees with its intent is not a state that exists to be
// recovered from. It travels inside the reference below, as evidence about the attempt rather than as a
// condition to be decided.
type Residue struct {
	Standing AttemptStanding
	// Active is the run state's reference, required when the standing is active and forbidden otherwise.
	Active *state.TestAttemptRef
	// Finalized is the ledger entry, required when the standing is finalized and forbidden otherwise.
	Finalized *state.FinalizedAttempt
	// Intent and Result are the attempt's durable records.
	Intent Artifact
	Result Artifact
	// Completion is the section 1 fact about the attempt's containment domain.
	Completion CompletionFact
}

// RecoveryAction is what to do with the attempt that was found.
type RecoveryAction string

const (
	// ActionFreshAttempt means nothing is owed: no attempt is outstanding, so a new one may be started.
	ActionFreshAttempt RecoveryAction = "fresh_attempt"
	// ActionLedgerOnlyInterrupted means finalize the attempt as interrupted, appending a ledger entry
	// with no outcome edge and no budget spent. The identity fact was never observed, so no verdict
	// about the code can be claimed.
	ActionLedgerOnlyInterrupted RecoveryAction = "ledger_only_interrupted"
	// ActionFinalizeFromResult means the SAME attempt is finalized from its already-published result.
	// The result is immutable and authoritative; recovery re-applies it rather than re-deciding it.
	ActionFinalizeFromResult RecoveryAction = "finalize_from_result"
	// ActionBlock means recovery stops for operator action.
	//
	// It is the honest answer whenever the durable state does not support a conclusion - including when
	// that state is INCONSISTENT, which is a fact about the run rather than about this code. That is why
	// it is not the same as a refusal: a refusal says THIS CODE has no decision for a shape, which is a
	// gap to be closed here; a block says the state on disk cannot be acted on safely, which is a
	// person's problem to look at. Collapsing them would hide a missing row behind an operator message,
	// or send an operator to investigate a bug in this package.
	ActionBlock RecoveryAction = "block"
)

// Recovery is the whole decision, carrying the evidence it was made from.
//
// There is deliberately NO separate "a fresh attempt may now start" output. An earlier version had one,
// on the reading that settling the attempt and permitting a new command are different questions. They
// are - but the permission was only ever an answer about the current moment, and settling an attempt
// CONSUMES the active reference, so the very next observation showed a run with nothing outstanding and
// cheerfully permitted the start that had just been refused. A prohibition its own action erases is not
// a prohibition. Section 9's closing rule is applied where it survives instead: recovery settles an
// attempt only once its containment domain is proven dead, so every settled attempt has that proof
// behind it and no later reader has to remember anything.
type Recovery struct {
	Action RecoveryAction
	// Attempt is the reference the action is about, present when the standing was active.
	Attempt *state.TestAttemptRef
	// ResultDigest names the record to finalize from, present only for ActionFinalizeFromResult.
	ResultDigest string
	// Reason is the operator-facing statement of which durable shape was found.
	Reason string
}

// observation is the table key: the facts that remain once the evidence has been proved consistent.
type observation struct {
	standing   AttemptStanding
	hasResult  bool
	completion CompletionFact
}

// reachableObservations enumerates the keys an admissible Residue can actually produce.
//
// It is NOT the Cartesian product of the vocabularies. A result belonging to no attempt is rejected as
// inconsistent evidence before the table is consulted, so the standing-none rows exist only without one.
// Enumerating the product anyway manufactures combinations no observation can produce and then demands
// a decision for each - which measures table fullness rather than coverage of reachable durable states.
func reachableObservations() []observation {
	var out []observation
	for _, c := range AllCompletionFacts() {
		out = append(out, observation{StandingNone, false, c})
		for _, r := range []bool{false, true} {
			out = append(out, observation{StandingActive, r, c})
			out = append(out, observation{StandingFinalized, r, c})
		}
	}
	return out
}

// recoveryTable is EXPLICIT over every reachable key, so a shape nobody decided is a missing row rather
// than whatever a default branch would have produced.
var recoveryTable = map[observation]Recovery{}

func init() {
	for _, c := range AllCompletionFacts() {
		// Nothing is outstanding. No containment domain was ever attributed to this run, which is why
		// the design's "after arming, before the active CAS" row needs no proof on Windows: nothing had
		// started that anything could prove dead.
		recoveryTable[observation{StandingNone, false, c}] = Recovery{
			Action: ActionFreshAttempt, Reason: "no attempt is outstanding"}

		// The most recent attempt is settled. Whether it was settled by the coordinator that ran it or
		// by an earlier recovery, its containment domain was proven dead first - a settled attempt with
		// an unproven domain is not a state this function will produce.
		for _, r := range []bool{false, true} {
			recoveryTable[observation{StandingFinalized, r, c}] = Recovery{
				Action: ActionFreshAttempt, Reason: "the most recent attempt is settled in the ledger"}
		}
	}

	// An attempt is outstanding with a published result: the design's "after result published, before
	// outcome CAS" row. The result is authoritative and is re-applied rather than re-decided - but only
	// once the domain that produced it is proven dead, because finalizing consumes the reference and
	// the run would then be free to start a second command beside the first one's survivors.
	recoveryTable[observation{StandingActive, true, CompletionProven}] = Recovery{
		Action: ActionFinalizeFromResult,
		Reason: "the outstanding attempt has a published result and its containment domain is proven dead"}
	recoveryTable[observation{StandingActive, true, CompletionAbsent}] = Recovery{
		Action: ActionBlock,
		Reason: "the outstanding attempt has a published result but no completion fact is readable"}
	recoveryTable[observation{StandingActive, true, CompletionUnavailable}] = Recovery{
		Action: ActionBlock,
		Reason: "the outstanding attempt has a published result and this platform can prove nothing about its containment domain"}

	// An attempt is outstanding with no result. Every cut from the CAS through the final observation
	// lands here and they are indistinguishable in durable state, so they share one answer: the identity
	// fact was never observed, therefore no verdict about the code may be claimed.
	recoveryTable[observation{StandingActive, false, CompletionProven}] = Recovery{
		Action: ActionLedgerOnlyInterrupted,
		Reason: "the outstanding attempt produced no result and its containment domain is proven dead"}
	recoveryTable[observation{StandingActive, false, CompletionAbsent}] = Recovery{
		Action: ActionBlock,
		Reason: "the outstanding attempt produced no result and no completion fact is readable"}
	recoveryTable[observation{StandingActive, false, CompletionUnavailable}] = Recovery{
		Action: ActionBlock,
		Reason: "the outstanding attempt produced no result and this platform can prove nothing about its containment domain"}
}

// Recover decides what to do with the durable residue of an attempt.
//
// It refuses rather than guessing. An unknown vocabulary value, or a reachable combination with no row,
// produces an error and no decision - a recovery function that answers every input is one that answers
// inputs nobody thought about. Evidence that contradicts itself is a different thing, and blocks.
func Recover(r Residue) (Recovery, error) {
	if err := r.known(); err != nil {
		return Recovery{}, err
	}
	if bad := r.inconsistency(); bad != "" {
		return Recovery{Action: ActionBlock, Attempt: r.Active, Reason: bad}, nil
	}

	dec, ok := recoveryTable[observation{r.Standing, r.Result.State == ArtifactValid, r.Completion}]
	if !ok {
		return Recovery{}, fmt.Errorf("%w: no recovery is decided for standing=%q result=%q completion=%q",
			ErrLifecycle, r.Standing, r.Result.State, r.Completion)
	}
	dec.Attempt = r.Active
	if dec.Action == ActionFinalizeFromResult {
		dec.ResultDigest = r.Result.Digest
	}
	if r.Standing == StandingNone && r.Intent.State != ArtifactAbsent {
		// The design separates "nothing" from "an orphan intent". They reach the same action - the
		// orphan is superseded, never resumed - but an operator reading this is owed the difference.
		dec.Reason += "; an orphan intent is durable and is superseded, not resumed"
	}
	return dec, nil
}

// known refuses values outside the closed vocabularies.
func (r Residue) known() error {
	switch r.Standing {
	case StandingNone, StandingActive, StandingFinalized:
	default:
		return fmt.Errorf("%w: unknown attempt standing %q", ErrLifecycle, r.Standing)
	}
	switch r.Completion {
	case CompletionProven, CompletionAbsent, CompletionUnavailable:
	default:
		return fmt.Errorf("%w: unknown completion fact %q", ErrLifecycle, r.Completion)
	}
	for _, a := range []struct {
		what string
		st   ArtifactState
	}{{"intent", r.Intent.State}, {"result", r.Result.State}} {
		switch a.st {
		case ArtifactAbsent, ArtifactValid, ArtifactInvalid:
		default:
			return fmt.Errorf("%w: unknown %s artifact state %q", ErrLifecycle, a.what, a.st)
		}
	}
	return nil
}

// inconsistency names the way the evidence contradicts itself, or "" when it hangs together.
//
// These BLOCK rather than refuse: each is a statement about the run's durable state, not about a shape
// this package forgot to decide.
func (r Residue) inconsistency() string {
	switch r.Standing {
	case StandingActive:
		if r.Active == nil {
			return "the run state names an active attempt but no reference was supplied for it"
		}
		if r.Finalized != nil {
			return fmt.Sprintf("attempt %q is active and finalized at once, which state cannot hold", r.Active.AttemptID)
		}
		// The active reference binds an exact intent digest, and the design permits state to reference
		// an intent only once that canonical record is durable. A missing or unreadable intent under an
		// active reference is therefore not an ignorable orphan - it is a reference to evidence that is
		// not there, and only an UNBOUND intent can be superseded.
		switch r.Intent.State {
		case ArtifactAbsent:
			return fmt.Sprintf("attempt %q is active but its intent is not durable", r.Active.AttemptID)
		case ArtifactInvalid:
			return fmt.Sprintf("attempt %q is active but its intent could not be validated", r.Active.AttemptID)
		}
		if r.Intent.AttemptID != r.Active.AttemptID {
			return fmt.Sprintf("attempt %q is active but the durable intent belongs to %q", r.Active.AttemptID, r.Intent.AttemptID)
		}
		if r.Intent.Digest != r.Active.IntentDigest {
			return fmt.Sprintf("attempt %q binds intent digest %q but the durable intent is %q",
				r.Active.AttemptID, r.Active.IntentDigest, r.Intent.Digest)
		}
		switch r.Result.State {
		case ArtifactInvalid:
			return fmt.Sprintf("attempt %q has a result that could not be validated", r.Active.AttemptID)
		case ArtifactValid:
			if r.Result.AttemptID != r.Active.AttemptID {
				return fmt.Sprintf("attempt %q is active but the durable result belongs to %q", r.Active.AttemptID, r.Result.AttemptID)
			}
		}
	case StandingFinalized:
		if r.Finalized == nil {
			return "the ledger is said to hold a finalization but no entry was supplied for it"
		}
		if r.Active != nil {
			return fmt.Sprintf("attempt %q is finalized but an active reference was supplied too", r.Finalized.AttemptID)
		}
		if r.Result.State == ArtifactValid && r.Result.Digest != r.Finalized.ResultDigest {
			return fmt.Sprintf("attempt %q is finalized against result %q but the durable result is %q",
				r.Finalized.AttemptID, r.Finalized.ResultDigest, r.Result.Digest)
		}
	case StandingNone:
		if r.Active != nil || r.Finalized != nil {
			return "the run state names no attempt but a reference or ledger entry was supplied"
		}
		// A result is published under the guard while a reference is active, so no cut in the design
		// produces one with nothing bound to it. That makes it evidence the state is not what this code
		// believes, and inventing a plausible action for it would be the same mistake as an outcome
		// function that answers every input.
		if r.Result.State != ArtifactAbsent {
			return "a result is durable but no attempt is outstanding to own it"
		}
	}
	return ""
}
