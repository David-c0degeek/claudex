// Package testgate owns the ORDER in which one mechanical test attempt happens, and the FACTS that
// travel between its steps.
//
// Every step it sequences already exists somewhere else — the run guard, the attempt store, the
// containment, the identity observation, the state CAS. What does not exist anywhere else are two
// things: the guarantee that they happen in the one order that makes each step's facts true when the
// next step reads them, and the guarantee that what was armed, published, observed, hashed and bound is
// the SAME execution. Neither can live in a call-site convention, because a convention is followed by
// whoever remembers it.
//
// The second guarantee is why the seams pass typed values rather than bare errors. An earlier draft
// sequenced the calls correctly and carried nothing between them: authorization resolved a command that
// intent publication and arming could not see, and the runner's terminal facts vanished into an
// `error` so the identity observer had to MANUFACTURE them. Production wiring would then have had to
// recreate exactly the hidden mutable side channels this design rejects everywhere else.
//
// The order, and why each step is where it is:
//
//  1. UNDER THE RUN GUARD, authorize: exactly ownerless TESTS with NO active attempt, the frozen
//     identity proven, the command resolved and the record's fit re-checked. All of it before anything
//     is minted, so a resolution or fit failure is a refusal that wrote nothing.
//  2. Durably publish the immutable intent.
//  3. ARM THE CONTAINMENT DORMANT — no command in it yet.
//  4. CAS-bind the active attempt.
//  5. Release the run guard.
//  6. Send GO. Only now does a command exist.
//  7. Execute. The subprocess is entirely outside the lock.
//  8. Reacquire the run guard.
//  9. Re-authorize the exact attempt and make the FINAL identity observation.
//  10. Construct and durably publish the ONE canonical result.
//  11. CAS the outcome, then release.
//
// Two of those positions are corrections of earlier drafts, and both are the same kind of mistake in
// opposite directions:
//
//   - The result is published AFTER the final observation (step 9 → 10), not before. The record is
//     immutable and contains the `unchanged` versus `changed` distinction, so publishing it earlier
//     would have asserted a fact that did not yet exist.
//   - The containment is armed BEFORE the CAS (step 3 → 4), not after. A durable fact that recovery
//     depends on must exist before the state that obliges recovery to read it; otherwise the crash row
//     between them is unrecoverable by construction, because nothing existed that could publish the
//     proof.
//
// Arming before the CAS is also what serializes a late operator cancel against outcome finalization.
package testgate

import (
	"errors"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
)

// The four outcome classes, which differ in WHAT SURVIVES rather than in severity. Collapsing them was
// a real defect: a caller cannot decide what to do next without knowing whether an attempt exists.
var (
	// ErrRefused means NOTHING durable was written: no intent, no containment, no state. Authorization
	// and resolution failures only. A caller may retry freely.
	ErrRefused = errors.New("testgate: refused before anything was written")

	// ErrOrphanedIntent means an intent may exist on disk and possibly a containment was armed and torn
	// down again — but there is NO active reference and NO command. The design treats an orphan intent
	// as ignorable and a fresh attempt as correct, which is why this is retryable; it is a separate
	// class from ErrRefused because the promise is weaker and saying otherwise would be a lie.
	ErrOrphanedIntent = errors.New("testgate: an orphan intent may exist, but no attempt was bound")

	// ErrLifecycle means an attempt EXISTS and this process tore it down. The command's own failure is
	// never reported this way: one is an infrastructure fault, the other is the answer the gate exists
	// to produce.
	ErrLifecycle = errors.New("testgate: attempt lifecycle failed")

	// ErrRecoveryOwned means an attempt MAY exist and this process must not tear anything down.
	//
	// It exists for one situation: a state append that was visible but whose durability could not be
	// confirmed. Closing the containment there would destroy the only handle that proves the domain is
	// gone — on Windows, the sole job handle — producing exactly the post-CAS row with no readable fact
	// that recovery must BLOCK on. Leaving it to recovery is the honest outcome.
	ErrRecoveryOwned = errors.New("testgate: an attempt may exist; recovery owns the containment and the state")
)

// ExecutionSpec is the COMPLETE description of the process arming must create.
//
// It carries the environment BYTES, not merely their digest, and that is the whole correction. A digest
// identifies an environment; it cannot be handed to a supervisor, so arming would have had to re-read
// state or reach through a captured side channel to obtain the values — which is the precise defect this
// carrier exists to remove. The digest stays, as the identity of these bytes rather than a substitute
// for them.
//
// cwd is here for the same reason: the run worktree is where the command must execute, and a step that
// had to look it up elsewhere could look up a different one.
type ExecutionSpec struct {
	// Executable is the absolute path selected against the FROZEN PATH, never an ambient lookup.
	Executable string
	// Argv is the full vector including argv[0]. Empty later elements are meaningful and preserved.
	Argv []string
	// Cwd is the run worktree.
	Cwd string
	// Env is the frozen environment, ordered, as raw name/value BYTES — an inherited Unix value need not
	// be valid UTF-8, and the child must receive it byte for byte.
	Env config.ResolvedExecution
	// Digest is the canonical exec.spec.v1 artifact digest - the bytes the supervisor is handed and
	// checks. It is bound in BOTH the intent and the result.
	//
	// It is deliberately not the ExecutionView's digest. The view carries the environment IDENTITY, the
	// spec carries its ordered VALUES, so neither can stand in for the other; leaving one of them
	// unbound with a prose sentence relating them would have been two identities and one check.
	Digest string
}

