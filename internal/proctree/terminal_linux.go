package proctree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// LaunchOutcome is which of the three things actually happened to the command.
//
// All three go through the same ordered authority. An earlier version accepted only a started
// command, which left the two failure paths to a call-site convention in a later slice — while this
// file claimed the order covered "every terminal path". The claim was the design's; the code was not.
type LaunchOutcome int

const (
	// LaunchAborted: CANCEL arrived before GO. Nothing was attempted, so there is no group and no
	// spawn error — only a no-command receipt and terminal.
	LaunchAborted LaunchOutcome = iota + 1
	// LaunchSpawnFailed: GO was given and the command could not start. Still no group, but the
	// terminal carries the spawn-failure class, which is a statement about the configured command.
	LaunchSpawnFailed
	// LaunchStarted: the command ran and owns a process group that must be torn down.
	LaunchStarted
)

func (o LaunchOutcome) String() string {
	switch o {
	case LaunchAborted:
		return "aborted"
	case LaunchSpawnFailed:
		return "spawn-failed"
	case LaunchStarted:
		return "started"
	}
	return "unknown"
}

// Supervision is one attempt's containment, from a launch outcome to a published terminal.
//
// It exists so the ORDER is a single object's behaviour rather than a convention spread across call
// sites. The components are individually correct and still wrong if sequenced badly: a well-formed
// exit frame arriving before the group is proven empty lets the gate finalize a pass while a
// grandchild is still writing to the worktree.
type Supervision struct {
	// Outcome selects which launch actually happened. All three are handled here.
	Outcome LaunchOutcome
	// AttemptID binds the receipt to this attempt.
	AttemptID string
	// Spawn holds the stream write ends the supervisor must drop. Present for every outcome, because
	// the coordinator is waiting for their EOF whether or not a command ever ran.
	Spawn CommandSpawn
	// Reaper is the SINGLE authority for the owned group: whether one exists, what its id is, and what
	// its teardown proved. It is required for LaunchStarted and must be absent otherwise.
	//
	// There is deliberately no companion PGID field. One existed, validation only required it to be
	// positive, and teardown ran on the reaper's group while the terminal and the receipt both recorded
	// the field — so a successful, self-consistent pair of durable facts could certify a group nobody
	// had torn down.
	Reaper *Reaper
	// SpawnErrorClass is required for LaunchSpawnFailed and must be empty otherwise.
	SpawnErrorClass string
	// Root is the attempt directory the receipt is published into.
	Root *os.Root
	// Lease is released last, after the receipt is durable and the terminal is sent. It is REQUIRED:
	// recovery's held/unheld interpretation depends on this object owning that step, so a wiring
	// omission must not be able to publish success without it.
	Lease releaser
	// Stat is where the one TERMINAL frame goes.
	Stat io.Writer
	// Protocol enforces one-terminal-only and the launch-phase rules.
	Protocol *SupervisorProtocol
	// Policy bounds the teardown; AwaitDeadline/AwaitPoll bound waiting for a normal exit.
	Policy        ReapPolicy
	AwaitDeadline time.Time
	AwaitPoll     time.Duration
	// Now supplies the receipt timestamp; injected so a test does not depend on the clock.
	Now func() time.Time
}

// releaser is the lease's shape, kept minimal so tests need no filesystem lock.
type releaser interface{ Release() error }

// ErrTerminalSequence marks a failure of the ordered shutdown itself.
var ErrTerminalSequence = errors.New("proctree: terminal sequence failed")

