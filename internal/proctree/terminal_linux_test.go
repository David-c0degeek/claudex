package proctree

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type recordingLease struct{ released int }

func (l *recordingLease) Release() error { l.released++; return nil }

type failingLease struct{}

func (failingLease) Release() error { return errors.New("lease stuck") }

// supervise wires a real contained command to a real reaper, real receipt publication and the real
// protocol, so the ordering is exercised end to end rather than against stubs.
func supervise(t *testing.T, script string, tweak func(*Supervision)) (Supervision, *bytes.Buffer, *os.Root) {
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
	pgid := cmd.Process.Pid
	r, err := NewReaper(pgid)
	if err != nil {
		t.Fatalf("NewReaper: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { root.Close() })

	stat := &bytes.Buffer{}
	proto := &SupervisorProtocol{}
	if err := proto.SendReady(stat); err != nil {
		t.Fatalf("SendReady: %v", err)
	}
	stat.Reset()
	if _, err := proto.Recv(Frame{Type: TypeGo, Payload: proto.challenge}); err != nil {
		t.Fatalf("Recv GO: %v", err)
	}

	s := Supervision{
		AttemptID:     "att-1",
		Spawn:         sp,
		Reaper:        r,
		PGID:          pgid,
		Root:          root,
		Stat:          stat,
		Protocol:      proto,
		Policy:        fastPolicy,
		AwaitDeadline: time.Now().Add(10 * time.Second),
		AwaitPoll:     20 * time.Millisecond,
		Now:           func() time.Time { return time.Unix(1753500000, 0) },
	}
	if tweak != nil {
		tweak(&s)
	}
	return s, stat, root
}

// TestTerminalSequenceHappyPath: the receipt is durable BEFORE the terminal is announced, and the
// lease is released last.
func TestTerminalSequenceHappyPath(t *testing.T) {
	lease := &recordingLease{}
	s, stat, root := supervise(t, "exit 3", func(s *Supervision) { s.Lease = lease })

	term, err := RunToTerminal(s)
	if err != nil {
		t.Fatalf("RunToTerminal: %v", err)
	}
	if !term.Exited || term.ExitCode != 3 {
		t.Fatalf("terminal = %+v, want a normal exit 3", term)
	}
	got, err := ReadReceipt(root, "att-1")
	if err != nil {
		t.Fatalf("the receipt must be durable before the terminal is announced: %v", err)
	}
	if err := got.AgreesWith(term); err != nil {
		t.Fatalf("receipt and terminal disagree: %v", err)
	}
	if lease.released != 1 {
		t.Fatalf("lease released %d times, want exactly 1", lease.released)
	}
	f, err := ReadFrame(stat)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if f.Type != TypeTerminal {
		t.Fatalf("frame = %v, want TERMINAL", f.Type)
	}
	if stat.Len() != 0 {
		t.Fatalf("%d bytes left on stat; more than one frame was sent", stat.Len())
	}
}

// TestTerminalSequenceClosesStreamsFirst is the reason that is step one: the coordinator's drain
// cannot see EOF while the supervisor still holds a write end, so the ordering would deadlock.
func TestTerminalSequenceClosesStreamsFirst(t *testing.T) {
	s, _, _ := supervise(t, "echo hello; exit 0", nil)
	if _, err := RunToTerminal(s); err != nil {
		t.Fatalf("RunToTerminal: %v", err)
	}
	// Closing an already-closed handle is the observable proof the sequence dropped them.
	if err := s.Spawn.Stdout.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdout write copy was not closed by the sequence: %v", err)
	}
	if err := s.Spawn.Stderr.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stderr write copy was not closed by the sequence: %v", err)
	}
}