// View renders the spec as the ONE execution description the intent and the result both bind.
//
// Building it here rather than at each carrier is what makes "the result describes the execution that
// was armed" a single digest comparison instead of six hand-written field checks - the kind that
// silently omits whichever field was added last.
func (s ExecutionSpec) View() (ExecutionView, error) {
	envDigest, err := s.Env.EnvIdentityDigest()
	if err != nil {
		return ExecutionView{}, fmt.Errorf("%w: digesting the environment identity: %v", ErrLifecycle, err)
	}
	argv := make(ByteList, 0, len(s.Argv))
	for _, a := range s.Argv {
		argv = append(argv, Bytes(a))
	}
	names := make(ByteList, 0, len(s.Env.Env))
	for _, e := range s.Env.Env {
		names = append(names, append(Bytes(nil), e.Name...))
	}
	return ExecutionView{
		Executable: Bytes(s.Executable),
		Argv:       argv,
		Cwd:        Bytes(s.Cwd),
		EnvNames:   names,
		EnvDigest:  envDigest,
	}, nil
}

// PreparedAttempt is everything the guarded pre-flight established, travelling as ONE value.
//
// It is the answer to "what was authorized": the identity the attempt is a statement about, the command
// that will run, and the digests that bind them. Every later step takes it, so no step has to reconstruct
// a fact an earlier one already proved.
type PreparedAttempt struct {
	// AttemptID is the minted identity every later fact binds.
	AttemptID string
	// StartRevision is the state revision at which this attempt becomes active — the SAME number the
	// active CAS will commit at, not an approximation of it.
	//
	// The intent is published before that CAS and its schema binds this value, so it has to be known
	// beforehand. It cannot be computed as head+1: the generation store deliberately skips occupied and
	// quarantined slots, so the next generation is whatever it turns out to be, and state validates the
	// attempt's start revision against the revision that actually created it. It is therefore RESERVED
	// under the held guard, which is the window in which the answer cannot change, and the binding CAS is
	// required to have committed exactly it.
	StartRevision uint64
	// TestedCommit and TestedTree are the repository identity this attempt is a statement ABOUT.
	TestedCommit string
	TestedTree   string
	// IntentDigest binds the exact published intent.
	IntentDigest string
	// Spec is what will run, complete. Carried because arming and intent publication both need the exact
	// command AND environment authorization chose, and neither may construct its own.
	Spec ExecutionSpec
	// MaxOutputBytes is the frozen policy ceiling on the COMBINED retained stdout+stderr excerpt, in RAW
	// bytes. MaxRecordBytes is the ceiling on the CANONICAL result record.
	//
	// They are separate because they measure different things and neither implies the other: the excerpt
	// is base64 in the record, and the argv, environment names and terminal account are variable-length
	// metadata that can make even a zero-output record nearly exhaust the record ceiling. Checking the
	// raw excerpt alone does not prove the encoded record fits anywhere.
	//
	// Both travel with the attempt because this lifecycle has to enforce them and does not read policy.
	MaxOutputBytes uint64
	MaxRecordBytes uint64
}

// Terminal is what the RUNNER observed about the command, in the closed vocabulary.
//
// It comes back from Wait rather than being reconstructed later, because the runner is the only party
// that saw the command end. An earlier draft returned only an error here, so these facts disappeared and
// the identity observer had to invent them.
type Terminal struct {
	Execution      state.TestExecution
	TerminalReason string // already canonical; see state.CanonicalTerminalReason
	// HasExitCode/ExitCode carry the optional exit fact BY VALUE.
	//
	// A pointer would be validated once and then travel as a shallow alias through identity observation,
	// result publication and both CAS seams — and the runner keeps its own copy of it too. A collaborator
	// setting *ExitCode = 17 after `ok + 0` was accepted leaves outcome derivation still producing a
	// pass while publication records a command that failed, and nothing downstream can catch it: the
	// ledger does not store the exit code. Optionality is worth a second field; a mutable alias to a
	// validated fact is not.
	HasExitCode bool
	ExitCode    int
	// Author says WHO observed this ending. The runner ordinarily did; a live coordinator that watched
	// its supervisor die authors `interrupted`, because nobody saw the command end and the runner is not
	// there to say so. Without it the record could not be built truthfully, and the ledger field would
	// have to be guessed at publication time.
	Author state.TerminalAuthor
	// Stdout and Stderr are the runner's validated stream evidence, carried here because the runner is
	// the only party that saw the streams. An earlier version left them out, so whoever published the
	// result had to find them again through the side channel this package exists to remove.
	Stdout StreamRecord
	Stderr StreamRecord
}

// Identity is the FINAL identity observation, in the closed vocabulary.
//
// `unobserved` is a first-class value, not an error. Its combinations produce durable indeterminate
// outcomes by design, so an observation that could not be made must be RECORDED as unobserved rather
// than recast as an unrecorded lifecycle failure — the gate is then honest about what it could not see,
// instead of silently dropping the attempt.
type Identity struct {
	Value state.TestIdentity
	// Commit and Tree are the identity at the END of the attempt, empty when unobserved.
	Commit string
	Tree   string
}

// CommitStatus is the AUTHORITATIVE outcome of a state compare-and-swap.
//
// Three outcomes, deliberately not collapsed into `error`. A clean conflict means no attempt exists and
// the containment is this process's to destroy; a confirmed commit means an attempt exists; and a
// visible-but-unconfirmed append means it MAY exist, which is the one case where tearing down would
// destroy the evidence recovery needs.
type CommitStatus int

const (
	// BindCommitted: the append is durable. An attempt exists.
	BindCommitted CommitStatus = iota + 1
	// BindNotCommitted: nothing was written — a clean revision conflict, for instance. No attempt exists.
	BindNotCommitted
	// BindUncertain: the append may be visible but its durability is unconfirmed.
	BindUncertain
)

func (b CommitStatus) String() string {
	switch b {
	case BindCommitted:
		return "committed"
	case BindNotCommitted:
		return "not-committed"
	case BindUncertain:
		return "uncertain"
	}
	return "unknown"
}

// Containment is the armed domain the command runs inside.
type Containment interface {
	// Go starts the command. Before this call no command exists.
	Go() error
	// Wait blocks until the command is finished and its domain is torn down, and returns what the
	// runner observed.
	Wait() (Terminal, error)
	// Close releases the containment on every path, including those where Go was never called.
	Close() error
}

