package proctree

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fastPolicy drives the real loop quickly. The loop itself is unmodified: these tests exercise the
// shipped teardown, not a stand-in for it.
// helperEnv re-execs the test binary as a fixture process.
//
// The SIGTERM-immune fixture is a Go helper rather than a shell script on purpose. `trap ” TERM` in
// sh does not produce a TERM-immune GROUP — the shell ignores the signal but the `sleep` it is
// blocked on does not, so the shell exits as soon as that child dies — and reasoning about which
// shell does what is exactly the kind of assumption this subject keeps punishing. A helper that calls
// signal.Ignore is unambiguous, and it is also closer to the real case: a test command that ignores
// SIGTERM.
const helperEnv = "CLAUDEX_PROCTREE_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		// Readiness, announced only AFTER the disposition is installed. Without it the test races the
		// helper's startup: a SIGTERM that arrives first finds the default disposition and kills the
		// process, so the group drains immediately and the escalation and deadline tests pass while
		// proving nothing. That race cost three wrong guesses before it was measured.
		fmt.Println("ready")
		os.Stdout.Sync()
		// A bare select{} would trip the runtime's deadlock detector and exit immediately, which
		// would make the group drain on its own and quietly turn the escalation and deadline tests
		// into no-ops that pass.
		time.Sleep(10 * time.Minute)
		os.Exit(3)
	case "dump":
		// Reports the exact descriptor set, argv and environment the contained command received, so
		// the containment can be asserted from the CHILD's point of view rather than from the
		// parent's intentions.
		dumpChildState()
		os.Exit(0)
	case "":
		os.Exit(m.Run())
	default:
		os.Exit(2)
	}
}

// startTermImmuneLeader launches a group whose leader ignores SIGTERM but cannot ignore SIGKILL.
func startTermImmuneLeader(t *testing.T) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), helperEnv+"=ignore-term")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	ready := make(chan struct{})
	go func() {
		buf := make([]byte, len("ready\n"))
		if _, rerr := io.ReadFull(stdout, buf); rerr == nil {
			close(ready)
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		t.Fatal("helper never signalled readiness")
	}
	return cmd
}

var fastPolicy = ReapPolicy{Grace: 150 * time.Millisecond, Deadline: 10 * time.Second, Poll: 5 * time.Millisecond}

// startGroupLeader launches sh as the leader of a fresh process group, exactly as the supervisor will
// spawn the test command.
func startGroupLeader(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	return cmd
}

func alive(pid int) bool { return unix.Kill(pid, 0) == nil }

// TestReapSurvivingGrandchild is the shape a direct-child wait silently passes.
//
// The leader exits immediately while a background descendant lives on. Without subreaper the orphan
// reparents to init and is never waitable here; with it, the orphan reparents to this process — and
// then the second trap appears: waitpid(-pgid) reports ECHILD while the group is still populated. A
// teardown that drained until ECHILD and stopped would declare the group empty over a live process.
func TestReapSurvivingGrandchild(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	cmd := startGroupLeader(t, "sleep 30 & exit 0")
	pgid := cmd.Process.Pid

	// The leader is reaped here deliberately: this test is about the DESCENDANT that outlives it, so
	// the leader is removed from the picture the way a parent would. Tests that need the leader's exit
	// status let ReapGroup reap it instead — there cannot be two reapers.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("leader wait: %v", err)
	}

	// The trap, asserted directly: nothing is waitable, yet the group is NOT empty.
	deadline := time.Now().Add(5 * time.Second)
	var sawEchildWhilePopulated bool
	for time.Now().Before(deadline) {
		n, _, err := drainGroup(pgid)
		if err != nil {
			t.Fatalf("drainGroup: %v", err)
		}
		empty, err := groupIsEmpty(pgid)
		if err != nil {
			t.Fatalf("groupIsEmpty: %v", err)
		}
		if n == 0 && !empty {
			sawEchildWhilePopulated = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawEchildWhilePopulated {
		t.Fatal("did not observe the drain-exhausted-but-group-populated state this guard exists for")
	}

	res, err := ReapGroup(pgid, fastPolicy)
	if err != nil {
		t.Fatalf("ReapGroup: %v", err)
	}
	if !res.GroupEmpty {
		t.Fatal("ReapGroup returned without proving the group empty")
	}
	if res.Reaped < 1 {
		t.Fatalf("reaped %d; the surviving grandchild was never waited on", res.Reaped)
	}
	if empty, _ := groupIsEmpty(pgid); !empty {
		t.Fatal("group still populated after a successful reap")
	}
}

// TestReapDoesNotWaitOnASetsidEscape is the out-of-model case.
//
// A descendant that setsid()s out of the group cannot be contained by an unprivileged supervisor —
// that is a stated limit, not a hole. But because this process is a subreaper, the escapee still
// REPARENTS here, so an unqualified wait would block on it forever. Scoping waitpid to -pgid excludes
// it by construction. The test proves both halves: the teardown completes promptly, and the escapee
// is neither waited on nor killed.
func TestReapDoesNotWaitOnASetsidEscape(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("setsid unavailable: %v", err)
	}
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}

	pidFile := filepath.Join(t.TempDir(), "escapee.pid")
	cmd := startGroupLeader(t, "setsid /bin/sh -c 'echo $$ > "+pidFile+"; sleep 30' & exit 0")
	pgid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("leader wait: %v", err)
	}

	var escapee int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid > 0 {
				escapee = pid
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if escapee == 0 {
		t.Skip("escapee never reported its pid; setsid semantics differ here")
	}
	t.Cleanup(func() { _ = unix.Kill(escapee, unix.SIGKILL) })

	done := make(chan struct{})
	var res ReapResult
	var rerr error
	go func() {
		res, rerr = ReapGroup(pgid, fastPolicy)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("ReapGroup blocked on an out-of-model child")
	}
	if rerr != nil {
		t.Fatalf("ReapGroup: %v", rerr)
	}
	if !res.GroupEmpty {
		t.Fatal("the owned group should be empty once the escapee has left it")
	}
	if !alive(escapee) {
		t.Fatal("the escapee was killed; it is outside the containment model, and claiming otherwise would overstate the guarantee")
	}
}

