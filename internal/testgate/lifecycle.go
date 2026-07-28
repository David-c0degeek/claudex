// Package testgate owns the ORDER in which one mechanical test attempt happens.
//
// Every step it sequences already exists somewhere else — the run guard, the attempt store, the
// containment, the identity observation, the state CAS. What does not exist anywhere else is the
// guarantee that they happen in the one order that makes each step's facts true when the next step
// reads them. That guarantee cannot live in a call-site convention, because a convention is followed by
// whoever remembers it; so it lives here, as one object's behaviour, the way the terminal sequence does
// in internal/proctree.
//
// The order, and why each step is where it is:
//
//  1. UNDER THE RUN GUARD, authorize: exactly ownerless TESTS with NO active attempt, the frozen
//     identity proven, the command resolved and the record's fit re-checked. All of it before anything
//     is minted, so a resolution or fit failure is a pre-attempt refusal that binds nothing.
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
)

// ErrLifecycle marks a failure of the ordered lifecycle itself, as distinct from a failure of the
// command under test. The two must never be confused: one is an infrastructure fault, the other is the
// answer the gate exists to produce.
var ErrLifecycle = errors.New("testgate: attempt lifecycle failed")

// ErrPreAttempt marks a refusal that happened BEFORE anything was minted or bound.
//
// It is a distinct class because its promise is different: nothing was published, nothing was armed,
// no state changed. A caller may retry it freely, which is not true of any later failure.
var ErrPreAttempt = errors.New("testgate: refused before the attempt existed")

// Authorization is what the guarded pre-flight proved, frozen for the rest of the attempt.
type Authorization struct {
	// AttemptID is the minted identity every later fact binds.
	AttemptID string
	// TestedCommit and TestedTree are the repository identity this attempt is a statement ABOUT.
	TestedCommit string
	TestedTree   string
	// IntentDigest binds the exact published intent.
	IntentDigest string
}

// Observation is the final identity observation plus what the runner saw.
type Observation struct {
	// Commit and Tree are the identity at the END of the attempt.
	Commit string
	Tree   string
	// Execution is the runner's terminal fact for the command.
	Execution string
	// TerminalReason is the runner's descriptive detail, ALREADY canonical.
	TerminalReason string
}

// Deps are the collaborators, each one an authority that exists elsewhere.
//
// They are injected rather than reached for because this package's whole contribution is the ORDER, and
// an order can only be proven by observing when each collaborator is called relative to the others.
type Deps struct {
	// AcquireGuard and ReleaseGuard bracket the two locked sections. The subprocess runs between them.
	AcquireGuard func() error
	ReleaseGuard func() error

	// Authorize proves ownerless TESTS with no active attempt, freezes the identity, resolves the
	// command and re-checks the record fit. It runs under the guard and mints nothing until it has
	// succeeded, so its failure binds nothing.
	Authorize func() (Authorization, error)

	// PublishIntent durably writes the immutable intent.
	PublishIntent func(Authorization) error

	// ArmContainment arms the containment with NO command in it, and returns a Containment whose Go
	// starts one.
	ArmContainment func(Authorization) (Containment, error)

	// BindActive CASes the active attempt reference into state.
	BindActive func(Authorization) error

	// Reauthorize proves the exact attempt is still the active one after the guard is reacquired.
	Reauthorize func(Authorization) error

	// ObserveIdentity makes the final exact identity observation, under the guard.
	ObserveIdentity func(Authorization) (Observation, error)

	// PublishResult durably writes the ONE canonical result and returns its digest.
	PublishResult func(Authorization, Observation) (digest string, err error)

	// FinalizeOutcome CASes the outcome into state, moving the active ref into the ledger.
	FinalizeOutcome func(Authorization, Observation, string) error
}

// Containment is the armed domain the command runs inside.
type Containment interface {
	// Go starts the command. Before this call no command exists.
	Go() error
	// Wait blocks until the command is finished and its domain is torn down.
	Wait() error
	// Close releases the containment on every path, including those where Go was never called.
	Close() error
}