// Deps are the collaborators, each an authority that exists elsewhere.
type Deps struct {
	// AcquireGuard and ReleaseGuard bracket the two locked sections. The subprocess runs between them.
	AcquireGuard func() error
	ReleaseGuard func() error

	// ReserveStartRevision returns the revision the next state append will commit at, under the guard
	// this lifecycle already holds.
	//
	// It exists because the intent binds that revision and is published BEFORE the append. Guessing
	// head+1 would be wrong whenever the store skips an occupied or quarantined slot, and the guess
	// would be baked into a durable digest.
	//
	// The reserved value CONSTRAINS the binding append: it must commit at exactly this revision or
	// report BindNotCommitted. It is not a prediction that the caller then reconciles afterwards. State
	// requires a new active attempt's start revision to equal the revision that created it, and refuses
	// anything else BEFORE serialization, so "committed somewhere else" is not a durable state that can
	// exist — an intervening append means the slot is gone, the binding does not commit, and the residue
	// is the orphaned intent the design already treats as ignorable.
	ReserveStartRevision func() (uint64, error)

	// Authorize proves ownerless TESTS with no active attempt, freezes the identity, resolves the
	// command and re-checks the record fit — returning all of it as one value, INCLUDING the reserved
	// start revision it was given, because that revision is part of the intent it digests. It mints
	// nothing until every check has passed, so its failure writes nothing.
	Authorize func(startRevision uint64) (PreparedAttempt, error)

	// PublishIntent durably writes the immutable intent and returns the digest of the EXACT canonical
	// bytes it wrote.
	//
	// Returning the digest is what makes "published and armed are the same execution" checkable rather
	// than assumed: without it, a publisher could persist one command while arming ran another, and
	// isolating the later calls from mutation would not have revealed it — the two would simply differ.
	//
	// Durability uncertainty here is BENIGN and needs no status: either the intent is on disk and
	// orphaned, or it is not, and the design treats an orphan intent as ignorable with a fresh attempt
	// superseding it. Nothing acts on an intent that no active reference points at.
	PublishIntent func(IntentRecord) (digest string, err error)

	// ArmContainment arms the containment with NO command in it. It returns the Containment even on
	// failure when anything was partially armed, so the caller can always release what exists.
	ArmContainment func(PreparedAttempt) (Containment, error)

	// BindActive CASes the active attempt into state at EXACTLY PreparedAttempt.StartRevision, and
	// reports the AUTHORITATIVE status together with the revision it committed at.
	//
	// Committing anywhere else is not permitted, and not merely discouraged: state validates a new
	// active attempt's start revision against the revision that created it and refuses the record
	// before it is serialized. If the reserved slot is no longer available, this reports
	// BindNotCommitted. The revision comes back so the lifecycle can VERIFY that contract rather than
	// assume it.
	BindActive func(PreparedAttempt) (Bind, error)

	// ConfirmBind settles an uncertain append while the guard is STILL HELD. It is a separate seam
	// because that is the only moment the uncertainty can be resolved cheaply — afterwards the answer
	// belongs to recovery.
	ConfirmBind func(PreparedAttempt) (Bind, error)

	// Reauthorize proves the exact attempt is still the active one after the guard is reacquired.
	Reauthorize func(PreparedAttempt) error

	// ObserveIdentity makes the final exact identity observation, under the guard, given what the
	// runner saw. It may legitimately return `unobserved`; it returns an error only when the
	// observation itself could not be attempted.
	ObserveIdentity func(PreparedAttempt, Terminal) (Identity, error)

	// PublishResult durably writes the ONE canonical result and returns its digest.
	//
	// It receives the RECORD, already built and validated, rather than the pieces. Handing over the
	// pieces meant the publisher had to assemble the canonical record itself - and it had no way to
	// reach the stream evidence or the terminal authority, so it would have had to find them through
	// exactly the side channel this package exists to remove.
	PublishResult func(ResultRecord) (digest string, err error)

	// FinalizeOutcome CASes the outcome into state, moving the active ref into the ledger, and reports
	// the AUTHORITATIVE status — the same three-way answer the active binding gives, for the same
	// reason: "it failed" does not say whether the outcome was applied.
	FinalizeOutcome func(PreparedAttempt, Terminal, Identity, string) (CommitStatus, error)

	// ConfirmFinalize settles an uncertain outcome append while the second guard is STILL HELD.
	//
	// It receives the EXPECTED tuple, and returns the entry it actually found. Given only the attempt
	// id it could answer no better than "some finalization exists", which is not the question — an
	// implementation would have had to capture the expectation through the side channel this package
	// exists to eliminate, or accept any ledger entry for the attempt, including one written by another
	// party with a different result. Confirmation READS rather than retries: the design's row is
	// "re-confirm; never double-apply", because re-applying an outcome that did commit would bind a
	// second verdict to one attempt.
	ConfirmFinalize func(PreparedAttempt, Terminal, Identity, string) (FinalizeConfirmation, error)
}

// Handoff transfers a live containment to whoever called Run, for the cases where this function must
// NOT tear it down.
//
// Returning the error alone was not ownership transfer, it was abandonment: the containment became an
// unreachable local, and on Windows the sole handle to the unnamed job - the very fact being preserved -
// went with it. Naming the recipient is what makes "recovery owns this" true rather than aspirational.
type Handoff struct {
	// Prepared is the attempt the containment belongs to, so the recipient can identify what it holds.
	Prepared PreparedAttempt
	// Containment is still ARMED and must be released by the recipient, not by this function.
	Containment Containment
	// Guard is what is known about the run guard, TYPED.
	//
	// Without it the recipient cannot act: reacquiring a guard this process still holds risks
	// self-deadlock, and proceeding under a guard that was actually released risks unlocked mutation. A
	// failed release establishes NEITHER, and no free-text reason can be reasoned about — which is why
	// this is an enum the recipient must handle rather than prose it must interpret.
	Guard GuardDisposition
	// Reason states why ownership moved, for a human reading a log. Nothing decides on it.
	Reason string
}

// GuardDisposition is what is known about the run guard at the moment of a handoff.
type GuardDisposition int

