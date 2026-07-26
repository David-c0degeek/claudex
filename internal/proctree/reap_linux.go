package proctree

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// ReapPolicy is the timing of a group teardown. It is a parameter rather than a set of constants so
// tests can drive the real loop quickly instead of asserting against a mock of it.
type ReapPolicy struct {
	// Grace is how long the group gets after SIGTERM before SIGKILL. A test command deserves the
	// chance to remove its own temporary files; it does not get to decline.
	Grace time.Duration
	// Deadline bounds the whole teardown. Its expiry is not a failure to report and move past: the
	// supervisor publishes NO success receipt, so recovery finds the proof absent and blocks.
	Deadline time.Duration
	// Poll is the interval between drain attempts.
	Poll time.Duration
}

// DefaultReapPolicy is what the supervisor uses in production.
var DefaultReapPolicy = ReapPolicy{
	Grace:    2 * time.Second,
	Deadline: 30 * time.Second,
	Poll:     10 * time.Millisecond,
}

// ReapResult is what the teardown proved.
type ReapResult struct {
	// Reaped counts descendants actually waited on. It is only truthful because the supervisor is a
	// child subreaper: without that, an orphaned grandchild reparents to init and can never be
	// counted here, so the number would silently under-report.
	Reaped int
	// GroupEmpty is the ESRCH proof. False means the deadline expired with processes still in the
	// group, and no success receipt may be published.
	GroupEmpty bool
}

// ErrReapDeadline means the owned group did not drain in time.
var ErrReapDeadline = errors.New("proctree: owned group did not drain before the cleanup deadline")

// BecomeSubreaper makes this process the reaper of its orphaned descendants.
//
// It must be called BEFORE the command is spawned. The supervisor is the parent only of the command
// leader, so once ordinary grandchildren orphan they are not waitable by it at all — a reap count
// gathered without this would be a number the mechanism cannot support, and "the group was drained"
// would be a claim about processes nobody ever waited on.
func BecomeSubreaper() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("proctree: PR_SET_CHILD_SUBREAPER: %w", err)
	}
	return nil
}

// groupIsEmpty reports whether any process remains in pgid.
//
// kill(-pgid, 0) returns ESRCH when no process is in the group. EPERM is deliberately NOT treated as
// empty: it means a process exists that this supervisor may not signal, which is the opposite of the
// fact being sought.
func groupIsEmpty(pgid int) (bool, error) {
	err := unix.Kill(-pgid, 0)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, unix.ESRCH):
		return true, nil
	default:
		return false, fmt.Errorf("proctree: probe group %d: %w", pgid, err)
	}
}

// signalGroup signals every member of the owned group, not just the leader. A leader that has already
// exited leaves background descendants that a leader-only signal would never reach.
func signalGroup(pgid int, sig unix.Signal) error {
	err := unix.Kill(-pgid, sig)
	if err == nil || errors.Is(err, unix.ESRCH) {
		return nil
	}
	return fmt.Errorf("proctree: signal group %d with %v: %w", pgid, sig, err)
}

// drainGroup reaps everything currently waitable in the owned group, without blocking.
//
// waitpid is scoped to -pgid rather than -1 on purpose. A process that deliberately setsid()s out of
// the group is outside the containment model but, because this supervisor is a subreaper, it can
// still REPARENT here — and an unqualified wait would then block on an out-of-model child forever.
// Group scoping excludes it by construction rather than by hoping it exits.
func drainGroup(pgid int) (int, error) {
	reaped := 0
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-pgid, &ws, unix.WNOHANG, nil)
		switch {
		case err == nil && pid > 0:
			reaped++
			continue
		case err == nil && pid == 0:
			// Children exist but none has exited yet.
			return reaped, nil
		case errors.Is(err, unix.ECHILD):
			// No children in this group are ours right now. That is NOT the same as the group being
			// empty: an in-group grandchild may not have reparented yet, which is why the caller
			// probes separately instead of stopping here.
			return reaped, nil
		case errors.Is(err, unix.EINTR):
			continue
		default:
			return reaped, fmt.Errorf("proctree: wait on group %d: %w", pgid, err)
		}
	}
}

// ReapGroup tears down the owned command group and proves it empty.
//
// The sequence is exact, and each step exists because a shorter one is wrong:
//
//  1. signal the whole GROUP, since the leader may already be gone;
//  2. drain with waitpid(-pgid), scoped so a setsid escapee cannot block us forever;
//  3. probe with kill(-pgid, 0) — ECHILD and group-empty are DIFFERENT facts, and ECHILD can be true
//     before an in-group grandchild has reparented, so draining until ECHILD and stopping there would
//     declare victory over a group that still has members;
//  4. escalate to SIGKILL once the grace has passed;
//  5. give up at the deadline WITHOUT claiming success, so recovery blocks rather than retrying over
//     a group that will not drain.
func ReapGroup(pgid int, p ReapPolicy) (ReapResult, error) {
	if pgid <= 0 {
		return ReapResult{}, fmt.Errorf("proctree: refusing to signal group %d", pgid)
	}
	var res ReapResult

	if err := signalGroup(pgid, unix.SIGTERM); err != nil {
		return res, err
	}
	start := time.Now()
	escalated := false

	for {
		n, err := drainGroup(pgid)
		res.Reaped += n
		if err != nil {
			return res, err
		}
		empty, err := groupIsEmpty(pgid)
		if err != nil {
			return res, err
		}
		if empty {
			res.GroupEmpty = true
			return res, nil
		}
		elapsed := time.Since(start)
		if !escalated && elapsed >= p.Grace {
			if err := signalGroup(pgid, unix.SIGKILL); err != nil {
				return res, err
			}
			escalated = true
		}
		if elapsed >= p.Deadline {
			return res, fmt.Errorf("%w: group %d after %v, %d reaped", ErrReapDeadline, pgid, elapsed, res.Reaped)
		}
		time.Sleep(p.Poll)
	}
}
