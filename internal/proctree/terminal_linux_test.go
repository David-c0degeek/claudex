package proctree

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The ordering instruments below are the point of this file.
//
// A test that runs the sequence and afterwards checks a receipt, one frame and one release all exist
// would stay green if the terminal were sent before the receipt, or the lease released before either.
// That is presence, not order — and order is the entire reason this object exists. So each step
// asserts, AT THE INSTANT IT RUNS, what must already be true and what must not yet have happened.

// orderingLease asserts at release time that the receipt is durable and the terminal already sent.
type orderingLease struct {
	t        *testing.T
	root     *os.Root
	stat     *bytes.Buffer
	released int
	err      error
}

func (l *orderingLease) Release() error {
	l.t.Helper()
	l.released++
	if _, err := ReadReceipt(l.root, "att-1"); err != nil {
		l.t.Errorf("lease released before the receipt was durable: %v", err)
	}
	if l.stat.Len() == 0 {
		l.t.Error("lease released before the terminal was sent")
	}
	return l.err
}

// orderingStat asserts at write time that the receipt is already durable and the lease still held.
type orderingStat struct {
	t     *testing.T
	root  *os.Root
	buf   *bytes.Buffer
	lease *orderingLease
	err   error
}

func (w *orderingStat) Write(p []byte) (int, error) {
	w.t.Helper()
	if _, err := ReadReceipt(w.root, "att-1"); err != nil {
		w.t.Errorf("terminal sent before the receipt was durable: %v", err)
	}
	if w.lease != nil && w.lease.released != 0 {
		w.t.Error("terminal sent after the lease was already released")
	}
	if w.err != nil {
		return 0, w.err
	}
	return w.buf.Write(p)
}

