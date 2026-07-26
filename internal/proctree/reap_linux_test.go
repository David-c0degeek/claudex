package proctree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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

	// Reap the leader the way its parent would, leaving only the background descendant.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("leader wait: %v", err)
	}

	// The trap, asserted directly: nothing is waitable, yet the group is NOT empty.
	deadline := time.Now().Add(5 * time.Second)
	var sawEchildWhilePopulated bool
	for time.Now().Before(deadline) {
		n, err := drainGroup(pgid)
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
		_, _ = drainGroup(pgid)
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
		_, _ = drainGroup(pgid)
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