const (
	// GuardReleased: the guard was released successfully. Recovery may acquire it normally.
	GuardReleased GuardDisposition = iota + 1
	// GuardHeld: this process deliberately kept the guard. Recovery must not acquire it.
	//
	// Run never produces this today — it always attempts release — and a test pins that, so the claim
	// is checked rather than assumed. It is part of the vocabulary because the recipient's handling
	// must be total: a value that appears later must not fall into a default that guesses.
	GuardHeld
	// GuardUnknown: the release FAILED, which establishes neither disposition.
	//
	// Recovery must BLOCK guarded work here rather than choose, and must retain the containment — the
	// conservative direction, because both alternatives are unsafe and only one of them is loud.
	GuardUnknown
)

func (g GuardDisposition) String() string {
	switch g {
	case GuardReleased:
		return "released"
	case GuardHeld:
		return "held"
	case GuardUnknown:
		return "unknown"
	}
	return "unset"
}

// FinalizeConfirmation is the authoritative answer about an uncertain outcome append.
type FinalizeConfirmation struct {
	Status CommitStatus
	// Entry is the ledger record that is bound, and must be present when Status is committed —
	// otherwise "committed" is an assertion with nothing behind it.
	//
	// It is the STATE type rather than a shape local to this package. A parallel copy carrying the
	// fields this package happened to think of drifts from the record it claims to describe: the first
	// version omitted TerminalReason, so an entry with the same digest and verdict but a different
	// reason compared equal to a result that does not contain it.
	Entry *state.FinalizedAttempt
}

// Bind is the authoritative answer about the active-attempt CAS.
type Bind struct {
	Status CommitStatus
	// Revision is the revision the append committed at, meaningful when Status is committed.
	Revision uint64
}

// Result is what one completed attempt produced.
type Result struct {
	// Recovery is non-nil exactly when this function deliberately did not release the containment.
	Recovery     *Handoff
	Prepared     PreparedAttempt
	Terminal     Terminal
	Identity     Identity
	ResultDigest string
	// Outcome is the verdict derived from the two independent observations by the one total function
	// that owns that decision.
	Outcome state.TestOutcome
}

// Run executes one attempt in the pinned order.
//
// The guard is held for two disjoint sections and released in between. That is the entire reason this
// function is shaped the way it is: a mechanical test can run for minutes, and holding a lock the rest
// of the run needs for that long would make the gate a serialization point rather than a check.
func Run(d Deps) (Result, error) {
	if err := d.validate(); err != nil {
		return Result{}, err
	}

	// ---- First guarded section -------------------------------------------------------------------
	if err := d.AcquireGuard(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: acquiring the run guard", ErrRefused), err)
	}

	prep, cont, _, err := d.armUnderGuard()
	if err != nil {
		// Teardown ownership depends on WHAT SURVIVED, which is why the status is carried out here
		// rather than folded into the error. If an attempt may exist, closing the containment would
		// destroy the only proof recovery can use - so it is HANDED OVER rather than merely not closed.
		if errors.Is(err, ErrRecoveryOwned) {
			// The disposition is decided by what the release ACTUALLY did, so it is filled in after the
			// attempt rather than predicted before it.
			h := &Handoff{Prepared: clonePrepared(prep), Containment: cont, Guard: GuardReleased,
				Reason: "the active binding could not be resolved, so an attempt may exist"}
			if rerr := d.ReleaseGuard(); rerr != nil {
				h.Guard = GuardUnknown
				err = errors.Join(err, fmt.Errorf("%w: releasing the run guard", ErrLifecycle), rerr)
			}
			return Result{Recovery: h}, err
		}
		cerr := closeContainment(cont)
		if rerr := d.ReleaseGuard(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("%w: releasing the run guard after a refusal", ErrLifecycle), rerr)
		}
		return Result{}, errors.Join(err, cerr)
	}

	if err := d.ReleaseGuard(); err != nil {
		// The state is DEFINITELY bound and the guard may still be held. Closing the containment here
		// would volunteer for the Windows post-CAS row with no readable fact - destroying the sole
		// handle over a lock failure that says nothing about the domain. It is handed to recovery.
		return Result{Recovery: &Handoff{Prepared: clonePrepared(prep), Containment: cont,
				Guard:  GuardUnknown,
				Reason: "the attempt is bound and the run guard could not be released"}},
			errors.Join(fmt.Errorf("%w: releasing the run guard before GO", ErrRecoveryOwned), err)
	}

	// ---- Outside the lock ------------------------------------------------------------------------
	//
	// GO is sent AFTER the guard is released, so the command's entire lifetime is outside the lock.
	if err := cont.Go(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: starting the command", ErrLifecycle), err, closeContainment(cont))
	}
	term, err := cont.Wait()
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: awaiting the command", ErrLifecycle), err, closeContainment(cont))
	}
	// Canonicalized and shape-checked THE MOMENT the runner fact is accepted, before anything hashes or
	// publishes it. "Already canonical" was only a comment, so a containment returning a token-shaped
	// spawn detail reached the durable result seam raw - and the later state boundary can refuse such a
	// record but cannot un-write it from disk.
	term, err = acceptTerminal(term)
	if err != nil {
		return Result{}, errors.Join(err, closeContainment(cont))
	}

	// ---- Second guarded section ------------------------------------------------------------------
	if err := d.AcquireGuard(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: reacquiring the run guard", ErrLifecycle), err, closeContainment(cont))
	}
	res, ferr := d.finalizeUnderGuard(prep, term)
	rerr := d.ReleaseGuard()
	cerr := closeContainment(cont)
	if ferr != nil || rerr != nil || cerr != nil {
		var problems []error
		if ferr != nil {
			problems = append(problems, ferr)
		}
		if rerr != nil {
			problems = append(problems, fmt.Errorf("%w: releasing the run guard after finalization", ErrLifecycle), rerr)
		}
		if cerr != nil {
			problems = append(problems, cerr)
		}
		return res, errors.Join(problems...)
	}
	return res, nil
}