func TestReapOrdinaryCommand(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	cmd := startGroupLeader(t, "sleep 30")
	pgid := cmd.Process.Pid
	res, err := ReapGroup(pgid, fastPolicy)
	if err != nil {
		t.Fatalf("ReapGroup: %v", err)
	}
	if !res.GroupEmpty || res.Reaped < 1 {
		t.Fatalf("result = %+v, want an empty group with at least one reaped", res)
	}
}

// TestReapEscalatesToSIGKILL: a command that ignores SIGTERM must not keep the group alive.
func TestReapEscalatesToSIGKILL(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	cmd := startTermImmuneLeader(t)
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = unix.Kill(-pgid, unix.SIGKILL)
		_, _, _ = drainGroup(pgid)
	})
	start := time.Now()
	res, err := ReapGroup(pgid, fastPolicy)
	if err != nil {
		t.Fatalf("ReapGroup: %v", err)
	}
	if !res.GroupEmpty {
		t.Fatal("a SIGTERM-ignoring group was not escalated")
	}
	if elapsed := time.Since(start); elapsed < fastPolicy.Grace {
		t.Fatalf("escalated after %v, before the grace period elapsed", elapsed)
	}
}

// TestReapDeadlineDoesNotClaimSuccess. The whole point of the deadline is that expiry publishes NO
// success receipt: a group that will not drain is exactly the case nobody should retry over, so
// GroupEmpty must stay false and the error must be distinguishable.
func TestReapDeadlineDoesNotClaimSuccess(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	cmd := startTermImmuneLeader(t)
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = unix.Kill(-pgid, unix.SIGKILL)
		_, _, _ = drainGroup(pgid)
	})

	// Grace beyond the deadline, so SIGKILL never arrives and the group cannot drain.
	res, err := ReapGroup(pgid, ReapPolicy{Grace: time.Hour, Deadline: 200 * time.Millisecond, Poll: 5 * time.Millisecond})
	if !errors.Is(err, ErrReapDeadline) {
		t.Fatalf("err = %v, want ErrReapDeadline", err)
	}
	if res.GroupEmpty {
		t.Fatal("a timed-out teardown claimed the group was empty")
	}
}

func TestReapRefusesInvalidGroup(t *testing.T) {
	for _, pgid := range []int{0, -1} {
		if _, err := ReapGroup(pgid, fastPolicy); err == nil {
			t.Fatalf("ReapGroup(%d) was accepted; signalling group 0 would hit the caller's own group", pgid)
		}
	}
}

// --- the teardown must supply the authoritative TERMINAL it exists to support ---