// RunToTerminal executes the ordered shutdown and returns the terminal it published.
//
// The order, and why each step is where it is:
//
//  1. Close the supervisor's stream write copies. Until they are gone the coordinator cannot see
//     stream EOF, so a drain that the terminal ordering depends on would wait forever. This runs for
//     every outcome, because the coordinator is waiting whether or not a command existed.
//  2. For a started command: await it, then tear the group down. Awaiting first is what lets a
//     command that finished normally report what it DID; signalling first reports every command as
//     terminated. Teardown is attempted UNCONDITIONALLY once a command started — see below.
//  3. Publish the cleanup receipt, durably. This is the fact recovery reads after the coordinator
//     dies, so it must exist before anything announces completion.
//  4. Send exactly one TERMINAL, and only after the receipt is durable.
//  5. Release the lease last. Holding it through cleanup is what makes "an unheld lease means the
//     owner died" true; releasing earlier would let a recovering coordinator see a free lease while
//     this supervisor was still working.
//
// Errors before teardown do NOT short-circuit the teardown. A supervisor that returned early on a
// close or await failure would exit with the command group still alive — the lease released by
// process death, the domain uncontained, and the worktree still mutable. Recovery blocking on an
// absent receipt does not clean that up. So earlier errors are accumulated, containment cleanup still
// runs, and the accumulated failure withholds the successful terminal.
func RunToTerminal(s Supervision) (Terminal, error) {
	if err := s.validate(); err != nil {
		// A MALFORMED supervision must not leave a live group behind either.
		//
		// Accumulating close and await failures fixed the same hole one step further in, and this gate
		// still returned early — so a nil status channel, a missing lease or a bad clock left the
		// command running while the supervisor exited. The reason to clean up is the state of the
		// world, not the validity of the request, and the two are independent.
		return Terminal{}, errors.Join(err, s.emergencyTeardown())
	}

	var problems []error

	// 1. The command owns the stream ends now — for every outcome.
	if err := CloseStreamCopies(s.Spawn); err != nil {
		problems = append(problems, fmt.Errorf("%w: closing stream copies", ErrTerminalSequence), err)
	}

	// Keyed on the REAPER, not on Outcome. "A group is running" is one fact with one owner, and every
	// path that must act on it — this one and the emergency path below — asks the same question of the
	// same object. Validation has already bound a reaper to LaunchStarted in both directions, so this
	// is not a second, looser rule; it is the same rule stated where the work happens.
	if s.Reaper != nil {
		// 2. Let it finish, then prove the group empty. An await failure is recorded, never a reason
		// to leave the group standing.
		if _, err := s.Reaper.Await(s.AwaitDeadline, s.AwaitPoll); err != nil {
			problems = append(problems, fmt.Errorf("%w: awaiting the command", ErrTerminalSequence), err)
		}
		if _, terr := s.Reaper.Teardown(s.Policy); terr != nil {
			problems = append(problems, fmt.Errorf("%w: tearing down the owned group", ErrTerminalSequence), terr)
		}
	}

	if len(problems) > 0 {
		// Containment cleanup has been attempted; the attempt simply cannot be reported as finished.
		return Terminal{}, errors.Join(problems...)
	}

	term, receipt, err := s.facts()
	if err != nil {
		return Terminal{}, err
	}
	// The two facts describe one attempt and are read by different parties — recovery reads the
	// receipt, the coordinator reads the terminal — so they are checked against each other before
	// either is published rather than after both are believed.
	//
	// Stated honestly about what it is worth HERE: both facts now derive from the one reaper, so the
	// comparison holds by construction for the code as written and is not catching a live hazard today.
	// It still fires the moment either derivation drifts — a receipt built from anything but the
	// reaper's own group and result disagrees immediately — so it is a real regression net against
	// re-splitting the sources rather than a guard that can never fire. Its other firing site is the
	// reader: recovery holding a receipt off disk and the coordinator holding a terminal that arrived
	// over a pipe from a different process are genuinely independent, which is what the method is for.
	if err := receipt.AgreesWith(term); err != nil {
		return Terminal{}, errors.Join(fmt.Errorf("%w: receipt and terminal disagree", ErrTerminalSequence), err)
	}

	// 3. The durable completion fact, before anything announces completion.
	if err := PublishReceipt(s.Root, receipt); err != nil {
		return Terminal{}, errors.Join(fmt.Errorf("%w: publishing the cleanup receipt", ErrTerminalSequence), err)
	}
	// 4. Exactly one terminal, after the receipt is durable.
	if err := s.Protocol.SendTerminal(s.Stat, term); err != nil {
		return Terminal{}, errors.Join(fmt.Errorf("%w: sending the terminal", ErrTerminalSequence), err)
	}
	// 5. The lease goes last.
	if err := s.Lease.Release(); err != nil {
		return term, errors.Join(fmt.Errorf("%w: releasing the lease", ErrTerminalSequence), err)
	}
	return term, nil
}

// emergencyTeardown reaps the owned group when the sequence cannot proceed at all.
//
// The capability to clean up is the REAPER's existence and NOTHING else. Every other field on this
// struct — Outcome, the attempt id, the lease, the clock, the policy — describes what will be
// REPORTED, and a malformed report is precisely when a live group most needs tearing down. Gating
// cleanup on any of them recreated the abandoned-group defect through whichever discriminator was
// checked first: with the old ownership triple, a real started supervision whose PGID field had been
// left at zero, or whose Outcome had been set to a no-command value, failed validation and walked away
// from a group its identity-bound reaper was perfectly able to tear down.
//
// The policy is not trusted either, for the same reason one step down: an invalid one falls back to
// the shipped default rather than becoming another way to skip the teardown. The stream copies are
// dropped too, because the coordinator is waiting for their EOF regardless of why this failed.
func (s Supervision) emergencyTeardown() error {
	var problems []error
	if err := CloseStreamCopies(s.Spawn); err != nil {
		problems = append(problems, err)
	}
	if s.Reaper == nil {
		return errors.Join(problems...)
	}
	policy := s.Policy
	if policy.Validate() != nil {
		policy = DefaultReapPolicy
	}
	if _, err := s.Reaper.Teardown(policy); err != nil {
		problems = append(problems, fmt.Errorf("%w: emergency teardown", ErrTerminalSequence), err)
	}
	return errors.Join(problems...)
}

