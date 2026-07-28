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
// in the one place it matters most - the cut before GO and the cut after GO leave IDENTICAL residue on
// Windows, so "nothing had started" is an inference the state does not support and the two rows must
// therefore reach the same conclusion.

// CompletionFact is the section 1 proof that the containment domain of an attempt is dead.
//
// Absent and unavailable both block, and they are still kept apart: absent means the platform CAN
// publish the fact and none is readable, which an operator can investigate; unavailable means the
// platform has no mechanism at all, so the same operator would otherwise be sent looking for a receipt
// that can never exist.
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

// Residue is the durable evidence a recovering process can read.
//
// The attempt's start revision is deliberately NOT one of these facts. State refuses a new active
// attempt whose start revision is not the revision that created it, and refuses it before the record is
// serialized, so an attempt whose durable revision disagrees with its intent is not a state that exists
// to be recovered from. A field for it here would be a decision about an impossible shape, read later as
// evidence that somebody had considered it.
type Residue struct {
	// IntentPublished records that an attempt intent is durable. It never changes the ACTION - an
	// orphan intent is ignored either way - but the design lists "nothing" and "an orphan intent" as
	// separate rows, and an operator reading the reason is owed the difference.
	IntentPublished bool
	// ActiveAttempt records that the run state still names an attempt as active.
	ActiveAttempt bool
	// ResultPublished records that the immutable result record for that attempt is durable.
	ResultPublished bool
	// FinalizedEntry records that the ledger already holds a finalization for it.
	FinalizedEntry bool
	// Completion is the section 1 fact about the attempt's containment domain.
	Completion CompletionFact
}

// RecoveryAction is what to do with the attempt that was found.
type RecoveryAction string

const (
	// ActionFreshAttempt means nothing is owed: no attempt is bound, so a new one may be started.
	ActionFreshAttempt RecoveryAction = "fresh_attempt"
	// ActionLedgerOnlyInterrupted means finalize the attempt as interrupted, appending a ledger entry
	// with no outcome edge and no budget spent. The identity fact was never observed, so no verdict
	// about the code can be claimed.
	ActionLedgerOnlyInterrupted RecoveryAction = "ledger_only_interrupted"
	// ActionFinalizeFromResult means the SAME attempt is finalized from its already-published result.
	// The result is immutable and authoritative; recovery re-applies it rather than re-deciding it.
	ActionFinalizeFromResult RecoveryAction = "finalize_from_result"
	// ActionReconfirmOutcome means the finalization is visible and must be re-confirmed by READING.
	// Never re-applied: re-applying an outcome that did commit would bind a second verdict to one
	// attempt.
	ActionReconfirmOutcome RecoveryAction = "reconfirm_outcome"
	// ActionBlock means recovery stops for operator action.
	//
	// It is the honest answer whenever the durable state does not support a conclusion - including when
	// the state is INCONSISTENT, which is a fact about the run rather than about this code. That is why
	// it is not the same as the refusal below: a refusal says THIS CODE has no decision for a shape,
	// which is a gap to be closed here; a block says the state on disk cannot be acted on safely, which
	// is a person's problem to look at. Collapsing them would hide a missing row behind an operator
	// message, or send an operator to investigate a bug in this package.
	ActionBlock RecoveryAction = "block"
)

// Recovery is the whole decision.
type Recovery struct {
	Action RecoveryAction
	// StartPermitted says whether a FRESH attempt may begin once Action has been carried out.
	//
	// It is separate from Action on purpose. Settling the attempt that was found and being allowed to
	// run a new command are different questions, and section 9 answers them differently: "an active
	// attempt cannot become retryable until the section 1 cleanup completion fact is proven". Finalizing
	// a durable result settles the attempt without proving anything about its containment domain, so
	// collapsing the two would let a recovering process start a second command while the first one's
	// descendants may still be running against the same worktree.
	StartPermitted bool
	// Reason is the operator-facing statement of which durable shape was found.
	Reason string
}

// observation is the table key. Every field is one durable fact.
type observation struct {
	active     bool
	result     bool
	finalized  bool
	completion CompletionFact
}

// recoveryTable is EXPLICIT over the whole cross product, so a shape nobody decided is a missing row
// rather than whatever a default branch would have produced. The design's crash table is the source
// for every entry; a combination the design does not describe is refused rather than guessed.
var recoveryTable = map[observation]Recovery{}