// superviseStarted wires a real contained command to a real reaper, real receipt publication and the
// real protocol, with the ordering instruments in place.
//
// It returns the leader's pid as os/exec reported it. That is an identity source INDEPENDENT of the
// reaper, and it exists so a test can check that the durable facts describe the group that was
// actually spawned rather than merely agreeing with whatever the supervision was handed.
func superviseStarted(t *testing.T, script string, tweak func(*Supervision)) (Supervision, *bytes.Buffer, *os.Root, *orderingLease, int) {
	t.Helper()
	if err := BecomeSubreaper(); err != nil {
		t.Fatalf("BecomeSubreaper: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { outR.Close(); errR.Close() })

	spec := ExecSpec{
		Executable: []byte("/bin/sh"),
		Argv:       [][]byte{[]byte("sh"), []byte("-c"), []byte(script)},
		Cwd:        []byte(t.TempDir()),
		Env:        []EnvVar{{Name: []byte("PATH"), Value: []byte("/usr/bin:/bin")}},
		Identity:   NameByteExact,
	}
	sp := CommandSpawn{Spec: spec, Stdout: outW, Stderr: errW}
	cmd, err := SpawnContained(sp)
	if err != nil {
		t.Fatalf("SpawnContained: %v", err)
	}
	leaderPID := cmd.Process.Pid
	r, err := NewReaper(leaderPID)
	if err != nil {
		t.Fatalf("NewReaper: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	s, buf, root, lease := baseSupervision(t, LaunchStarted)
	s.Spawn = sp
	s.Reaper = r
	if tweak != nil {
		tweak(&s)
	}
	return s, buf, root, lease, leaderPID
}

func baseSupervision(t *testing.T, outcome LaunchOutcome) (Supervision, *bytes.Buffer, *os.Root, *orderingLease) {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { root.Close() })

	buf := &bytes.Buffer{}
	lease := &orderingLease{t: t, root: root, stat: buf}
	stat := &orderingStat{t: t, root: root, buf: buf, lease: lease}

	proto := &SupervisorProtocol{}
	ready := &bytes.Buffer{}
	if err := proto.SendReady(ready); err != nil {
		t.Fatalf("SendReady: %v", err)
	}
	// The protocol phase must MATCH the outcome, and the fact this is necessary is itself the
	// launch-phase coupling working: an abort-shape terminal is refused in the authorized phase,
	// because after GO "nothing happened" is not among the things that can be true.
	drive := Frame{Type: TypeGo, Payload: proto.challenge}
	if outcome == LaunchAborted {
		drive = Frame{Type: TypeCancel}
	}
	if _, err := proto.Recv(drive); err != nil {
		t.Fatalf("drive protocol to %v: %v", outcome, err)
	}
	return Supervision{
		Outcome:       outcome,
		AttemptID:     "att-1",
		Root:          root,
		Lease:         lease,
		Stat:          stat,
		Protocol:      proto,
		Policy:        fastPolicy,
		AwaitDeadline: time.Now().Add(10 * time.Second),
		AwaitPoll:     20 * time.Millisecond,
		Now:           func() time.Time { return time.Unix(1753500000, 0) },
	}, buf, root, lease
}

// TestTerminalSequenceOrderIsEnforced is the ordering proof: the instruments fail the test at the
// instant a step happens out of order, so a reordering is caught even though the end state looks the
// same.
func TestTerminalSequenceOrderIsEnforced(t *testing.T) {
	s, buf, root, lease, leaderPID := superviseStarted(t, "exit 3", nil)

	term, err := RunToTerminal(s)
	if err != nil {
		t.Fatalf("RunToTerminal: %v", err)
	}
	if !term.Exited || term.ExitCode != 3 {
		t.Fatalf("terminal = %+v, want a normal exit 3", term)
	}
	got, err := ReadReceipt(root, "att-1")
	if err != nil {
		t.Fatalf("ReadReceipt: %v", err)
	}
	if err := got.AgreesWith(term); err != nil {
		t.Fatalf("receipt and terminal disagree: %v", err)
	}
	if lease.released != 1 {
		t.Fatalf("lease released %d times, want exactly 1", lease.released)
	}
	f, err := ReadFrame(buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if f.Type != TypeTerminal {
		t.Fatalf("frame = %v, want TERMINAL", f.Type)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes left on stat; more than one frame was sent", buf.Len())
	}
	// The durable facts must name the group that was ACTUALLY spawned. leaderPID comes from os/exec,
	// so this compares the published identity against a source outside the reaper — the check that a
	// caller-supplied group id would have failed.
	if got.CommandPGID != leaderPID || term.CommandPGID != leaderPID {
		t.Fatalf("receipt PGID %d / terminal PGID %d, want the spawned leader %d", got.CommandPGID, term.CommandPGID, leaderPID)
	}
}

// TestTerminalCertifiesOnlyTheGroupItToreDown is the regression net for the two-identity defect.
//
// Supervision used to carry its own PGID field alongside the reaper, and validation only required it
// to be positive. Teardown ran on the reaper's group while the terminal and the receipt both recorded
// the field, so a supervision could prove group A empty and durably publish that proof as a statement
// about an unrelated group B — and because both false facts copied B, AgreesWith could not see it.
//
// The fix is structural: there is nowhere left to put a second group id. This test pins the property
// that fix exists for. It runs a real command in group A while an unrelated group B stays ALIVE, and
// requires the published identity to be A and B to be untouched.
func TestTerminalCertifiesOnlyTheGroupItToreDown(t *testing.T) {
	bystander := startGroupLeader(t, "sleep 30")
	bystanderPGID := bystander.Process.Pid
	t.Cleanup(func() { _, _ = ReapGroup(bystanderPGID, fastPolicy) })

	s, _, root, _, leaderPID := superviseStarted(t, "exit 0", nil)
	term, err := RunToTerminal(s)
	if err != nil {
		t.Fatalf("RunToTerminal: %v", err)
	}
	got, err := ReadReceipt(root, "att-1")
	if err != nil {
		t.Fatalf("ReadReceipt: %v", err)
	}
	if term.CommandPGID != leaderPID || got.CommandPGID != leaderPID {
		t.Fatalf("published identity terminal=%d receipt=%d, want the group actually torn down (%d)", term.CommandPGID, got.CommandPGID, leaderPID)
	}
	// The proof is only worth something if the OTHER group could have been named and was not: an empty
	// bystander would let a wrong identity look indistinguishable from a right one.
	empty, err := groupIsEmpty(bystanderPGID)
	if err != nil {
		t.Fatalf("groupIsEmpty: %v", err)
	}
	if empty {
		t.Fatal("the bystander group died on its own; the test proves nothing about which group was certified")
	}
}

// TestTerminalSequenceClosesStreamsFirst: the coordinator's drain cannot see EOF while the supervisor
// still holds a write end, so the ordering would deadlock.
func TestTerminalSequenceClosesStreamsFirst(t *testing.T) {
	s, _, _, _, _ := superviseStarted(t, "echo hello; exit 0", nil)
	if _, err := RunToTerminal(s); err != nil {
		t.Fatalf("RunToTerminal: %v", err)
	}
	if err := s.Spawn.Stdout.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdout write copy was not closed by the sequence: %v", err)
	}
	if err := s.Spawn.Stderr.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stderr write copy was not closed by the sequence: %v", err)
	}
}

// TestTerminalSequenceTearsDownDespiteEarlierFailure. Returning early on a pre-teardown error would
// leave the command group ALIVE while the supervisor exits — lease released by process death, domain
// uncontained, worktree still mutable. Recovery blocking on an absent receipt does not clean that up,
// so teardown must be attempted regardless.
func TestTerminalSequenceTearsDownDespiteEarlierFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*Supervision)
	}{
		{"await fails", func(s *Supervision) {
			// Closing the reaper makes Await fail while leaving the group perfectly alive.
			if err := s.Reaper.Close(); err != nil {
				t.Fatalf("close reaper: %v", err)
			}
		}},
		{"stream close fails", func(s *Supervision) {
			// Closing the underlying descriptor out from under the *os.File makes Close return EBADF,
			// which is a genuine failure rather than the ErrClosed the sequence tolerates. Passing a
			// bogus fd to os.NewFile does not work: it returns nil, which CloseStreamCopies skips.
			if err := syscall.Close(int(s.Spawn.Stdout.Fd())); err != nil {
				t.Fatalf("close underlying fd: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, buf, root, lease, _ := superviseStarted(t, "sleep 30", nil)
			pgid := s.Reaper.PGID()
			tc.break_(&s)

			if _, err := RunToTerminal(s); err == nil {
				t.Fatal("RunToTerminal reported success despite a pre-teardown failure")
			}
			// The whole point: the group must be GONE even though the sequence failed.
			empty, eerr := groupIsEmpty(pgid)
			if eerr != nil {
				t.Fatalf("groupIsEmpty: %v", eerr)
			}
			if !empty {
				t.Fatal("the owned group survived a failed sequence; containment cleanup was skipped")
			}
			if _, rerr := ReadReceipt(root, "att-1"); !errors.Is(rerr, ErrReceiptMissing) {
				t.Fatalf("a receipt was published for a failed sequence: %v", rerr)
			}
			if buf.Len() != 0 {
				t.Fatal("a terminal was announced for a failed sequence")
			}
			if lease.released != 0 {
				t.Fatal("the lease was released for a failed sequence")
			}
		})
	}
}

// TestTerminalSequenceNoCommandOutcomes: both no-command paths run the same ordered authority.
func TestTerminalSequenceNoCommandOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		outcome   LaunchOutcome
		errClass  string
		wantShape terminalShape
	}{
		{"aborted before GO", LaunchAborted, "", shapeAbort},
		{"spawn failed after GO", LaunchSpawnFailed, "not_found", shapeSpawnFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, buf, root, lease := baseSupervision(t, tc.outcome)
			s.SpawnErrorClass = tc.errClass

			term, err := RunToTerminal(s)
			if err != nil {
				t.Fatalf("RunToTerminal: %v", err)
			}
			if term.shape() != tc.wantShape {
				t.Fatalf("shape = %v, want %v", term.shape(), tc.wantShape)
			}
			if term.CommandStarted || term.CommandPGID != 0 || term.Reaped != 0 {
				t.Fatalf("a no-command terminal carries run facts: %+v", term)
			}
			got, rerr := ReadReceipt(root, "att-1")
			if rerr != nil {
				t.Fatalf("the receipt must be published for a no-command outcome too: %v", rerr)
			}
			if !got.GroupEmpty {
				t.Fatal("a no-command receipt must state the group is empty; nothing was ever started")
			}
			if err := got.AgreesWith(term); err != nil {
				t.Fatalf("receipt and terminal disagree: %v", err)
			}
			if lease.released != 1 {
				t.Fatalf("lease released %d times, want exactly 1", lease.released)
			}
			if f, ferr := ReadFrame(buf); ferr != nil || f.Type != TypeTerminal {
				t.Fatalf("frame = %v (err %v), want TERMINAL", f.Type, ferr)
			}
		})
	}
}