// facts derives the terminal and the receipt for the outcome.
//
// Both read the group's identity and its teardown from the one reaper, so neither can describe a group
// the other did not. The receipt is still built from the raw ReapResult rather than from the terminal,
// so a future change that re-splits the sources is caught by the agreement check rather than silently
// agreeing with itself.
func (s Supervision) facts() (Terminal, CleanupReceipt, error) {
	receipt := CleanupReceipt{
		AttemptID:     s.AttemptID,
		CompletedUnix: s.Now().Unix(),
	}
	var term Terminal

	switch s.Outcome {
	case LaunchStarted:
		t, err := s.Reaper.Terminal()
		if err != nil {
			return Terminal{}, CleanupReceipt{}, errors.Join(fmt.Errorf("%w: constructing the terminal", ErrTerminalSequence), err)
		}
		term = t
		res := s.Reaper.Result()
		receipt.CommandStarted = res.Leader.Observed
		receipt.CommandPGID = s.Reaper.PGID()
		receipt.Reaped = res.Reaped
		receipt.GroupEmpty = res.GroupEmpty
	case LaunchAborted, LaunchSpawnFailed:
		// No command ever ran, so there is no group to prove empty by observation — the proof is
		// structural: nothing was started, so nothing can remain. The receipt states that explicitly
		// rather than leaving GroupEmpty false, which would read as an unfinished teardown.
		term = Terminal{CommandStarted: false, SpawnErrorClass: s.SpawnErrorClass}
		receipt.GroupEmpty = true
	default:
		return Terminal{}, CleanupReceipt{}, fmt.Errorf("%w: unknown launch outcome", ErrTerminalSequence)
	}
	return term, receipt, nil
}

func (s Supervision) validate() error {
	switch {
	case s.AttemptID == "":
		return fmt.Errorf("%w: no attempt id", ErrTerminalSequence)
	case s.Root == nil:
		return fmt.Errorf("%w: no attempt directory", ErrTerminalSequence)
	case s.Stat == nil:
		return fmt.Errorf("%w: no status channel", ErrTerminalSequence)
	case s.Protocol == nil:
		return fmt.Errorf("%w: no protocol state", ErrTerminalSequence)
	case s.Lease == nil:
		// Required, not optional: recovery's held/unheld interpretation depends on this object owning
		// the release step, so a wiring omission must not be able to publish success without it.
		return fmt.Errorf("%w: no lease", ErrTerminalSequence)
	case s.Now == nil:
		return fmt.Errorf("%w: no clock", ErrTerminalSequence)
	}

	switch s.Outcome {
	case LaunchStarted:
		// The reaper is the whole ownership requirement. Its group id is positive by construction, so
		// there is no separate number to check — and therefore none to disagree.
		if s.Reaper == nil {
			return fmt.Errorf("%w: a started command needs a reaper", ErrTerminalSequence)
		}
		if s.SpawnErrorClass != "" {
			return fmt.Errorf("%w: a started command cannot carry a spawn error", ErrTerminalSequence)
		}
		if s.AwaitPoll <= 0 {
			return fmt.Errorf("%w: await poll must be positive", ErrTerminalSequence)
		}
		return s.Policy.Validate()
	case LaunchSpawnFailed:
		if s.SpawnErrorClass == "" {
			return fmt.Errorf("%w: a spawn failure must name its class", ErrTerminalSequence)
		}
	case LaunchAborted:
		if s.SpawnErrorClass != "" {
			return fmt.Errorf("%w: an aborted launch never attempted a spawn", ErrTerminalSequence)
		}
	default:
		return fmt.Errorf("%w: no launch outcome", ErrTerminalSequence)
	}
	// No command: a reaper here would mean the caller believes something ran. Refusing it is what makes
	// "a reaper exists" and "the outcome is LaunchStarted" the same statement, so the cleanup path can
	// key on either without them ever being able to disagree.
	if s.Reaper != nil {
		return fmt.Errorf("%w: %v launch cannot own a process group", ErrTerminalSequence, s.Outcome)
	}
	return nil
}
