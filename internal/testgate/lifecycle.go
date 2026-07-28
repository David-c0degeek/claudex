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
	"strings"

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
	// Digest is the identity of everything above, bound in the intent and the result.
	Digest string
}

// PreparedAttempt is everything the guarded pre-flight established, travelling as ONE value.
//
// It is the answer to "what was authorized": the identity the attempt is a statement about, the command
// that will run, and the digests that bind them. Every later step takes it, so no step has to reconstruct
// a fact an earlier one already proved.
type PreparedAttempt struct {
	// AttemptID is the minted identity every later fact binds.
	AttemptID string
	// TestedCommit and TestedTree are the repository identity this attempt is a statement ABOUT.
	TestedCommit string
	TestedTree   string
	// IntentDigest binds the exact published intent.
	IntentDigest string
	// Spec is what will run, complete. Carried because arming and intent publication both need the exact
	// command AND environment authorization chose, and neither may construct its own.
	Spec ExecutionSpec
}

// Terminal is what the RUNNER observed about the command, in the closed vocabulary.
//
// It comes back from Wait rather than being reconstructed later, because the runner is the only party
// that saw the command end. An earlier draft returned only an error here, so these facts disappeared and
// the identity observer had to invent them.
type Terminal struct {
	Execution      state.TestExecution
	TerminalReason string // already canonical; see state.CanonicalTerminalReason
	// ExitCode is present for a normal exit and absent otherwise — a timeout has no exit code, and
	// zero would be indistinguishable from success.
	ExitCode *int
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

	// Authorize proves ownerless TESTS with no active attempt, freezes the identity, resolves the
	// command and re-checks the record fit — returning all of it as one value. It mints nothing until
	// every check has passed, so its failure writes nothing.
	Authorize func() (PreparedAttempt, error)

	// PublishIntent durably writes the immutable intent for exactly this prepared attempt.
	//
	// Durability uncertainty here is BENIGN and needs no status: either the intent is on disk and
	// orphaned, or it is not, and the design treats an orphan intent as ignorable with a fresh attempt
	// superseding it. Nothing acts on an intent that no active reference points at.
	PublishIntent func(PreparedAttempt) error

	// ArmContainment arms the containment with NO command in it. It returns the Containment even on
	// failure when anything was partially armed, so the caller can always release what exists.
	ArmContainment func(PreparedAttempt) (Containment, error)

	// BindActive CASes the active attempt into state and reports the AUTHORITATIVE status.
	BindActive func(PreparedAttempt) (CommitStatus, error)

	// ConfirmBind settles an uncertain append while the guard is STILL HELD. It is a separate seam
	// because that is the only moment the uncertainty can be resolved cheaply — afterwards the answer
	// belongs to recovery.
	ConfirmBind func(PreparedAttempt) (CommitStatus, error)

	// Reauthorize proves the exact attempt is still the active one after the guard is reacquired.
	Reauthorize func(PreparedAttempt) error

	// ObserveIdentity makes the final exact identity observation, under the guard, given what the
	// runner saw. It may legitimately return `unobserved`; it returns an error only when the
	// observation itself could not be attempted.
	ObserveIdentity func(PreparedAttempt, Terminal) (Identity, error)

	// PublishResult durably writes the ONE canonical result and returns its digest.
	PublishResult func(PreparedAttempt, Terminal, Identity) (digest string, err error)

	// FinalizeOutcome CASes the outcome into state, moving the active ref into the ledger, and reports
	// the AUTHORITATIVE status — the same three-way answer the active binding gives, for the same
	// reason: "it failed" does not say whether the outcome was applied.
	FinalizeOutcome func(PreparedAttempt, Terminal, Identity, string) (CommitStatus, error)

	// ConfirmFinalize settles an uncertain outcome append while the second guard is STILL HELD. The
	// design's row for this is "re-confirm; never double-apply", so confirmation READS rather than
	// retries: re-applying an outcome that did commit would bind a second verdict to one attempt.
	ConfirmFinalize func(PreparedAttempt) (CommitStatus, error)
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
	// 1. Nothing is written until every check has passed, so this failure leaves no residue at all.
	prep, err := d.Authorize()
	if err != nil {
		return PreparedAttempt{}, nil, 0, errors.Join(fmt.Errorf("%w: authorizing the attempt", ErrRefused), err)
	}

	// 2. The intent is durable before anything can act on it. From HERE the strict no-residue promise no
	// longer holds: an orphan intent may exist, which is why later failures carry a weaker class.
	if err := d.PublishIntent(clonePrepared(prep)); err != nil {
		return prep, nil, 0, errors.Join(fmt.Errorf("%w: publishing the attempt intent", ErrOrphanedIntent), err)
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
	st, err := d.BindActive(clonePrepared(prep))
	if err != nil || st == BindUncertain {
		// Uncertainty is settled WHILE THE GUARD IS STILL HELD, because this is the only moment it can
		// be settled cheaply. Afterwards the question belongs to recovery.
		confirmed, cerr := d.ConfirmBind(prep)
		if cerr != nil {
			return prep, cont, BindUncertain, errors.Join(
				fmt.Errorf("%w: the active binding could not be confirmed", ErrRecoveryOwned), err, cerr)
		}
		st = confirmed
	}
	switch st {
	case BindCommitted:
		return prep, cont, st, nil
	case BindNotCommitted:
		// No attempt exists. The containment is this process's to destroy, and the caller may retry.
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
	if err := d.Reauthorize(prep); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: re-authorizing the attempt", ErrLifecycle), err)
	}

	// 9b. The final identity observation, UNDER the guard, is the linearization point the result's
	// unchanged-versus-changed claim refers to. It receives what the runner saw, so the two independent
	// axes come from the two parties that actually observed them.
	ident, err := d.ObserveIdentity(clonePrepared(prep), term)
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
	digest, err := d.PublishResult(clonePrepared(prep), term, ident)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: publishing the result", ErrLifecycle), err)
	}

	// 11. The outcome CAS names the digest of a record that is ALREADY durable, so state never
	// references a result a reader cannot fetch.
	st, err := d.FinalizeOutcome(clonePrepared(prep), term, ident, digest)
	if err != nil || st == BindUncertain {
		// Settled while the guard is STILL held, and by READING rather than retrying: the design's row
		// is "re-confirm; never double-apply", because re-applying an outcome that did commit would bind
		// a second verdict to one attempt.
		confirmed, cerr := d.ConfirmFinalize(prep)
		if cerr != nil {
			return Result{}, errors.Join(fmt.Errorf("%w: the outcome could not be confirmed", ErrLifecycle), err, cerr)
		}
		st = confirmed
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
	return Result{Prepared: prep, Terminal: term, Identity: ident, ResultDigest: digest, Outcome: outcome}, nil
}

// acceptTerminal canonicalizes and shape-checks the runner's fact at the boundary where it enters this
// process, which is the only place that can stop it reaching disk.
//
// Canonicalization happens HERE rather than at persistence because the result digest is computed over
// these bytes: rewriting them later would leave the digest identifying text that no longer exists, which
// is the defect the state boundary had to be corrected for. Once is the right number of times.
func acceptTerminal(t Terminal) (Terminal, error) {
	if !state.KnownTestExecution(t.Execution) {
		return Terminal{}, fmt.Errorf("%w: the runner reported the unknown execution %q", ErrLifecycle, t.Execution)
	}
	// The exit fact must AGREE with the verdict, not merely be present.
	//
	// Presence alone accepted `ok` with exit 17 — which state.Outcome turns into a PASS — and `nonzero`
	// with exit 0. Both are self-contradictory records, and the first advances the run on the strength
	// of a command that failed.
	switch t.Execution {
	case state.TestExecutionOK:
		if t.ExitCode == nil || *t.ExitCode != 0 {
			return Terminal{}, fmt.Errorf("%w: %q requires exit code 0, got %s", ErrLifecycle, t.Execution, exitString(t.ExitCode))
		}
	case state.TestExecutionNonzero:
		if t.ExitCode == nil || *t.ExitCode == 0 {
			return Terminal{}, fmt.Errorf("%w: %q requires a non-zero exit code, got %s", ErrLifecycle, t.Execution, exitString(t.ExitCode))
		}
		if *t.ExitCode < 0 {
			return Terminal{}, fmt.Errorf("%w: %q reported the impossible exit code %d", ErrLifecycle, t.Execution, *t.ExitCode)
		}
	default:
		// A timeout, a cancel, a spawn failure and an interruption all end without the command
		// reporting anything, so a code here would be invented.
		if t.ExitCode != nil {
			return Terminal{}, fmt.Errorf("%w: %q cannot carry an exit code", ErrLifecycle, t.Execution)
		}
	}
	if strings.TrimSpace(t.TerminalReason) == "" {
		return Terminal{}, fmt.Errorf("%w: the runner reported no terminal detail", ErrLifecycle)
	}
	t.TerminalReason = state.CanonicalTerminalReason(t.TerminalReason)
	// Bounded AFTER canonicalization, because canonicalization changes the length and the ledger's
	// bound applies to the stored bytes. Checking it here rather than at the state CAS is what stops an
	// oversized record becoming durable first and being refused afterwards, when it can no longer be
	// un-written.
	if len(t.TerminalReason) > state.MaxTerminalReasonBytes {
		return Terminal{}, fmt.Errorf("%w: the terminal detail is %d bytes after canonicalization, limit %d",
			ErrLifecycle, len(t.TerminalReason), state.MaxTerminalReasonBytes)
	}
	return t, nil
}

func exitString(c *int) string {
	if c == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *c)
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