// TestTerminalSequenceKeepsTheLeaseWhenTheTerminalCannotBeSent: the lease goes last, so a failure to
// announce must not leave it free while this supervisor is unfinished.
func TestTerminalSequenceKeepsTheLeaseWhenTheTerminalCannotBeSent(t *testing.T) {
	s, _, root, lease := baseSupervision(t, LaunchAborted)
	s.Stat.(*orderingStat).err = errors.New("stat pipe gone")

	if _, err := RunToTerminal(s); !errors.Is(err, ErrTerminalSequence) {
		t.Fatalf("err = %v, want ErrTerminalSequence", err)
	}
	if lease.released != 0 {
		t.Fatal("the lease was released even though the terminal was never announced")
	}
	// The receipt is already durable at that point, which is correct: it precedes the announcement.
	if _, err := ReadReceipt(root, "att-1"); err != nil {
		t.Fatalf("the receipt should already be durable: %v", err)
	}
}

// TestTerminalSequenceReportsALeaseReleaseFailure: releasing last does not mean failing quietly.
func TestTerminalSequenceReportsALeaseReleaseFailure(t *testing.T) {
	s, _, root, lease := baseSupervision(t, LaunchAborted)
	lease.err = errors.New("lease stuck")

	term, err := RunToTerminal(s)
	if !errors.Is(err, ErrTerminalSequence) {
		t.Fatalf("err = %v, want ErrTerminalSequence", err)
	}
	// The terminal is still returned: it WAS announced and the receipt IS durable. Only the release
	// failed, and a caller that discarded the terminal would lose facts that really happened.
	if term.shape() != shapeAbort {
		t.Fatalf("the terminal that was actually sent should still be returned, got %+v", term)
	}
	if _, rerr := ReadReceipt(root, "att-1"); rerr != nil {
		t.Fatalf("the receipt should be durable: %v", rerr)
	}
}

func TestTerminalSequenceValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutet func(*Supervision)
	}{
		{"no outcome", func(s *Supervision) { s.Outcome = 0 }},
		{"no attempt id", func(s *Supervision) { s.AttemptID = "" }},
		{"no root", func(s *Supervision) { s.Root = nil }},
		{"no stat", func(s *Supervision) { s.Stat = nil }},
		{"no protocol", func(s *Supervision) { s.Protocol = nil }},
		{"no clock", func(s *Supervision) { s.Now = nil }},
		{"no lease", func(s *Supervision) { s.Lease = nil }},
		{"abort carrying a spawn error", func(s *Supervision) { s.SpawnErrorClass = "x" }},
		{"spawn failure without a class", func(s *Supervision) {
			s.Outcome = LaunchSpawnFailed
			s.SpawnErrorClass = ""
		}},
		// The ONE malformation with no live-group counterpart, and the reason is structural rather
		// than an omission: the reaper is what owns a group, so a supervision without one has no group
		// to abandon. Every other case in this list is also exercised against a real running command
		// in TestTerminalSequenceTearsDownOnValidationFailure.
		{"started without a reaper", func(s *Supervision) { s.Outcome = LaunchStarted }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _ := baseSupervision(t, LaunchAborted)
			tc.mutet(&s)
			_, err := RunToTerminal(s)
			if err == nil {
				t.Fatal("RunToTerminal accepted an inconsistent supervision")
			}
			if !strings.Contains(err.Error(), "proctree:") {
				t.Fatalf("unexpected error shape: %v", err)
			}
		})
	}
}