func init() {
	// No attempt is bound. Nothing was CAS'd, so no containment domain was ever attributed to this run
	// and no completion fact is owed - which is exactly why the design's "after arming, before the
	// active CAS" row needs no proof on Windows: nothing had started that anything could prove dead.
	for _, c := range AllCompletionFacts() {
		recoveryTable[observation{false, false, false, c}] = Recovery{
			ActionFreshAttempt, true, "no attempt is bound"}
		recoveryTable[observation{false, false, true, c}] = Recovery{
			ActionFreshAttempt, true, "the previous attempt was finalized with a ledger-only entry"}
		recoveryTable[observation{false, true, true, c}] = Recovery{
			ActionFreshAttempt, true, "the previous attempt was finalized from its published result"}
		// A durable result with nothing bound to it. Results are published under the guard while the
		// reference is active, so this shape cannot be produced by any cut in the design - which makes it
		// evidence that the state is not what this code believes, and the only safe answer is to stop.
		recoveryTable[observation{false, true, false, c}] = Recovery{
			ActionBlock, false, "a result is durable but no attempt is bound to it"}
	}

	// An attempt is bound and its finalization is already visible. Confirmation READS; it never
	// re-applies. Whether a fresh attempt may follow is decided after that read, not now.
	for _, c := range AllCompletionFacts() {
		recoveryTable[observation{true, false, true, c}] = Recovery{
			ActionReconfirmOutcome, false, "a finalization is visible for the active attempt but the reference still stands"}
		recoveryTable[observation{true, true, true, c}] = Recovery{
			ActionReconfirmOutcome, false, "a finalization and a result are both visible for the active attempt"}
	}

	// An attempt is bound with a published result and no finalization: the design's "after result
	// published, before outcome CAS" row. The result is authoritative regardless of the completion fact,
	// so the ACTION does not depend on it - but the fact still governs whether anything new may start.
	recoveryTable[observation{true, true, false, CompletionProven}] = Recovery{
		ActionFinalizeFromResult, true, "the active attempt has a published result and the containment domain is proven dead"}
	recoveryTable[observation{true, true, false, CompletionAbsent}] = Recovery{
		ActionFinalizeFromResult, false, "the active attempt has a published result but no completion fact is readable"}
	recoveryTable[observation{true, true, false, CompletionUnavailable}] = Recovery{
		ActionFinalizeFromResult, false, "the active attempt has a published result and this platform can prove nothing about its containment domain"}

	// An attempt is bound with no result. Every cut from the CAS through the final observation lands
	// here, and they are indistinguishable in durable state, so they share one answer: the identity fact
	// was never observed, therefore no verdict about the code may be claimed.
	recoveryTable[observation{true, false, false, CompletionProven}] = Recovery{
		ActionLedgerOnlyInterrupted, true, "the active attempt produced no result and its containment domain is proven dead"}
	recoveryTable[observation{true, false, false, CompletionAbsent}] = Recovery{
		ActionBlock, false, "the active attempt produced no result and no completion fact is readable"}
	recoveryTable[observation{true, false, false, CompletionUnavailable}] = Recovery{
		ActionBlock, false, "the active attempt produced no result and this platform can prove nothing about its containment domain"}
}

// Recover decides what to do with the durable residue of an attempt.
//
// It refuses rather than guessing. An unknown completion value, or a combination of facts with no row,
// produces an error and no decision - a recovery function that answers every input is one that answers
// inputs nobody thought about.
func Recover(r Residue) (Recovery, error) {
	switch r.Completion {
	case CompletionProven, CompletionAbsent, CompletionUnavailable:
	default:
		return Recovery{}, fmt.Errorf("%w: unknown completion fact %q", ErrLifecycle, r.Completion)
	}
	dec, ok := recoveryTable[observation{r.ActiveAttempt, r.ResultPublished, r.FinalizedEntry, r.Completion}]
	if !ok {
		return Recovery{}, fmt.Errorf("%w: no recovery is decided for active=%t result=%t finalized=%t completion=%q",
			ErrLifecycle, r.ActiveAttempt, r.ResultPublished, r.FinalizedEntry, r.Completion)
	}
	if !r.IntentPublished || r.ActiveAttempt {
		return dec, nil
	}
	// The design separates "nothing" from "an orphan intent". They reach the same action - the orphan is
	// ignored and superseded - but an operator reading this is owed the difference.
	dec.Reason = dec.Reason + "; an orphan intent is durable and is superseded, not resumed"
	return dec, nil
}

