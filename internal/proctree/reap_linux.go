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

// Validate rejects a policy whose stated bound would not hold.
//
// The production constants happen to be ordered sensibly, but a function that claims to bound its own
// runtime must not depend on that accident: a Poll larger than the Deadline, or a nonpositive Poll,
// turns the bound into a suggestion or a hot loop.
func (p ReapPolicy) Validate() error {
	switch {
	case p.Poll <= 0:
		return fmt.Errorf("proctree: reap poll must be positive, got %v", p.Poll)
	case p.Deadline <= 0:
		return fmt.Errorf("proctree: reap deadline must be positive, got %v", p.Deadline)
	case p.Grace < 0:
		return fmt.Errorf("proctree: reap grace must not be negative, got %v", p.Grace)
	}
	return nil
}

// DefaultReapPolicy is what the supervisor uses in production.
var DefaultReapPolicy = ReapPolicy{
	Grace:    2 * time.Second,
	Deadline: 30 * time.Second,
	Poll:     10 * time.Millisecond,
}

// LeaderOutcome is the command leader's exact exit status, captured by the ONE process that reaps it.
//
// It lives here rather than being read separately by the caller because there cannot be two reapers.
// A caller that ran cmd.Wait() alongside this loop would race it, and whichever call lost would also
// lose the only copy of the leader's status — the fact TERMINAL exists to carry. Reaping it here and
// counting it in the same pass removes the race instead of coordinating it.
type LeaderOutcome struct {
	// Observed is false when the leader was never seen, which means no outcome may be claimed.
	Observed bool
	// Exited and ExitCode describe a normal exit; Signal describes a termination. Exactly one applies.
	Exited   bool
	ExitCode int
	Signal   int
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
	// Leader is the command leader's exit status, captured during the drain.
	Leader LeaderOutcome
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
func drainGroup(pgid int) (int, LeaderOutcome, error) {
	reaped := 0
	var leader LeaderOutcome
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-pgid, &ws, unix.WNOHANG, nil)
		switch {
		case err == nil && pid > 0:
			reaped++
			// The leader is the process whose pid IS the group id, since it created the group. This is
			// the only place its status exists, so it is captured here rather than left to a second
			// reaper that would race this one.
			if pid == pgid {
				leader = LeaderOutcome{Observed: true}
				if ws.Signaled() {
					leader.Signal = int(ws.Signal())
				} else {
					leader.Exited = true
					leader.ExitCode = ws.ExitStatus()
				}
			}
			continue
		case err == nil && pid == 0:
			// Children exist but none has exited yet.
			return reaped, leader, nil
		case errors.Is(err, unix.ECHILD):
			// No children in this group are ours right now. That is NOT the same as the group being
			// empty: an in-group grandchild may not have reparented yet, which is why the caller
			// probes separately instead of stopping here.
			return reaped, leader, nil
		case errors.Is(err, unix.EINTR):
			continue
		default:
			return reaped, leader, fmt.Errorf("proctree: wait on group %d: %w", pgid, err)
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
	var acc ReapResult
	return reapGroup(pgid, p, &acc)
}

func reapGroup(pgid int, p ReapPolicy, acc *ReapResult) (ReapResult, error) {
	if pgid <= 0 {
		return ReapResult{}, fmt.Errorf("proctree: refusing to signal group %d", pgid)
	}
	if err := p.Validate(); err != nil {
		return ReapResult{}, err
	}
	res := *acc

	if err := signalGroup(pgid, unix.SIGTERM); err != nil {
		return res, err
	}
	start := time.Now()
	// Absolute, so the bound is a property of this call rather than of how the increments happen to
	// add up.
	hardDeadline := start.Add(p.Deadline)
	escalated := false

	for {
		n, leader, err := drainGroup(pgid)
		res.Reaped += n
		if leader.Observed {
			res.Leader = leader
		}
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
		if !time.Now().Before(hardDeadline) {
			return res, fmt.Errorf("%w: group %d after %v, %d reaped", ErrReapDeadline, pgid, elapsed, res.Reaped)
		}
		// Capped: sleeping the full poll interval after checking the deadline would overshoot it by up
		// to one interval, so a 20ms deadline with a 500ms poll returned after 500ms. The stated bound
		// must not depend on the production constants happening to be ordered.
		nap := p.Poll
		if remaining := time.Until(hardDeadline); remaining < nap {
			nap = remaining
		}
		if nap > 0 {
			time.Sleep(nap)
		}
	}
}

// TerminalFrom builds the authoritative TERMINAL from a completed teardown.
//
// It exists so the composition is pinned rather than left to each caller: the leader's status, the
// reap count and the group id all come from the ONE teardown that observed them, which is what keeps
// the terminal and the cleanup receipt describing the same attempt.
//
// A teardown that never observed the leader cannot produce a terminal at all — "the command ran but
// we do not know how it ended" is not one of the outcomes TERMINAL is allowed to express.
func TerminalFrom(res ReapResult, pgid int) (Terminal, error) {
	if !res.Leader.Observed {
		return Terminal{}, fmt.Errorf("proctree: teardown never observed the command leader; no outcome can be claimed")
	}
	// The ESRCH proof is part of what makes the terminal TRUE, not a separate concern. A terminal can
	// be internally consistent and still false in context: reporting "exited 0" while descendants are
	// still running in the owned group would let the gate finalize over live processes, which is the
	// same phase/value gap found at the launch boundary, recreated at the cleanup boundary.
	if !res.GroupEmpty {
		return Terminal{}, fmt.Errorf("proctree: teardown did not prove the group empty; no outcome can be claimed")
	}
	t := Terminal{
		CommandStarted: true,
		CommandPGID:    pgid,
		Reaped:         res.Reaped,
		Exited:         res.Leader.Exited,
		ExitCode:       res.Leader.ExitCode,
		Signal:         res.Leader.Signal,
	}
	if err := t.validate(); err != nil {
		return Terminal{}, err
	}
	return t, nil
}

// Reaper is the SINGLE wait authority for one owned command group.
//
// Awaiting normal completion and tearing the group down are different operations, but they cannot be
// different reapers: a cmd.Wait() running alongside a waitpid(-pgid) loop races it, and whichever call
// loses also loses the only copy of the leader's exit status — the fact TERMINAL exists to carry.
// Splitting them into two methods of one accumulator gives the lifecycle its two phases without ever
// creating a second reaper.
//
// The supervisor uses it as: NewReaper before the command is spawned, Await while it runs, Teardown to
// clear whatever remains, then TerminalFrom over the accumulated result.
type Reaper struct {
	pgid     int
	leaderFd int
	res      ReapResult
}

// NewReaper binds a reaper to an owned group and takes an identity-bound handle on its leader.
//
// The pidfd is what keeps the group id from being recycled underneath the teardown. Detecting the
// leader's exit by REAPING it would free its PID the moment the group had no other members — and
// because the leader's pid IS the group id, a recycled number in the gap before teardown signals
// would send that signal to an unrelated group. A pidfd reports the exit without reaping, so the
// leader stays a zombie, its pid stays reserved, and the group identity remains ours until teardown
// finishes with it. Keeping the number in a Go struct gives no such affinity, which is exactly why
// the design already refuses a bare PGID for a later process.
func NewReaper(pgid int) (*Reaper, error) {
	if pgid <= 0 {
		return nil, fmt.Errorf("proctree: refusing to reap group %d", pgid)
	}
	fd, err := unix.PidfdOpen(pgid, 0)
	if err != nil {
		return nil, fmt.Errorf("proctree: pidfd_open on leader %d: %w", pgid, err)
	}
	return &Reaper{pgid: pgid, leaderFd: fd}, nil
}

// Close releases the leader handle. It is safe to call more than once.
func (r *Reaper) Close() error {
	if r.leaderFd < 0 {
		return nil
	}
	err := unix.Close(r.leaderFd)
	r.leaderFd = -1
	return err
}

// Result is everything observed so far.
func (r *Reaper) Result() ReapResult { return r.res }

// Await waits for the command leader to EXIT, without reaping it and without signalling anything.
//
// Two properties matter and neither is incidental. It does not signal, because a teardown that starts
// by killing would report every normally-finishing command as terminated by SIGTERM — that is not a
// hypothetical, it is what the first version of this code did. And it does not reap, because reaping
// is what frees the identity: the leader remains a zombie, so the group id stays ours until teardown
// has finished signalling it.
//
// The leader's exit STATUS is therefore not captured here; it is captured when teardown reaps it.
// Returns true when the leader has exited.
func (r *Reaper) Await(deadline time.Time, poll time.Duration) (bool, error) {
	if poll <= 0 {
		return false, fmt.Errorf("proctree: await poll must be positive, got %v", poll)
	}
	if r.leaderFd < 0 {
		return false, errors.New("proctree: reaper is closed")
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		wait := remaining
		if poll < wait {
			wait = poll
		}
		fds := []unix.PollFd{{Fd: int32(r.leaderFd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(wait.Milliseconds())+1)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return false, fmt.Errorf("proctree: poll leader pidfd: %w", err)
		}
		// A pidfd becomes readable exactly when its process terminates, so this is the exit signal
		// without the reap that would release the identity.
		if n > 0 && fds[0].Revents&unix.POLLIN != 0 {
			return true, nil
		}
	}
}

// Teardown signals the group and proves it empty, accumulating into the same result.
func (r *Reaper) Teardown(p ReapPolicy) (ReapResult, error) {
	out, err := reapGroup(r.pgid, p, &r.res)
	if err != nil {
		return r.res, err
	}
	r.res = out
	return r.res, nil
}