// TestTerminalSequencePublishesNothingWhenTeardownFails. A teardown that cannot prove the group empty
// must leave NO receipt and send NO terminal.
func TestTerminalSequencePublishesNothingWhenTeardownFails(t *testing.T) {
	// A plain `sleep 30` is NOT a hostile fixture: sleep dies on SIGTERM, the group drains inside the
	// deadline, and the test passes while proving nothing. An ignoring shell that regenerates work
	// survives SIGTERM and still cannot ignore SIGKILL.
	const survivesTerm = "trap '' TERM; while :; do sleep 0.05; done"
	s, buf, root, lease, _ := superviseStarted(t, survivesTerm, func(s *Supervision) {
		s.Policy = ReapPolicy{Grace: time.Hour, Deadline: 200 * time.Millisecond, Poll: 5 * time.Millisecond}
		s.AwaitDeadline = time.Now().Add(150 * time.Millisecond)
	})
	t.Cleanup(func() { _, _ = ReapGroup(s.Reaper.PGID(), fastPolicy) })

	// Prove the fixture is hostile before relying on it.
	time.Sleep(150 * time.Millisecond)
	if err := signalGroup(s.Reaper.PGID(), unix.SIGTERM); err != nil {
		t.Fatalf("probe signal: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if empty, eerr := groupIsEmpty(s.Reaper.PGID()); eerr != nil || empty {
		t.Fatalf("fixture is not SIGTERM-immune (empty=%v err=%v); the test would prove nothing", empty, eerr)
	}

	_, err := RunToTerminal(s)
	if !errors.Is(err, ErrReapDeadline) {
		t.Fatalf("err = %v, want the reap deadline preserved as the cause", err)
	}
	if _, rerr := ReadReceipt(root, "att-1"); !errors.Is(rerr, ErrReceiptMissing) {
		t.Fatalf("a receipt was published for a teardown that never proved the group empty: %v", rerr)
	}
	if buf.Len() != 0 {
		t.Fatal("a terminal was announced for an unfinished cleanup")
	}
	if lease.released != 0 {
		t.Fatal("the lease was released before cleanup completed")
	}
}

// TestTerminalSequenceTearsDownOnValidationFailure is CX's adversarial case, widened to EVERY
// malformation that can coexist with a running command.
//
// It is the third appearance of one asymmetry: close and await failures were made non-short-circuiting
// so a live group could not be abandoned, and the VALIDATION gate still returned early. The first fix
// covered only the four reporting fields it happened to look at, which left the OWNERSHIP
// discriminators — the outcome and the old duplicate group id — still able to disable cleanup: a real
// started supervision whose Outcome had been set to a no-command value walked away from a live group
// its reaper could have torn down.
//
// So the table now includes every discriminator, including the ones that gate cleanup. The property is
// that no field on this struct can switch containment cleanup off, because the capability comes from
// the reaper's existence and every other field only describes what will be reported.
func TestTerminalSequenceTearsDownOnValidationFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutet func(*Supervision)
	}{
		// Ownership discriminators: these decide whether the code BELIEVES a group is running, which is
		// exactly why they must not decide whether it is cleaned up.
		{"no outcome at all", func(s *Supervision) { s.Outcome = 0 }},
		{"outcome claims the launch aborted", func(s *Supervision) { s.Outcome = LaunchAborted }},
		{"outcome claims the spawn failed", func(s *Supervision) {
			s.Outcome = LaunchSpawnFailed
			s.SpawnErrorClass = "not_found"
		}},
		{"started carrying a spawn error class", func(s *Supervision) { s.SpawnErrorClass = "not_found" }},
		// Reporting fields.
		{"no attempt id", func(s *Supervision) { s.AttemptID = "" }},
		{"no attempt directory", func(s *Supervision) { s.Root = nil }},
		{"no status channel", func(s *Supervision) { s.Stat = nil }},
		{"no protocol", func(s *Supervision) { s.Protocol = nil }},
		{"no lease", func(s *Supervision) { s.Lease = nil }},
		{"no clock", func(s *Supervision) { s.Now = nil }},
		// Timing fields. An invalid policy must not become a second reason to skip the cleanup, which
		// is why the emergency path substitutes the shipped default rather than refusing.
		{"invalid reap policy", func(s *Supervision) { s.Policy = ReapPolicy{} }},
		// Disclosed rather than overclaimed: this row is NOT independently mutation-detectable.
		// Reaper.Await refuses a nonpositive poll as well, so deleting the check in validate moves the
		// refusal from the emergency path to the accumulated-error path — where teardown also runs and
		// also publishes nothing. The observable outcome is identical, so the row exercises the
		// property without being able to prove which layer enforced it.
		{"await poll not positive", func(s *Supervision) { s.AwaitPoll = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, buf, root, lease, leaderPID := superviseStarted(t, "sleep 30", nil)
			t.Cleanup(func() { _, _ = ReapGroup(leaderPID, fastPolicy) })
			tc.mutet(&s)

			if _, err := RunToTerminal(s); err == nil {
				t.Fatal("RunToTerminal accepted a malformed supervision")
			}
			empty, eerr := groupIsEmpty(leaderPID)
			if eerr != nil {
				t.Fatalf("groupIsEmpty: %v", eerr)
			}
			if !empty {
				t.Fatal("the started command group survived a validation failure; the supervisor would exit leaving it alive")
			}
			// Cleaning up is not the same as succeeding: a malformed supervision must still publish
			// nothing, announce nothing and keep the lease.
			if _, rerr := ReadReceipt(root, "att-1"); !errors.Is(rerr, ErrReceiptMissing) {
				t.Fatalf("a receipt was published for a malformed supervision: %v", rerr)
			}
			if buf.Len() != 0 {
				t.Fatal("a terminal was announced for a malformed supervision")
			}
			if lease.released != 0 {
				t.Fatal("the lease was released for a malformed supervision")
			}
		})
	}
}