// armUnderGuard is steps 1 to 4: authorize, publish the intent, arm dormant, CAS.
//
// The containment is returned even on failure, so the caller can always release what exists — and the
// bind status is returned so the caller can tell whether releasing is its job at all.
func (d Deps) armUnderGuard() (PreparedAttempt, Containment, CommitStatus, error) {
	// 0. The revision the binding CAS will commit at, reserved while the guard is held. The intent
	// published in step 2 binds this number, so it must exist before the intent is hashed — and it must
	// be READ rather than predicted, because the store skips occupied and quarantined slots.
	startRevision, err := d.ReserveStartRevision()
	if err != nil {
		return PreparedAttempt{}, nil, 0, errors.Join(fmt.Errorf("%w: reserving the start revision", ErrRefused), err)
	}

	// 1. Nothing is written until every check has passed, so this failure leaves no residue at all.
	authorized, err := d.Authorize(startRevision)
	if err != nil {
		return PreparedAttempt{}, nil, 0, errors.Join(fmt.Errorf("%w: authorizing the attempt", ErrRefused), err)
	}
	// The authorizer does not get to change the reservation. It receives the revision so it can bind it
	// into the intent it digests; returning a different one would publish an intent naming a revision
	// this lifecycle never reserved.
	if authorized.StartRevision != startRevision {
		return PreparedAttempt{}, nil, 0, fmt.Errorf("%w: authorization bound start revision %d, reserved %d",
			ErrRefused, authorized.StartRevision, startRevision)
	}
	// Cloned IMMEDIATELY on return, so this function owns the master copy outright. Anything the
	// authorizer still holds a reference to cannot reach through and change what later steps receive.
	prep := clonePrepared(authorized)

	// 2. The intent is durable before anything can act on it. From HERE the strict no-residue promise no
	// longer holds: an orphan intent may exist, which is why later failures carry a weaker class.
	// The intent is BUILT and ENCODED here, so its digest is computed rather than taken on trust. The
	// previous version accepted whatever digest authorization supplied and whatever the publisher
	// reported, which is the same self-reported-identity defect the result path had to be corrected for.
	intent, expected, err := buildIntentRecord(prep)
	if err != nil {
		// NOTHING durable exists yet - no intent, no containment, no active reference - so this belongs
		// to the refusal class with the digest mismatch below it. Returning the builder's error unchanged
		// reported "an attempt exists and was torn down" for a spec that was never written anywhere.
		return prep, nil, 0, errors.Join(fmt.Errorf("%w: building the attempt intent", ErrRefused), err)
	}
	if expected != prep.IntentDigest {
		return prep, nil, 0, fmt.Errorf("%w: authorization bound intent digest %q, but the intent it describes is %q",
			ErrRefused, prep.IntentDigest, expected)
	}
	published, err := d.PublishIntent(intent)
	if err != nil {
		return prep, nil, 0, errors.Join(fmt.Errorf("%w: publishing the attempt intent", ErrOrphanedIntent), err)
	}
	// What was WRITTEN must be what was AUTHORIZED. An intent describing a different command is durable
	// at this point, so the attempt cannot proceed on top of it.
	if published != expected {
		return prep, nil, 0, fmt.Errorf("%w: the published intent digest %q is not the authorized %q",
			ErrOrphanedIntent, published, expected)
	}

	// 3. Armed DORMANT. A crash here leaves an armed containment with no command in it, which is cheap
	// to prove empty — as opposed to a bound attempt whose containment never existed.
	cont, err := d.ArmContainment(clonePrepared(prep))
	if err != nil {
		// cont may be non-nil when arming partially succeeded; the caller closes whatever exists.
		return prep, cont, 0, errors.Join(fmt.Errorf("%w: arming the containment", ErrOrphanedIntent), err)
	}

	// 4. Only now does state carry an active reference — and by construction, every state that carries
	// one is a state in which the containment already existed.
	bind, err := d.BindActive(clonePrepared(prep))
	if err != nil || bind.Status == BindUncertain {
		// Uncertainty is settled WHILE THE GUARD IS STILL HELD, because this is the only moment it can
		// be settled cheaply. Afterwards the question belongs to recovery.
		confirmed, cerr := d.ConfirmBind(clonePrepared(prep))
		if cerr != nil {
			return prep, cont, BindUncertain, errors.Join(
				fmt.Errorf("%w: the active binding could not be confirmed", ErrRecoveryOwned), err, cerr)
		}
		bind = confirmed
	}
	st := bind.Status
	switch st {
	case BindCommitted:
		// A DEPENDENCY-CONTRACT VIOLATION, not a state recovery is designed around.
		//
		// State refuses a new active attempt whose start revision is not the revision that created it,
		// and it refuses it before serialization — so a record committed at another revision cannot
		// exist durably. A collaborator that reports one is therefore reporting something impossible,
		// and this process cannot tell which half of the claim is false: the commit may have landed at
		// the reserved slot, elsewhere, or not at all. It is handed over for exactly that reason, and
		// NOT because durable state is allowed to disagree with the published intent.
		//
		// The ordinary intervention — the reserved slot taken while the guard was held — does not arrive
		// here at all. It arrives as BindNotCommitted, below.
		if bind.Revision != prep.StartRevision {
			return prep, cont, BindUncertain, fmt.Errorf(
				"%w: the binding reported committing attempt %q at revision %d, but it was reserved at %d and state cannot hold that record",
				ErrRecoveryOwned, prep.AttemptID, bind.Revision, prep.StartRevision)
		}
		return prep, cont, st, nil
	case BindNotCommitted:
		// No attempt exists. This is also where an intervening append lands: the reserved slot was taken
		// while the guard was held, so the constrained CAS could not commit. The containment is this
		// process's to destroy, and the caller may retry.
		return prep, cont, st, errors.Join(fmt.Errorf("%w: the active attempt was not bound", ErrOrphanedIntent), err)
	case BindUncertain:
		// It may exist. Tearing down here would destroy the proof recovery needs.
		return prep, cont, st, fmt.Errorf("%w: the active binding is visible but unconfirmed", ErrRecoveryOwned)
	default:
		// An unknown status does NOT establish that no active reference exists, so it takes the
		// recovery-owned path. Treating it as an ordinary lifecycle failure would have torn the
		// containment down on the strength of a status nobody understood.
		return prep, cont, BindUncertain, fmt.Errorf("%w: the active binding reported the unknown status %v", ErrRecoveryOwned, st)
	}
}