// Result is what one completed attempt produced.
type Result struct {
	Authorization Authorization
	Observation   Observation
	ResultDigest  string
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
		return Result{}, errors.Join(fmt.Errorf("%w: acquiring the run guard", ErrPreAttempt), err)
	}

	auth, cont, err := d.armUnderGuard()
	if err != nil {
		// BOTH resources are released on every path out of the first section.
		//
		// The guard, because a refusal that left the run locked would be worse than the refusal. And the
		// CONTAINMENT, because arming happens before the CAS — so a failed bind returns with a domain
		// already armed, and returning without closing it would abandon exactly the thing the arming
		// order exists to make recoverable. That is the same asymmetry the terminal sequence in
		// internal/proctree had to be corrected for three times, arriving here one layer up; this test
		// caught it rather than a review round.
		cerr := closeContainment(cont)
		if rerr := d.ReleaseGuard(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("%w: releasing the run guard after a refusal", ErrLifecycle), rerr)
		}
		return Result{}, errors.Join(err, cerr)
	}

	if err := d.ReleaseGuard(); err != nil {
		// The containment is armed and the state is bound, so this is NOT a pre-attempt refusal: an
		// attempt now exists and must be torn down rather than abandoned.
		return Result{}, errors.Join(fmt.Errorf("%w: releasing the run guard before GO", ErrLifecycle), err, closeContainment(cont))
	}

	// ---- Outside the lock ------------------------------------------------------------------------
	//
	// GO is sent AFTER the guard is released, so the command's entire lifetime is outside the lock.
	if err := cont.Go(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: starting the command", ErrLifecycle), err, closeContainment(cont))
	}
	if err := cont.Wait(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: awaiting the command", ErrLifecycle), err, closeContainment(cont))
	}

	// ---- Second guarded section ------------------------------------------------------------------
	if err := d.AcquireGuard(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: reacquiring the run guard", ErrLifecycle), err, closeContainment(cont))
	}
	res, ferr := d.finalizeUnderGuard(auth)
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
// The containment is armed BEFORE the CAS and returned even on later failure, so the caller can tear it
// down. Returning it only on success would mean a failed CAS left an armed domain with no handle to it.
func (d Deps) armUnderGuard() (Authorization, Containment, error) {
	// 1. Nothing is minted until every check has passed, so this failure binds nothing.
	auth, err := d.Authorize()
	if err != nil {
		return Authorization{}, nil, errors.Join(fmt.Errorf("%w: authorizing the attempt", ErrPreAttempt), err)
	}

	// 2. The intent is durable before anything can act on it.
	if err := d.PublishIntent(auth); err != nil {
		return auth, nil, errors.Join(fmt.Errorf("%w: publishing the attempt intent", ErrPreAttempt), err)
	}

	// 3. Armed DORMANT. From here a crash leaves an armed containment with no command in it, which is
	// cheap to prove empty — as opposed to a bound attempt whose containment never existed.
	cont, err := d.ArmContainment(auth)
	if err != nil {
		return auth, nil, errors.Join(fmt.Errorf("%w: arming the containment", ErrPreAttempt), err)
	}

	// 4. Only now does state carry an active reference — and by construction, every state that carries
	// one is a state in which the containment already existed.
	if err := d.BindActive(auth); err != nil {
		return auth, cont, errors.Join(fmt.Errorf("%w: binding the active attempt", ErrLifecycle), err)
	}
	return auth, cont, nil
}

// finalizeUnderGuard is steps 9 to 11: re-authorize, observe, publish, CAS.
func (d Deps) finalizeUnderGuard(auth Authorization) (Result, error) {
	// 9a. The attempt being finalized must still be THE active one. Between releasing and reacquiring
	// the guard, another party may have cancelled it or a recovery may have finalized it.
	if err := d.Reauthorize(auth); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: re-authorizing the attempt", ErrLifecycle), err)
	}

	// 9b. The final identity observation, UNDER the guard, is the linearization point the result's
	// unchanged-versus-changed claim refers to.
	obs, err := d.ObserveIdentity(auth)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: observing the final identity", ErrLifecycle), err)
	}

	// 10. Only now can the record be built: it is immutable and states the identity distinction, so
	// before step 9b that field would have been a claim about a fact that did not exist.
	digest, err := d.PublishResult(auth, obs)
	if err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: publishing the result", ErrLifecycle), err)
	}

	// 11. The outcome CAS names the digest of a record that is ALREADY durable, so state never
	// references a result a reader cannot fetch.
	if err := d.FinalizeOutcome(auth, obs, digest); err != nil {
		return Result{}, errors.Join(fmt.Errorf("%w: finalizing the outcome", ErrLifecycle), err)
	}
	return Result{Authorization: auth, Observation: obs, ResultDigest: digest}, nil
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
	missing := []string{}
	for name, fn := range map[string]bool{
		"AcquireGuard":    d.AcquireGuard == nil,
		"ReleaseGuard":    d.ReleaseGuard == nil,
		"Authorize":       d.Authorize == nil,
		"PublishIntent":   d.PublishIntent == nil,
		"ArmContainment":  d.ArmContainment == nil,
		"BindActive":      d.BindActive == nil,
		"Reauthorize":     d.Reauthorize == nil,
		"ObserveIdentity": d.ObserveIdentity == nil,
		"PublishResult":   d.PublishResult == nil,
		"FinalizeOutcome": d.FinalizeOutcome == nil,
	} {
		if fn {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// Sorted so the message is stable rather than dependent on map iteration.
		sortStrings(missing)
		return fmt.Errorf("%w: incomplete wiring, missing %v", ErrPreAttempt, missing)
	}
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