// reapLeaderOnly runs a leader to completion and lets ReapGroup be the SOLE reaper, which is the only
// arrangement in which the leader's exit status survives: a concurrent cmd.Wait would race the group
// wait, and whichever call lost would also lose the only copy of that status.
func reapLeaderOnly(t *testing.T, cmd *exec.Cmd) (ReapResult, int) {
	t.Helper()
	pgid := cmd.Process.Pid
	r, err := NewReaper(pgid)
	if err != nil {
		t.Fatalf("NewReaper: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	// Await first, so a command that finishes normally reports what it DID. Signalling straight away
	// would report every command as terminated by SIGTERM regardless of its actual outcome — which is
	// exactly what happened before these two phases were separated.
	exited, err := r.Await(time.Now().Add(5*time.Second), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if !exited {
		t.Fatal("leader did not exit within the await window")
	}
	res, err := r.Teardown(fastPolicy)
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !res.GroupEmpty {
		t.Fatal("teardown did not prove the group empty")
	}
	return res, pgid
}

func TestTeardownCarriesTheLeaderExitStatus(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	for _, tc := range []struct {
		name string
		code int
	}{
		{"exit 0", 0},
		{"exit 7", 7},
		{"exit 127", 127},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := startGroupLeader(t, fmt.Sprintf("exit %d", tc.code))
			res, pgid := reapLeaderOnly(t, cmd)
			if !res.Leader.Observed {
				t.Fatal("teardown did not observe the leader")
			}
			term, err := TerminalFrom(res, pgid)
			if err != nil {
				t.Fatalf("TerminalFrom: %v", err)
			}
			if !term.Exited || term.ExitCode != tc.code {
				t.Fatalf("terminal = %+v, want a normal exit with code %d", term, tc.code)
			}
			if term.Reaped < 1 {
				t.Fatalf("reaped %d; the leader must be counted by the reaper that waited on it", term.Reaped)
			}
			// Exit 127 is the case the whole design turns on: it must be a statement about the
			// command, distinguishable from any supervisor failure.
			if tc.code == 127 && (!term.CommandStarted || term.Signal != 0) {
				t.Fatalf("exit 127 did not survive as a command fact: %+v", term)
			}
		})
	}
}

// TestTeardownCarriesSignalTermination: a command killed by the escalation must report the signal, not
// a fabricated exit code.
func TestTeardownCarriesSignalTermination(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	cmd := startTermImmuneLeader(t)
	pgid := cmd.Process.Pid
	r, err := NewReaper(pgid)
	if err != nil {
		t.Fatalf("NewReaper: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	// This leader never finishes, so Await must give up rather than hang; the teardown then kills it.
	observed, err := r.Await(time.Now().Add(100*time.Millisecond), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if observed {
		t.Fatal("a command that never exits was reported as observed")
	}
	res, err := r.Teardown(fastPolicy)
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	term, err := TerminalFrom(res, pgid)
	if err != nil {
		t.Fatalf("TerminalFrom: %v", err)
	}
	if term.Exited || term.Signal != int(unix.SIGKILL) {
		t.Fatalf("terminal = %+v, want termination by SIGKILL", term)
	}
}

// TestTeardownOfAnAlreadyExitedLeader is the cancel race in its simplest form: CANCEL is a REQUEST,
// not the outcome fact, so a command that had already finished must still report what it did.
func TestTeardownOfAnAlreadyExitedLeader(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	cmd := startGroupLeader(t, "exit 3")
	// Let it finish and become a zombie before the teardown begins.
	time.Sleep(200 * time.Millisecond)
	res, pgid := reapLeaderOnly(t, cmd)
	term, err := TerminalFrom(res, pgid)
	if err != nil {
		t.Fatalf("TerminalFrom: %v", err)
	}
	if !term.Exited || term.ExitCode != 3 {
		t.Fatalf("terminal = %+v, want the exit that already happened", term)
	}
}

// TestTerminalFromRefusesAnUnobservedLeader: "the command ran but we do not know how it ended" is not
// an outcome TERMINAL may express.
func TestTerminalFromRefusesAnUnobservedLeader(t *testing.T) {
	if _, err := TerminalFrom(ReapResult{Reaped: 2, GroupEmpty: true}, 42); err == nil {
		t.Fatal("TerminalFrom produced a terminal without having observed the leader")
	}
}

// TestTerminalFromRequiresTheGroupEmptyProof. A terminal can be internally consistent and still false
// in context: reporting a clean exit while descendants are still running in the owned group would let
// the gate finalize over live processes.
func TestTerminalFromRequiresTheGroupEmptyProof(t *testing.T) {
	unfinished := ReapResult{
		Reaped:     1,
		GroupEmpty: false,
		Leader:     LeaderOutcome{Observed: true, Exited: true},
	}
	if _, err := TerminalFrom(unfinished, 42); err == nil {
		t.Fatal("TerminalFrom accepted a teardown that never proved the group empty")
	}
}

// TestNoTerminalAfterADeadlinedTeardown is the same rule end to end on a real group: the leader is
// observed, the teardown times out, and no outcome may be claimed.
func TestNoTerminalAfterADeadlinedTeardown(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	cmd := startTermImmuneLeader(t)
	pgid := cmd.Process.Pid
	r, err := NewReaper(pgid)
	if err != nil {
		t.Fatalf("NewReaper: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = unix.Kill(-pgid, unix.SIGKILL)
		_, _, _ = drainGroup(pgid)
	})
	res, terr := r.Teardown(ReapPolicy{Grace: time.Hour, Deadline: 100 * time.Millisecond, Poll: 5 * time.Millisecond})
	if !errors.Is(terr, ErrReapDeadline) {
		t.Fatalf("Teardown: err = %v, want ErrReapDeadline", terr)
	}
	if _, err := TerminalFrom(res, pgid); err == nil {
		t.Fatal("a timed-out teardown produced an authoritative terminal")
	}
}

// TestAwaitDoesNotReleaseTheLeaderIdentity is the window an end-state test cannot see.
//
// If Await reaped the leader, its pid — which IS the group id — would be free the moment the group
// had no other members, and the teardown's kill(-pgid, ...) could then land on an unrelated recycled
// group. Keeping the leader unreaped holds the identity, and the proof is that the group still
// answers a probe after Await reports the exit.
func TestAwaitDoesNotReleaseTheLeaderIdentity(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	cmd := startGroupLeader(t, "exit 5")
	pgid := cmd.Process.Pid
	r, err := NewReaper(pgid)
	if err != nil {
		t.Fatalf("NewReaper: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	exited, err := r.Await(time.Now().Add(5*time.Second), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if !exited {
		t.Fatal("leader did not exit")
	}
	// The leader is a zombie here, not gone: the group must still exist, or its id was released
	// before the teardown ever signalled it.
	empty, err := groupIsEmpty(pgid)
	if err != nil {
		t.Fatalf("groupIsEmpty: %v", err)
	}
	if empty {
		t.Fatal("the group id was released before teardown; a recycled id could be signalled instead")
	}

	res, err := r.Teardown(fastPolicy)
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	term, err := TerminalFrom(res, pgid)
	if err != nil {
		t.Fatalf("TerminalFrom: %v", err)
	}
	if !term.Exited || term.ExitCode != 5 {
		t.Fatalf("terminal = %+v, want the exit captured by the teardown that reaped it", term)
	}
}

// TestReapDeadlineIsNotOvershotByThePoll. The deadline is the function's stated bound, so it must not
// depend on the production constants happening to be ordered: a poll longer than the deadline used to
// sleep in full after the check and overshoot by an entire interval.
func TestReapDeadlineIsNotOvershotByThePoll(t *testing.T) {
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	cmd := startTermImmuneLeader(t)
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = unix.Kill(-pgid, unix.SIGKILL)
		_, _, _ = drainGroup(pgid)
	})
	start := time.Now()
	res, err := ReapGroup(pgid, ReapPolicy{Grace: time.Hour, Deadline: 20 * time.Millisecond, Poll: 500 * time.Millisecond})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrReapDeadline) {
		t.Fatalf("err = %v, want ErrReapDeadline", err)
	}
	if res.GroupEmpty {
		t.Fatal("a timed-out teardown claimed the group was empty")
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("20ms deadline returned after %v; the poll is not capped to the remaining time", elapsed)
	}
}

func TestReapPolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    ReapPolicy
	}{
		{"zero poll", ReapPolicy{Grace: time.Second, Deadline: time.Second, Poll: 0}},
		{"negative poll", ReapPolicy{Grace: time.Second, Deadline: time.Second, Poll: -1}},
		{"zero deadline", ReapPolicy{Grace: time.Second, Deadline: 0, Poll: time.Millisecond}},
		{"negative grace", ReapPolicy{Grace: -1, Deadline: time.Second, Poll: time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.p.Validate(); err == nil {
				t.Fatal("Validate accepted a policy whose bound would not hold")
			}
			if _, err := ReapGroup(1234567, tc.p); err == nil {
				t.Fatal("ReapGroup accepted an invalid policy")
			}
		})
	}
	if err := DefaultReapPolicy.Validate(); err != nil {
		t.Fatalf("the shipped policy must be valid: %v", err)
	}
}

// dumpChildState prints what this process actually holds. The fd listing excludes the descriptor
// opened to read /proc/self/fd itself, which would otherwise always appear as a phantom extra.
func dumpChildState() {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		fmt.Println("FDERR", err)
		return
	}
	var fds []string
	for _, e := range entries {
		target, lerr := os.Readlink("/proc/self/fd/" + e.Name())
		if lerr != nil {
			// The reading descriptor is gone by the time we resolve it, or is the directory itself.
			continue
		}
		if strings.HasPrefix(target, "/proc/") && strings.HasSuffix(target, "/fd") {
			continue
		}
		fds = append(fds, e.Name()+"="+target)
	}
	sort.Strings(fds)
	fmt.Println("FDS", strings.Join(fds, ","))
	for i, a := range os.Args {
		fmt.Printf("ARG %d %s\n", i, base64.StdEncoding.EncodeToString([]byte(a)))
	}
	env := os.Environ()
	sort.Strings(env)
	for _, kv := range env {
		fmt.Println("ENV", base64.StdEncoding.EncodeToString([]byte(kv)))
	}
	fmt.Println("PGID", syscall.Getpgrp())
}