// LaunchFault is a failure with the coordinator STILL ALIVE, which is what separates this vocabulary
// from the crash table above.
//
// One distinction governs all of it. `spawn_failed` is a statement about the CONFIGURED TEST COMMAND,
// and it points an operator at the run policy. A supervisor or protocol fault is coordinator
// infrastructure, and recording it as `spawn_failed` sends somebody to debug a test command that was
// never even reached. Both map to `indeterminate`, so nothing about the routing changes - but the record
// is what a human reads, and that makes it the same family of lie as routing an infrastructure failure
// to FIX.
type LaunchFault string

const (
	// FaultSupervisorSpawn, FaultHandshake and FaultArmingDeadline all happen BEFORE the active CAS.
	FaultSupervisorSpawn LaunchFault = "supervisor_spawn"
	FaultHandshake       LaunchFault = "handshake"
	FaultArmingDeadline  LaunchFault = "arming_deadline"
	// FaultGoWrite is a GO that failed to reach a supervisor that had already died. Framing guarantees a
	// truncated GO is not a GO, so no command started.
	FaultGoWrite LaunchFault = "go_write"
	// FaultSupervisorLostBeforeSpawn and FaultSupervisorLostAfterSpawn cover a supervisor that died or
	// sent corrupt frames on either side of the command starting.
	FaultSupervisorLostBeforeSpawn LaunchFault = "supervisor_lost_before_spawn"
	FaultSupervisorLostAfterSpawn  LaunchFault = "supervisor_lost_after_spawn"
	// FaultCommandDidNotStart is the ONLY member of this vocabulary that is about the operator's command.
	FaultCommandDidNotStart LaunchFault = "command_did_not_start"
)

// AllLaunchFaults is the closed vocabulary.
func AllLaunchFaults() []LaunchFault {
	return []LaunchFault{
		FaultSupervisorSpawn, FaultHandshake, FaultArmingDeadline, FaultGoWrite,
		FaultSupervisorLostBeforeSpawn, FaultSupervisorLostAfterSpawn, FaultCommandDidNotStart,
	}
}

// LaunchDisposition is what a live coordinator does about one of those faults.
type LaunchDisposition struct {
	// AttemptBound says whether an attempt reference exists, which is decided by whether the fault
	// happened before or after the active CAS.
	AttemptBound bool
	// ResultIssued says whether an immutable result record is published. Only an authoritative statement
	// about the command produces one.
	ResultIssued bool
	// Execution is the recorded observation, empty when no attempt exists to record anything against.
	Execution state.TestExecution
	// CompletionFactRequired says whether the attempt can only become retryable once the section 1
	// cleanup fact is proven. A live-coordinator fault does not weaken that rule: the residue to be
	// excluded is identical to the crash case.
	CompletionFactRequired bool
	// BudgetSpent says whether this consumes the code-fix budget. Nothing here does - none of these
	// faults is a statement about the code.
	BudgetSpent bool
	Reason      string
}

var launchTable = map[LaunchFault]LaunchDisposition{
	FaultSupervisorSpawn: {
		Reason: "the supervisor could not be started, before anything was bound"},
	FaultHandshake: {
		Reason: "the supervisor handshake failed, before anything was bound"},
	FaultArmingDeadline: {
		Reason: "arming did not complete within its deadline, before anything was bound"},
	FaultGoWrite: {
		AttemptBound: true, Execution: state.TestExecutionInterrupted, CompletionFactRequired: true,
		Reason: "the GO could not be delivered; framing guarantees a truncated GO is not a GO, so no command started"},
	FaultSupervisorLostBeforeSpawn: {
		AttemptBound: true, Execution: state.TestExecutionInterrupted, CompletionFactRequired: true,
		Reason: "the supervisor was lost before the command spawned, so its facts are untrustworthy"},
	FaultSupervisorLostAfterSpawn: {
		AttemptBound: true, Execution: state.TestExecutionInterrupted, CompletionFactRequired: true,
		Reason: "the supervisor was lost after the command spawned; partial streams are retained"},
	FaultCommandDidNotStart: {
		AttemptBound: true, ResultIssued: true, Execution: state.TestExecutionSpawnFailed,
		CompletionFactRequired: true,
		Reason:                 "the configured test command could not be started, which is an authoritative statement about the policy"},
}

// ClassifyLaunchFault decides what one live-coordinator fault is recorded as.
//
// It refuses an unknown fault rather than choosing a default. A default here would silently be
// `spawn_failed` or `interrupted` for something nobody classified, and both of those are claims about
// where the problem is.
func ClassifyLaunchFault(f LaunchFault) (LaunchDisposition, error) {
	d, ok := launchTable[f]
	if !ok {
		return LaunchDisposition{}, fmt.Errorf("%w: no classification for the launch fault %q", ErrLifecycle, f)
	}
	return d, nil
}
