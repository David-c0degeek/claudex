package proctree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Supervision is one attempt's containment, from a started command to a published terminal.
//
// It exists so the ORDER is a single object's behaviour rather than a convention spread across call
// sites. The design pins that order on every terminal path — normal exit and spawn failure included —
// because the components are individually correct and still wrong if sequenced badly: a well-formed
// exit frame arriving before the group is proven empty lets the gate finalize a pass while a
// grandchild is still writing to the worktree.
type Supervision struct {
	// AttemptID binds the receipt to this attempt.
	AttemptID string
	// Spawn holds the stream write ends the supervisor must drop once the command owns them.
	Spawn CommandSpawn
	// Reaper is the single wait authority for the owned group.
	Reaper *Reaper
	// PGID is the owned command group.
	PGID int
	// Root is the attempt directory the receipt is published into.
	Root *os.Root
	// Lease is released last, after the receipt is durable and the terminal is sent.
	Lease releaser
	// Stat is where the one TERMINAL frame goes.
	Stat io.Writer
	// Protocol enforces one-terminal-only and the launch-phase rules.
	Protocol *SupervisorProtocol
	// Policy bounds the teardown.
	Policy ReapPolicy
	// AwaitDeadline bounds waiting for the command to finish on its own.
	AwaitDeadline time.Time
	// AwaitPoll is the interval between liveness checks while awaiting.
	AwaitPoll time.Duration
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
//     stream EOF, so a drain that the terminal ordering depends on would wait forever.
//  2. Await the command, then tear the group down. Awaiting first is what lets a command that
//     finished normally report what it DID; signalling first reports every command as terminated.
//  3. Publish the cleanup receipt, durably. This is the fact recovery reads after the coordinator
//     dies, so it must exist before anything announces completion.
//  4. Send exactly one TERMINAL, and only after the receipt is durable.
//  5. Release the lease last. Holding it through cleanup is what makes "unheld lease means the owner
//     died" true; releasing earlier would let a recovering coordinator see a free lease while this
//     supervisor was still working.
//
// A failure at any step short-circuits WITHOUT sending a successful terminal, so recovery finds the
// proof absent and blocks rather than trusting an unfinished cleanup.
func RunToTerminal(s Supervision) (Terminal, error) {
	if err := s.validate(); err != nil {
		return Terminal{}, err
	}

	// 1. The command owns the stream ends now.
	if err := CloseStreamCopies(s.Spawn); err != nil {
		return Terminal{}, errors.Join(fmt.Errorf("%w: closing stream copies", ErrTerminalSequence), err)
	}

	// 2. Let it finish, then prove the group empty.
	if _, err := s.Reaper.Await(s.AwaitDeadline, s.AwaitPoll); err != nil {
		return Terminal{}, errors.Join(fmt.Errorf("%w: awaiting the command", ErrTerminalSequence), err)
	}
	res, err := s.Reaper.Teardown(s.Policy)
	if err != nil {
		// A teardown that did not prove the group empty publishes NO receipt and sends no terminal.
		return Terminal{}, errors.Join(fmt.Errorf("%w: tearing down the owned group", ErrTerminalSequence), err)
	}

	term, err := TerminalFrom(res, s.PGID)
	if err != nil {
		return Terminal{}, errors.Join(fmt.Errorf("%w: constructing the terminal", ErrTerminalSequence), err)
	}

	// 3. The durable completion fact, before anything announces completion.
	// Built from the TEARDOWN RESULT, not from the terminal.
	//
	// Deriving the receipt from `term` would make the agreement check below tautological — a guard
	// that reads like one and can never fire, which is worse than no guard. Both facts now come from
	// `res` by independent paths, so a drift in either constructor is caught instead of propagated.
	receipt := CleanupReceipt{
		AttemptID:      s.AttemptID,
		CommandStarted: res.Leader.Observed,
		CommandPGID:    s.PGID,
		Reaped:         res.Reaped,
		GroupEmpty:     res.GroupEmpty,
		CompletedUnix:  s.Now().Unix(),
	}
	// The two facts describe one attempt and are read by different parties — recovery reads the
	// receipt, the coordinator reads the terminal — so they are checked against each other before
	// either is published rather than after both are believed.
	if err := receipt.AgreesWith(term); err != nil {
		return Terminal{}, errors.Join(fmt.Errorf("%w: receipt and terminal disagree", ErrTerminalSequence), err)
	}
	if err := PublishReceipt(s.Root, receipt); err != nil {
		return Terminal{}, errors.Join(fmt.Errorf("%w: publishing the cleanup receipt", ErrTerminalSequence), err)
	}

	// 4. Exactly one terminal, after the receipt is durable.
	if err := s.Protocol.SendTerminal(s.Stat, term); err != nil {
		return Terminal{}, errors.Join(fmt.Errorf("%w: sending the terminal", ErrTerminalSequence), err)
	}

	// 5. The lease goes last.
	if s.Lease != nil {
		if err := s.Lease.Release(); err != nil {
			return term, errors.Join(fmt.Errorf("%w: releasing the lease", ErrTerminalSequence), err)
		}
	}
	return term, nil
}

func (s Supervision) validate() error {
	switch {
	case s.AttemptID == "":
		return fmt.Errorf("%w: no attempt id", ErrTerminalSequence)
	case s.Reaper == nil:
		return fmt.Errorf("%w: no reaper", ErrTerminalSequence)
	case s.PGID <= 0:
		return fmt.Errorf("%w: invalid command group %d", ErrTerminalSequence, s.PGID)
	case s.Root == nil:
		return fmt.Errorf("%w: no attempt directory", ErrTerminalSequence)
	case s.Stat == nil:
		return fmt.Errorf("%w: no status channel", ErrTerminalSequence)
	case s.Protocol == nil:
		return fmt.Errorf("%w: no protocol state", ErrTerminalSequence)
	case s.AwaitPoll <= 0:
		return fmt.Errorf("%w: await poll must be positive", ErrTerminalSequence)
	case s.Now == nil:
		return fmt.Errorf("%w: no clock", ErrTerminalSequence)
	}
	return s.Policy.Validate()
}