// finalizeUnderGuard is steps 9 to 11: re-authorize, observe, publish, CAS.
func (d Deps) finalizeUnderGuard(prep PreparedAttempt, term Terminal) (Result, error) {
	// 9a. The attempt being finalized must still be THE active one. Between releasing and reacquiring
	// the guard, another party may have cancelled it or a recovery may have finalized it.
	if err := d.Reauthorize(clonePrepared(prep)); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: re-authorizing the attempt", ErrLifecycle), err)
	}

	// 9b. The final identity observation, UNDER the guard, is the linearization point the result's
	// unchanged-versus-changed claim refers to. It receives what the runner saw, so the two independent
	// axes come from the two parties that actually observed them.
	ident, err := d.ObserveIdentity(clonePrepared(prep), cloneTerminal(term))
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: observing the final identity", ErrLifecycle), err)
	}
	// Checked BEFORE the verdict is derived and long before anything is published, so a
	// self-contradictory identity cannot authorize a pass.
	if err := acceptIdentity(prep, ident); err != nil {
		return Result{}, err
	}

	// The verdict is derived here, by the one total function that owns that decision, so an
	// unrepresentable pair is refused before a record claims it.
	outcome, err := state.Outcome(term.Execution, ident.Value)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: deriving the outcome", ErrLifecycle), err)
	}

	// 10. Only now can the record be built: it is immutable and states the identity distinction, so
	// before step 9b that field would have been a claim about a fact that did not exist.
	//
	// It is built and VALIDATED here, on the main path, from facts this function already holds - the
	// command and environment identity the attempt was authorised with, the runner's terminal and its
	// stream evidence, and the identity just observed. Handing the publisher the pieces instead left it
	// to assemble the canonical record with no way to reach half of what the record binds.
	rec, err := buildResultRecord(prep, term, ident)
	if err != nil {
		return Result{}, err
	}
	// What the record IS, computed here. The publisher reports a digest and the state CAS binds it, so
	// without an independent expectation the ledger can name bytes nobody produced - the same hole
	// intent publication was corrected for, one step later.
	_, expected, err := rec.Encode()
	if err != nil {
		return Result{}, err
	}
	digest, err := d.PublishResult(rec)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: publishing the result", ErrLifecycle), err)
	}
	if digest != expected {
		// A digest that does not identify the record handed over would be bound by the outcome CAS, and
		// every later reader would fetch bytes that say something else - or nothing at all.
		return Result{}, fmt.Errorf("%w: the publisher reported digest %q for a record whose canonical digest is %q",
			ErrLifecycle, digest, expected)
	}

	// 11. The outcome CAS names the digest of a record that is ALREADY durable, so state never
	// references a result a reader cannot fetch.
	st, err := d.FinalizeOutcome(clonePrepared(prep), cloneTerminal(term), ident, digest)
	if err != nil || st == BindUncertain {
		conf, cerr := d.ConfirmFinalize(clonePrepared(prep), cloneTerminal(term), ident, digest)
		if cerr != nil {
			return Result{}, errors.Join(fmt.Errorf("%w: the outcome could not be confirmed", ErrLifecycle), err, cerr)
		}
		if conf.Status == BindCommitted {
			// "Committed" is only useful if it is OUR outcome. A bound entry for this attempt carrying a
			// different result was written by somebody else, and reporting success would return a digest
			// that nothing in the ledger references.
			if conf.Entry == nil {
				return Result{}, fmt.Errorf("%w: the outcome was reported committed with no entry to show for it", ErrRecoveryOwned)
			}
			if mismatch := disagreesWithPublished(*conf.Entry, prep, term, ident, digest); mismatch != "" {
				return Result{}, fmt.Errorf("%w: the bound outcome for attempt %q is not the one published here: %s",
					ErrRecoveryOwned, prep.AttemptID, mismatch)
			}
		}
		st = conf.Status
	}
	switch st {
	case BindCommitted:
	case BindNotCommitted:
		return Result{}, errors.Join(fmt.Errorf("%w: the outcome was not applied", ErrLifecycle), err)
	default:
		// Unresolved after confirmation. The command has already finished and its domain is drained, so
		// there is nothing live to preserve here - only the state question, which recovery re-reads.
		return Result{}, fmt.Errorf("%w: the outcome is visible but unconfirmed", ErrLifecycle)
	}
	return Result{Prepared: clonePrepared(prep), Terminal: cloneTerminal(term), Identity: ident, ResultDigest: digest, Outcome: outcome}, nil
}