// TestTerminalSequencePublishesNothingWhenTeardownFails. A teardown that cannot prove the group empty
// must leave NO receipt and send NO terminal, so recovery finds the proof absent and blocks rather
// than trusting an unfinished cleanup.
func TestTerminalSequencePublishesNothingWhenTeardownFails(t *testing.T) {
	lease := &recordingLease{}
	// A plain `sleep 30` is NOT a hostile fixture: sleep dies on SIGTERM, the group drains inside the
	// deadline, and the test passes while proving nothing — the same trap that cost three wrong
	// guesses in the reap slice. An ignoring shell that keeps regenerating work survives SIGTERM and
	// still cannot ignore SIGKILL.
	const survivesTerm = "trap '' TERM; while :; do sleep 0.05; done"
	s, stat, root := supervise(t, survivesTerm, func(s *Supervision) {
		s.Lease = lease
		// Grace beyond the deadline: SIGKILL never arrives, so the group cannot drain in time.
		s.Policy = ReapPolicy{Grace: time.Hour, Deadline: 200 * time.Millisecond, Poll: 5 * time.Millisecond}
		s.AwaitDeadline = time.Now().Add(150 * time.Millisecond)
	})
	t.Cleanup(func() { _, _ = ReapGroup(s.PGID, fastPolicy) })

	// Prove the fixture is hostile before relying on it: give the shell time to install its trap,
	// signal the group, and require it to still be populated. Without this the test could go green
	// because the group died on its own.
	time.Sleep(150 * time.Millisecond)
	if err := signalGroup(s.PGID, unix.SIGTERM); err != nil {
		t.Fatalf("probe signal: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if empty, eerr := groupIsEmpty(s.PGID); eerr != nil || empty {
		t.Fatalf("fixture is not SIGTERM-immune (empty=%v err=%v); the test would prove nothing", empty, eerr)
	}

	_, err := RunToTerminal(s)
	if !errors.Is(err, ErrTerminalSequence) {
		t.Fatalf("err = %v, want ErrTerminalSequence", err)
	}
	if !errors.Is(err, ErrReapDeadline) {
		t.Fatalf("err = %v, want the reap deadline preserved as the cause", err)
	}
	if _, rerr := ReadReceipt(root, "att-1"); !errors.Is(rerr, ErrReceiptMissing) {
		t.Fatalf("a receipt was published for a teardown that never proved the group empty: %v", rerr)
	}
	if stat.Len() != 0 {
		t.Fatal("a terminal was announced for an unfinished cleanup")
	}
	if lease.released != 0 {
		t.Fatal("the lease was released before cleanup completed; a recovering coordinator would see it free")
	}
}

// TestTerminalSequenceKeepsTheLeaseWhenTheTerminalCannotBeSent: the lease goes last, so a failure to
// announce must not leave it free while this supervisor is unfinished.
func TestTerminalSequenceKeepsTheLeaseWhenTheTerminalCannotBeSent(t *testing.T) {
	lease := &recordingLease{}
	s, _, _ := supervise(t, "exit 0", func(s *Supervision) {
		s.Lease = lease
		s.Stat = failWriter{err: errors.New("stat pipe gone")}
	})
	if _, err := RunToTerminal(s); !errors.Is(err, ErrTerminalSequence) {
		t.Fatalf("err = %v, want ErrTerminalSequence", err)
	}
	if lease.released != 0 {
		t.Fatal("the lease was released even though the terminal was never announced")
	}
}

// TestTerminalSequenceReportsALeaseReleaseFailure: releasing last does not mean failing quietly.
func TestTerminalSequenceReportsALeaseReleaseFailure(t *testing.T) {
	s, _, root := supervise(t, "exit 0", func(s *Supervision) { s.Lease = failingLease{} })
	term, err := RunToTerminal(s)
	if !errors.Is(err, ErrTerminalSequence) {
		t.Fatalf("err = %v, want ErrTerminalSequence", err)
	}
	// The terminal is still returned: it WAS announced and the receipt IS durable. Only the release
	// failed, and a caller that discarded the terminal would lose facts that really happened.
	if !term.CommandStarted {
		t.Fatal("the terminal that was actually sent should still be returned")
	}
	if _, rerr := ReadReceipt(root, "att-1"); rerr != nil {
		t.Fatalf("the receipt should be durable: %v", rerr)
	}
}

func TestTerminalSequenceValidation(t *testing.T) {
	base, _, _ := supervise(t, "exit 0", nil)
	t.Cleanup(func() { _, _ = ReapGroup(base.PGID, fastPolicy) })
	for _, tc := range []struct {
		name  string
		mutet func(*Supervision)
	}{
		{"no attempt id", func(s *Supervision) { s.AttemptID = "" }},
		{"no reaper", func(s *Supervision) { s.Reaper = nil }},
		{"bad pgid", func(s *Supervision) { s.PGID = 0 }},
		{"no root", func(s *Supervision) { s.Root = nil }},
		{"no stat", func(s *Supervision) { s.Stat = nil }},
		{"no protocol", func(s *Supervision) { s.Protocol = nil }},
		{"no clock", func(s *Supervision) { s.Now = nil }},
		{"bad await poll", func(s *Supervision) { s.AwaitPoll = 0 }},
		{"bad policy", func(s *Supervision) { s.Policy = ReapPolicy{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mutet(&s)
			_, err := RunToTerminal(s)
			if err == nil {
				t.Fatal("RunToTerminal accepted an incomplete supervision")
			}
			if !strings.Contains(err.Error(), "proctree:") {
				t.Fatalf("unexpected error shape: %v", err)
			}
		})
	}
}
