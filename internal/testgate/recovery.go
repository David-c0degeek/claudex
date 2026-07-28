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

// CompletionEvidence is the completion fact AND whose domain it is about.
//
// The fact alone was the most dangerous unbound value in this package: it is the one input that
// authorises consuming an attempt's reference and letting a new command run, and the receipt that
// carries it is attempt-bound by design. A stale receipt left by attempt A, classified as proven while
// attempt B is outstanding, would settle B and start a fresh command beside B's live processes - which
// is precisely the outcome the fact exists to prevent.
type CompletionEvidence struct {
	Fact CompletionFact
	// AttemptID is the attempt the receipt was published for, required when the fact is proven.
	AttemptID string
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
// boolean cannot establish it.
//
// This shape is enough for the INTENT, which nothing here has to execute - its presence and its digest
// are the whole of what the decisions depend on. The result needs more, and gets it below.
type Artifact struct {
	State     ArtifactState
	AttemptID string
	Digest    string
}

// ResultProjection is the part of a published result that the LEDGER binds.
//
// It is deliberately NOT called the canonical result, and it is not that. The design's record also
// carries the resolved argv and executable, the environment identity, the exit and timeout facts, and
// the retained stream excerpt with its counts, digests and truncation flags - a verifier is expected to
// inspect all of it. Calling this "the result in full" while it holds a projection was an overclaim
// that would have been read as permission to drop the rest.
//
// What it IS: enough to append the ledger entry, validated, so THAT step needs no re-read. Anything
// beyond it is reached through RecordReader below, which is a named capability rather than an ambient
// lookup.
type ResultProjection struct {
	AttemptID      string
	TestedCommit   string
	TestedTree     string
	Execution      state.TestExecution
	Identity       state.TestIdentity
	TerminalReason string
	// Digest is the canonical digest of the WHOLE record, and the identity state binds.
	Digest string
}

// RecordReader re-reads an immutable canonical record and proves it is the one named.
//
// This exists so that "the omitted evidence is still reachable" is a contract with a shape rather than
// a reassurance. An implementation MUST verify that the bytes it returns hash to the requested digest
// and fail otherwise; a reader that merely fetches whatever is at a path is the ambient re-read this
// package refuses, wearing a type.
type RecordReader interface {
	ReadRecord(digest string) ([]byte, error)
}

// StreamEvidence is the non-authoritative crash-time staging an interrupted attempt leaves behind.
//
// The design keeps these leaves explicitly separate from what state binds: they are partial evidence,
// retained so an interrupted attempt is not silently evidence-free. Recovery may find them present or
// absent, and cannot tell from that which cut it is looking at - the cut before GO and the cut after it
// are indistinguishable, which is the same reason those rows share one decision.
type StreamEvidence struct {
	State        ArtifactState
	StdoutDigest string
	StderrDigest string
	StdoutBytes  uint64
	StderrBytes  uint64
	Truncated    bool
}

// InterruptionPlan is the result record recovery must PUBLISH before it can settle an interrupted
// attempt.
//
// It exists because settling that attempt appends a ledger entry, every ledger entry binds a required
// canonical result digest, and the attempt that reaches this row has no published result by definition.
// Without the plan the action names something that cannot be carried out: there would be no record to
// point the entry at, and whoever executed it would have to invent the execution, the identity and the
// terminal reason - which are exactly the facts nobody is entitled to invent after a crash.
//
// The digest is absent on purpose. It is the digest of bytes that do not exist yet, and this decision
// is what authorises writing them.
type InterruptionPlan struct {
	AttemptID    string
	TestedCommit string
	TestedTree   string
	// Execution and Identity are fixed for this row and carried rather than left implicit: the runner
	// never reported an ending, and no identity observation was ever made, so nothing about the code may
	// be claimed.
	Execution      state.TestExecution
	Identity       state.TestIdentity
	TerminalReason string
	// RetainedStreams is the partial evidence found on disk, to be embedded in the published record.
	RetainedStreams StreamEvidence
}

// ResultEvidence is what was found when the result was looked for.
type ResultEvidence struct {
	State ArtifactState
	// Record is the validated ledger projection, required when the state is valid and forbidden
	// otherwise - one shape per state, so "absent" cannot arrive carrying a record that something later
	// decides to trust.
	Record *ResultProjection
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
	Result ResultEvidence
	// RetainedStreams is the crash-time staging for an attempt that produced no result.
	RetainedStreams StreamEvidence
	// Completion is the section 1 evidence about the attempt's containment domain.
	Completion CompletionEvidence
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
	// Attempt is the reference the action is about, present when the standing was active. It is a CLONE:
	// the caller must not be able to rewrite the attempt id, revision or bound digests after they were
	// validated and before the action is carried out.
	Attempt *state.TestAttemptRef
	// Result is the validated ledger projection to finalize from, present only for
	// ActionFinalizeFromResult. Also a clone, for the same reason. Everything the projection omits is
	// reached through a RecordReader using Result.Digest.
	Result *ResultProjection
	// Publish is the record that must be written BEFORE the ledger entry, present only for
	// ActionLedgerOnlyInterrupted. Without it that action would name a settlement nobody could perform.
	Publish *InterruptionPlan
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
		// A result belonging to no attempt is rejected as inconsistent evidence before the table is
		// consulted, so this standing exists only without one.
		out = append(out, observation{StandingNone, false, c})
		for _, r := range []bool{false, true} {
			out = append(out, observation{StandingActive, r, c})
		}
		// A ledger entry BINDS a required canonical result digest, and state references a result only
		// once that record is durable. So a finalized attempt whose result is missing is not a state
		// that can exist - "ledger-only" means no outcome edge and no budget spent, not an entry
		// pointing at nothing.
		out = append(out, observation{StandingFinalized, true, c})
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
		recoveryTable[observation{StandingFinalized, true, c}] = Recovery{
			Action: ActionFreshAttempt, Reason: "the most recent attempt is settled in the ledger"}
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
	if bad := r.shapes(); bad != "" {
		return Recovery{Action: ActionBlock, Attempt: cloneAttemptRef(r.Active), Reason: bad}, nil
	}
	if bad := r.inconsistency(); bad != "" {
		// CLONED here too. The contract on Recovery.Attempt is unconditional, and an operator-facing
		// block that names an attempt the caller can rename afterwards is worse than one that names
		// none - it is a report that quietly becomes about something else.
		return Recovery{Action: ActionBlock, Attempt: cloneAttemptRef(r.Active), Reason: bad}, nil
	}

	dec, ok := recoveryTable[observation{r.Standing, r.Result.State == ArtifactValid, r.Completion.Fact}]
	if !ok {
		return Recovery{}, fmt.Errorf("%w: no recovery is decided for standing=%q result=%q completion=%q",
			ErrLifecycle, r.Standing, r.Result.State, r.Completion.Fact)
	}
	// CLONED, not aliased. The caller supplied these and would otherwise keep a handle into the values
	// that were just validated, free to rewrite the attempt id or the bound digests between the decision
	// and the action it authorises.
	dec.Attempt = cloneAttemptRef(r.Active)
	switch dec.Action {
	case ActionFinalizeFromResult:
		dec.Result = cloneResult(r.Result.Record)
	case ActionLedgerOnlyInterrupted:
		// Settling this attempt appends a ledger entry, and every entry binds a durable canonical
		// result - which this attempt does not have. So the decision carries the record to write, built
		// from the identity the attempt was authorised against and the partial evidence found on disk.
		dec.Publish = &InterruptionPlan{
			AttemptID:    r.Active.AttemptID,
			TestedCommit: r.Active.TestedCommit,
			TestedTree:   r.Active.TestedTree,
			// Fixed for this row, and carried rather than left implicit. The runner never reported an
			// ending and no identity observation was ever made, so nothing about the code may be claimed.
			Execution:       state.TestExecutionInterrupted,
			Identity:        state.TestIdentityUnobserved,
			TerminalReason:  state.CanonicalTerminalReason("the attempt was interrupted and no result was published"),
			RetainedStreams: r.RetainedStreams,
		}
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
	switch r.Completion.Fact {
	case CompletionProven, CompletionAbsent, CompletionUnavailable:
	default:
		return fmt.Errorf("%w: unknown completion fact %q", ErrLifecycle, r.Completion.Fact)
	}
	for _, a := range []struct {
		what string
		st   ArtifactState
	}{{"intent", r.Intent.State}, {"result", r.Result.State}, {"retained stream", r.RetainedStreams.State}} {
		switch a.st {
		case ArtifactAbsent, ArtifactValid, ArtifactInvalid:
		default:
			return fmt.Errorf("%w: unknown %s artifact state %q", ErrLifecycle, a.what, a.st)
		}
	}
	return nil
}

// ledgerDisagreesWithRecord names the first bound field on which the ledger entry and the record it
// names differ, or "" when they agree.
func ledgerDisagreesWithRecord(e state.FinalizedAttempt, rec ResultProjection) string {
	for _, f := range []struct{ name, ledger, record string }{
		{"tested commit", e.TestedCommit, rec.TestedCommit},
		{"tested tree", e.TestedTree, rec.TestedTree},
		{"execution", string(e.Execution), string(rec.Execution)},
		{"identity", string(e.Identity), string(rec.Identity)},
		{"terminal reason", e.TerminalReason, rec.TerminalReason},
	} {
		if f.ledger != f.record {
			return fmt.Sprintf("attempt %q is finalized saying %s %q, but the record it names says %q",
				e.AttemptID, f.name, f.ledger, f.record)
		}
	}
	return ""
}

// shapes rejects evidence that has more than one way of saying the same thing.
//
// A fact with two representations is a fact two readers can disagree about, and here the readers are a
// recovering process and whatever wrote the residue. Absent must not arrive carrying a record, and a
// proof must not arrive without the attempt it proves - otherwise "no record" and "a record nobody
// looked at" become indistinguishable, and so do "unproven" and "proven for somebody else".
func (r Residue) shapes() string {
	switch r.Completion.Fact {
	case CompletionProven:
		if r.Completion.AttemptID == "" {
			return "the completion fact is proven but names no attempt"
		}
	default:
		if r.Completion.AttemptID != "" {
			return fmt.Sprintf("the completion fact is %q but still names attempt %q", r.Completion.Fact, r.Completion.AttemptID)
		}
	}
	switch r.Result.State {
	case ArtifactValid:
		if r.Result.Record == nil {
			return "the result is reported valid with no record behind it"
		}
	default:
		if r.Result.Record != nil {
			return fmt.Sprintf("the result is %q but still carries a record", r.Result.State)
		}
	}
	if r.Intent.State != ArtifactValid && (r.Intent.AttemptID != "" || r.Intent.Digest != "") {
		return fmt.Sprintf("the intent is %q but still carries an identity or digest", r.Intent.State)
	}
	if r.RetainedStreams.State != ArtifactValid &&
		(r.RetainedStreams.StdoutDigest != "" || r.RetainedStreams.StderrDigest != "" ||
			r.RetainedStreams.StdoutBytes != 0 || r.RetainedStreams.StderrBytes != 0 || r.RetainedStreams.Truncated) {
		return fmt.Sprintf("the retained streams are %q but still carry evidence", r.RetainedStreams.State)
	}
	return ""
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
			if r.Result.Record.AttemptID != r.Active.AttemptID {
				return fmt.Sprintf("attempt %q is active but the durable result belongs to %q", r.Active.AttemptID, r.Result.Record.AttemptID)
			}
			// A result is a statement ABOUT a particular tree. Finalizing from one that was computed
			// against different code would publish a verdict about something this attempt never ran.
			if r.Result.Record.TestedCommit != r.Active.TestedCommit || r.Result.Record.TestedTree != r.Active.TestedTree {
				return fmt.Sprintf("attempt %q is a statement about %s/%s but its result is about %s/%s",
					r.Active.AttemptID, r.Active.TestedCommit, r.Active.TestedTree,
					r.Result.Record.TestedCommit, r.Result.Record.TestedTree)
			}
		}
		// The proof that authorises consuming this reference must be a proof about THIS attempt. The
		// receipt is attempt-bound by design, and a stale one from an earlier attempt would otherwise
		// settle a live one and release a new command beside its processes.
		if r.Completion.Fact == CompletionProven && r.Completion.AttemptID != r.Active.AttemptID {
			return fmt.Sprintf("the completion fact proves the domain of attempt %q, not the outstanding %q",
				r.Completion.AttemptID, r.Active.AttemptID)
		}
	case StandingFinalized:
		if r.Finalized == nil {
			return "the ledger is said to hold a finalization but no entry was supplied for it"
		}
		if r.Active != nil {
			return fmt.Sprintf("attempt %q is finalized but an active reference was supplied too", r.Finalized.AttemptID)
		}
		// A ledger entry binds a required canonical result digest, and state references a result only
		// once that record is durable. An entry pointing at a record that is missing or unreadable is
		// therefore inconsistent evidence, not a "ledger-only" finalization - that phrase means no
		// outcome edge and no budget spent, not an entry with nothing behind it.
		switch r.Result.State {
		case ArtifactAbsent:
			return fmt.Sprintf("attempt %q is finalized against result %q but no result is durable",
				r.Finalized.AttemptID, r.Finalized.ResultDigest)
		case ArtifactInvalid:
			return fmt.Sprintf("attempt %q is finalized but its result could not be validated", r.Finalized.AttemptID)
		}
		if r.Result.Record.AttemptID != r.Finalized.AttemptID {
			return fmt.Sprintf("attempt %q is finalized but the durable result belongs to %q",
				r.Finalized.AttemptID, r.Result.Record.AttemptID)
		}
		if r.Result.Record.Digest != r.Finalized.ResultDigest {
			return fmt.Sprintf("attempt %q is finalized against result %q but the durable result is %q",
				r.Finalized.AttemptID, r.Finalized.ResultDigest, r.Result.Record.Digest)
		}
		// Naming the right record is not the same as copying it correctly. The ledger entry holds its own
		// copies of the tested identity and the verdict, so an entry can point at the real record and
		// still disagree with it about what happened - and state cannot catch that, because the record is
		// external to it. This is the case slice 3b's disagreesWithPublished exists for, on the other
		// side of the same boundary.
		if bad := ledgerDisagreesWithRecord(*r.Finalized, *r.Result.Record); bad != "" {
			return bad
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

// cloneAttemptRef and cloneResult hand out owned copies.
//
// Slice 3b had to learn this at every collaborator boundary: a validated value reachable through a
// pointer the caller still holds is a value that can change between the check and the use.
func cloneAttemptRef(r *state.TestAttemptRef) *state.TestAttemptRef {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func cloneResult(r *ResultProjection) *ResultProjection {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func clonePlan(p *InterruptionPlan) *InterruptionPlan {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}
