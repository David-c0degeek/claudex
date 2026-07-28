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

// ResolvedCommand is what authorization actually resolved, carried so the steps that need it receive it
// rather than re-deriving it from ambient state.
type ResolvedCommand struct {
	// Executable is the absolute path selected against the FROZEN PATH, never an ambient lookup.
	Executable string
	// Argv is the full vector including argv[0].
	Argv []string
	// EnvDigest binds the exact frozen environment this command will run with.
	EnvDigest string
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
	// Command is what will run. Carried because arming and intent publication both need the exact
	// command authorization chose, and neither may choose its own.
	Command ResolvedCommand
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

// BindStatus is the AUTHORITATIVE outcome of the active-attempt CAS.
//
// Three outcomes, deliberately not collapsed into `error`. A clean conflict means no attempt exists and
// the containment is this process's to destroy; a confirmed commit means an attempt exists; and a
// visible-but-unconfirmed append means it MAY exist, which is the one case where tearing down would
// destroy the evidence recovery needs.
type BindStatus int

const (
	// BindCommitted: the append is durable. An attempt exists.
	BindCommitted BindStatus = iota + 1
	// BindNotCommitted: nothing was written — a clean revision conflict, for instance. No attempt exists.
	BindNotCommitted
	// BindUncertain: the append may be visible but its durability is unconfirmed.
	BindUncertain
)

func (b BindStatus) String() string {
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
	BindActive func(PreparedAttempt) (BindStatus, error)

	// ConfirmBind settles an uncertain append while the guard is STILL HELD. It is a separate seam
	// because that is the only moment the uncertainty can be resolved cheaply — afterwards the answer
	// belongs to recovery.
	ConfirmBind func(PreparedAttempt) (BindStatus, error)

	// Reauthorize proves the exact attempt is still the active one after the guard is reacquired.
	Reauthorize func(PreparedAttempt) error

	// ObserveIdentity makes the final exact identity observation, under the guard, given what the
	// runner saw. It may legitimately return `unobserved`; it returns an error only when the
	// observation itself could not be attempted.
	ObserveIdentity func(PreparedAttempt, Terminal) (Identity, error)

	// PublishResult durably writes the ONE canonical result and returns its digest.
	PublishResult func(PreparedAttempt, Terminal, Identity) (digest string, err error)

	// FinalizeOutcome CASes the outcome into state, moving the active ref into the ledger.
	FinalizeOutcome func(PreparedAttempt, Terminal, Identity, string) error
}

// Result is what one completed attempt produced.
type Result struct {
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

	prep, cont, bound, err := d.armUnderGuard()
	if err != nil {
		// Teardown ownership depends on WHAT SURVIVED, which is why the bind status is carried out here
		// rather than folded into the error. If an attempt may exist, closing the containment would
		// destroy the only proof recovery can use — so this process releases the guard and stops.
		var cerr error
		if !errors.Is(err, ErrRecoveryOwned) {
			cerr = closeContainment(cont)
		}
		if rerr := d.ReleaseGuard(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("%w: releasing the run guard after a refusal", ErrLifecycle), rerr)
		}
		return Result{}, errors.Join(err, cerr)
	}
	_ = bound

	if err := d.ReleaseGuard(); err != nil {
		// The containment is armed and the state is bound, so an attempt now exists and must be torn
		// down rather than abandoned.
		return Result{}, errors.Join(fmt.Errorf("%w: releasing the run guard before GO", ErrLifecycle), err, closeContainment(cont))
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
func (d Deps) armUnderGuard() (PreparedAttempt, Containment, BindStatus, error) {
	// 1. Nothing is written until every check has passed, so this failure leaves no residue at all.
	prep, err := d.Authorize()
	if err != nil {
		return PreparedAttempt{}, nil, 0, errors.Join(fmt.Errorf("%w: authorizing the attempt", ErrRefused), err)
	}

	// 2. The intent is durable before anything can act on it. From HERE the strict no-residue promise no
	// longer holds: an orphan intent may exist, which is why later failures carry a weaker class.
	if err := d.PublishIntent(prep); err != nil {
		return prep, nil, 0, errors.Join(fmt.Errorf("%w: publishing the attempt intent", ErrOrphanedIntent), err)
	}

	// 3. Armed DORMANT. A crash here leaves an armed containment with no command in it, which is cheap
	// to prove empty — as opposed to a bound attempt whose containment never existed.
	cont, err := d.ArmContainment(prep)
	if err != nil {
		// cont may be non-nil when arming partially succeeded; the caller closes whatever exists.
		return prep, cont, 0, errors.Join(fmt.Errorf("%w: arming the containment", ErrOrphanedIntent), err)
	}

	// 4. Only now does state carry an active reference — and by construction, every state that carries
	// one is a state in which the containment already existed.
	st, err := d.BindActive(prep)
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
		return prep, cont, st, fmt.Errorf("%w: the active binding reported the unknown status %v", ErrLifecycle, st)
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
	ident, err := d.ObserveIdentity(prep, term)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: observing the final identity", ErrLifecycle), err)
	}

	// The verdict is derived here, by the one total function that owns that decision, so an
	// unrepresentable pair is refused before a record claims it.
	outcome, err := state.Outcome(term.Execution, ident.Value)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: deriving the outcome", ErrLifecycle), err)
	}

	// 10. Only now can the record be built: it is immutable and states the identity distinction, so
	// before step 9b that field would have been a claim about a fact that did not exist.
	digest, err := d.PublishResult(prep, term, ident)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: publishing the result", ErrLifecycle), err)
	}

	// 11. The outcome CAS names the digest of a record that is ALREADY durable, so state never
	// references a result a reader cannot fetch.
	if err := d.FinalizeOutcome(prep, term, ident, digest); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: finalizing the outcome", ErrLifecycle), err)
	}
	return Result{Prepared: prep, Terminal: term, Identity: ident, ResultDigest: digest, Outcome: outcome}, nil
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