// acceptTerminal canonicalizes and shape-checks the runner's fact at the boundary where it enters this
// process, which is the only place that can stop it reaching disk.
//
// Canonicalization happens HERE rather than at persistence because the result digest is computed over
// these bytes: rewriting them later would leave the digest identifying text that no longer exists, which
// is the defect the state boundary had to be corrected for. Once is the right number of times.
// disagreesWithPublished names the first immutable field on which a bound ledger record differs from
// what this lifecycle published, or "" when they agree.
//
// Every field the lifecycle KNOWS is compared, not only the ones that change routing. The terminal
// reason does not steer the run anywhere, but it is the text a human reads to understand the verdict,
// and a record pairing this attempt's digest with somebody else's account of how the command ended is
// evidence that disagrees with itself.
//
// StartRevision IS compared. This lifecycle reserved it, bound it into the published intent and checked
// the binding CAS against it, so it is a fact this process knows exactly. BoundRevision is the only
// excluded field: the store assigns it at finalization, this process never learns it, and a comparison
// against a value it invented could not fail. Saying "the revisions" would license removing the
// StartRevision comparison on the strength of a sentence that was meant to describe only the other one.
func disagreesWithPublished(e state.FinalizedAttempt, prep PreparedAttempt, term Terminal, ident Identity, digest string) string {
	for _, f := range []struct{ name, got, want string }{
		{"attempt id", e.AttemptID, prep.AttemptID},
		{"start revision", fmt.Sprintf("%d", e.StartRevision), fmt.Sprintf("%d", prep.StartRevision)},
		{"tested commit", e.TestedCommit, prep.TestedCommit},
		{"tested tree", e.TestedTree, prep.TestedTree},
		{"result digest", e.ResultDigest, digest},
		{"execution", string(e.Execution), string(term.Execution)},
		{"identity", string(e.Identity), string(ident.Value)},
		{"terminal reason", e.TerminalReason, term.TerminalReason},
		{"terminal authority", string(e.TerminalAuthor), string(term.Author)},
	} {
		if f.got != f.want {
			return fmt.Sprintf("the bound %s is %q, published %q", f.name, f.got, f.want)
		}
	}
	return ""
}

func acceptTerminal(t Terminal) (Terminal, error) {
	// Canonicalization happens HERE, and BEFORE the shared check, because this is the boundary where the
	// runner's raw account enters the process: the shared table refuses non-canonical text, which is
	// right for a record already on its way to disk but would reject every genuine runner report if it
	// ran first. Doing it here rather than at persistence is what keeps the result digest identifying
	// the bytes that are actually stored.
	t.TerminalReason = state.CanonicalTerminalReason(t.TerminalReason)
	// ONE truth table, shared with the record boundary - including the length bound, which applies AFTER
	// canonicalization because canonicalization changes the length. Two validators for one fact is one
	// validator and one liability: the record boundary was admitting contradictions this function had
	// already learned to refuse.
	if err := checkTerminalFacts(t.Execution, "", t.TerminalReason, t.Author, t.HasExitCode, t.ExitCode); err != nil {
		return Terminal{}, err
	}
	for _, st := range []struct {
		what string
		s    StreamRecord
	}{{"stdout", t.Stdout}, {"stderr", t.Stderr}} {
		if err := st.s.validate(st.what); err != nil {
			return Terminal{}, err
		}
	}
	t.Stdout, t.Stderr = cloneStream(t.Stdout), cloneStream(t.Stderr)
	return t, nil
}

// buildResultRecord assembles the ONE canonical record from what the guarded section already knows.
//
// Nothing here is re-derived. The command and the environment identity come from the prepared attempt,
// which is what authorization froze; recomputing them now would describe this process rather than the
// attempt.
// cloneTerminal hands out an owned copy, including the stream excerpts.
//
// A Terminal is nearly all scalars, which is why an earlier version passed it around by value and
// assumed that was enough. The retained excerpts are SLICES: a collaborator receiving a value copy still
// shares their backing arrays, so it could rewrite the evidence between acceptance and the record that
// binds it - and the record's own digest check would then fail on bytes nobody meant to change.
// checkRetainedOutput applies the combined ceiling and the deterministic allocation together.
//
// They belong together because neither is meaningful alone: a ceiling with no allocation rule does not
// say which bytes to keep, and an allocation rule with no ceiling does not say how many.
// checkRecordFits proves the CANONICAL record is storable, which the raw excerpt bound does not.
func checkRecordFits(rec ResultRecord, maxRecordBytes uint64) error {
	raw, _, err := rec.Encode()
	if err != nil {
		return err
	}
	if uint64(len(raw)) > maxRecordBytes {
		return fmt.Errorf("%w: the canonical result is %d bytes, over the frozen ceiling of %d",
			ErrLifecycle, len(raw), maxRecordBytes)
	}
	return nil
}

func checkRetainedOutput(stdout, stderr StreamRecord, combined uint64) error {
	if !FitsCombinedOutputCeiling(stdout, stderr, combined) {
		return fmt.Errorf("%w: the retained output is %d bytes, over the frozen ceiling of %d",
			ErrLifecycle, stdout.RetainedBytes()+stderr.RetainedBytes(), combined)
	}
	outBudget, errBudget := AllocateOutputBudget(combined)
	if err := stdout.CheckSplit("stdout", outBudget); err != nil {
		return err
	}
	return stderr.CheckSplit("stderr", errBudget)
}

func cloneTerminal(t Terminal) Terminal {
	t.Stdout, t.Stderr = cloneStream(t.Stdout), cloneStream(t.Stderr)
	return t
}

// buildIntentRecord assembles the intent the attempt is about to publish, and returns it with the digest
// of its canonical bytes.
//
// It exists on the main path because the digest has to be COMPUTED somewhere that holds the facts. An
// intent type that only recovery ever constructed meant the live path published something whose shape
// nothing checked, and the reference bound a digest whose provenance was a promise.
func buildIntentRecord(prep PreparedAttempt) (IntentRecord, string, error) {
	view, err := prep.Spec.View()
	if err != nil {
		return IntentRecord{}, "", err
	}
	rec := IntentRecord{
		SchemaVersion:  IntentRecordVersion,
		AttemptID:      prep.AttemptID,
		StartRevision:  prep.StartRevision,
		TestedCommit:   prep.TestedCommit,
		TestedTree:     prep.TestedTree,
		View:           view,
		SpecDigest:     prep.Spec.Digest,
		MaxOutputBytes: prep.MaxOutputBytes,
		MaxRecordBytes: prep.MaxRecordBytes,
	}
	_, digest, err := rec.Encode()
	if err != nil {
		return IntentRecord{}, "", err
	}
	return rec, digest, nil
}

func buildResultRecord(prep PreparedAttempt, term Terminal, ident Identity) (ResultRecord, error) {
	view, err := prep.Spec.View()
	if err != nil {
		return ResultRecord{}, err
	}
	rec := ResultRecord{
		SchemaVersion:  ResultRecordVersion,
		AttemptID:      prep.AttemptID,
		TestedCommit:   prep.TestedCommit,
		TestedTree:     prep.TestedTree,
		View:           view,
		SpecDigest:     prep.Spec.Digest,
		Execution:      term.Execution,
		Identity:       ident.Value,
		TerminalReason: term.TerminalReason,
		TerminalAuthor: term.Author,
		HasExitCode:    term.HasExitCode,
		ExitCode:       term.ExitCode,
		Stdout:         cloneStream(term.Stdout),
		Stderr:         cloneStream(term.Stderr),
	}
	// The COMBINED ceiling and the split RULE, applied where the frozen bound is known. Per-stream
	// checking admits a record twice the size the operator allowed; accepting any division of the right
	// total makes "head+tail" a shape rather than a rule, so two producers could keep different bytes
	// and each call itself correct.
	if err := checkRetainedOutput(rec.Stdout, rec.Stderr, prep.MaxOutputBytes); err != nil {
		return ResultRecord{}, err
	}
	if err := checkRecordFits(rec, prep.MaxRecordBytes); err != nil {
		return ResultRecord{}, err
	}
	// Refused HERE if it contradicts itself, rather than handed on for the publisher to discover it
	// cannot be written - at which point the attempt is already past the point of being retried cheaply.
	if err := rec.validate(); err != nil {
		return ResultRecord{}, err
	}
	return rec, nil
}

func exitString(t Terminal) string {
	if !t.HasExitCode {
		return "none"
	}
	return fmt.Sprintf("%d", t.ExitCode)
}

// acceptIdentity proves the identity value AGREES with its own evidence.
//
// state.Outcome validates the enum, not what the enum claims. Without this, `unchanged` could carry a
// commit and tree that differ from the ones the attempt was authorized against — and an `ok` execution
// would then become a PASS certifying a tree nobody compared. `unobserved` could carry a claimed
// identity it did not observe, and `changed` could carry the unchanged one.
func acceptIdentity(prep PreparedAttempt, id Identity) error {
	if !state.KnownTestIdentity(id.Value) {
		return fmt.Errorf("%w: the observer reported the unknown identity %q", ErrLifecycle, id.Value)
	}
	switch id.Value {
	case state.TestIdentityUnchanged:
		if id.Commit != prep.TestedCommit || id.Tree != prep.TestedTree {
			return fmt.Errorf("%w: identity says unchanged but names a different commit/tree than the attempt was authorized against", ErrLifecycle)
		}
	case state.TestIdentityChanged:
		if id.Commit == "" || id.Tree == "" {
			return fmt.Errorf("%w: identity says changed but observed no commit/tree to have changed to", ErrLifecycle)
		}
		// A VALID observed identity, not merely a non-empty one. Arbitrary strings were durably
		// publishable and routed to `fail`, spending the code-fix budget on an observation that cannot
		// name a git object at all. The grammar is imported from the one authority rather than copied,
		// because a copied grammar drifts.
		if !state.IsGitOID(id.Commit) || !state.IsGitOID(id.Tree) {
			return fmt.Errorf("%w: identity says changed but %q/%q are not git object ids", ErrLifecycle, id.Commit, id.Tree)
		}
		if id.Commit == prep.TestedCommit && id.Tree == prep.TestedTree {
			return fmt.Errorf("%w: identity says changed but names exactly the authorized commit/tree", ErrLifecycle)
		}
	case state.TestIdentityUnobserved:
		if id.Commit != "" || id.Tree != "" {
			return fmt.Errorf("%w: identity says unobserved but carries a commit/tree it cannot have observed", ErrLifecycle)
		}
	}
	return nil
}

// clonePrepared returns an independently owned copy, nested bytes included.
//
// The same value is handed to intent publication, arming, binding, result publication and the handoff.
// A shallow copy shares the argv backing array and every environment name and value, so a collaborator
// that sorted or canonicalized in place would silently change what the NEXT step receives — after the
// digest identifying those bytes had already been chosen. The "one execution travels" proof would then
// hold only for collaborators that promised not to touch it, which is a convention, and conventions are
// exactly what this package exists to replace.
func clonePrepared(p PreparedAttempt) PreparedAttempt {
	c := p
	c.Spec.Argv = append([]string(nil), p.Spec.Argv...)
	if p.Spec.Env.Env != nil {
		env := make([]config.ResolvedVar, len(p.Spec.Env.Env))
		for i, e := range p.Spec.Env.Env {
			env[i] = config.ResolvedVar{
				Name:  append([]byte(nil), e.Name...),
				Value: append([]byte(nil), e.Value...),
			}
		}
		c.Spec.Env.Env = env
	}
	return c
}

// closeContainment releases the domain, tolerating the case where none was ever armed.
func closeContainment(c Containment) error {
	if c == nil {
		return nil
	}
	if err := c.Close(); err != nil {
		return errors.Join(fmt.Errorf("%w: closing the containment", ErrLifecycle), err)
	}
	return nil
}

func (d Deps) validate() error {
	var missing []string
	for _, f := range []struct {
		name  string
		blank bool
	}{
		{"AcquireGuard", d.AcquireGuard == nil},
		{"ReleaseGuard", d.ReleaseGuard == nil},
		{"ReserveStartRevision", d.ReserveStartRevision == nil},
		{"Authorize", d.Authorize == nil},
		{"PublishIntent", d.PublishIntent == nil},
		{"ArmContainment", d.ArmContainment == nil},
		{"BindActive", d.BindActive == nil},
		{"ConfirmBind", d.ConfirmBind == nil},
		{"Reauthorize", d.Reauthorize == nil},
		{"ObserveIdentity", d.ObserveIdentity == nil},
		{"PublishResult", d.PublishResult == nil},
		{"FinalizeOutcome", d.FinalizeOutcome == nil},
		{"ConfirmFinalize", d.ConfirmFinalize == nil},
	} {
		if f.blank {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: incomplete wiring, missing %v", ErrRefused, missing)
	}
	return nil
}
