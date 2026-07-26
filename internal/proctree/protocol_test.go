package proctree

import (
	"bytes"
	"errors"
	"testing"

	"github.com/David-c0degeek/claudex/internal/canonjson"
)

type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

func mustRead(t *testing.T, b *bytes.Buffer) Frame {
	t.Helper()
	f, err := ReadFrame(b)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	return f
}

// handshake runs READY -> GO and returns the two halves plus their buffers, so each test starts from
// the shipped sequence rather than from hand-set states.
func handshake(t *testing.T) (*SupervisorProtocol, *CoordinatorProtocol, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var ctrl, stat bytes.Buffer
	sup := &SupervisorProtocol{}
	coord := &CoordinatorProtocol{}

	if err := sup.SendReady(&stat); err != nil {
		t.Fatalf("SendReady: %v", err)
	}
	if err := coord.RecvReady(mustRead(t, &stat)); err != nil {
		t.Fatalf("RecvReady: %v", err)
	}
	if err := coord.SendGo(&ctrl); err != nil {
		t.Fatalf("SendGo: %v", err)
	}
	act, err := sup.Recv(mustRead(t, &ctrl))
	if err != nil {
		t.Fatalf("Recv GO: %v", err)
	}
	if act != ActionStart {
		t.Fatalf("action = %v, want ActionStart", act)
	}
	return sup, coord, &ctrl, &stat
}

func TestHandshakeAndTerminal(t *testing.T) {
	sup, coord, _, stat := handshake(t)

	want := Terminal{CommandStarted: true, Exited: true, ExitCode: 127, CommandPGID: 4242, Reaped: 3}
	if err := sup.SendTerminal(stat, want); err != nil {
		t.Fatalf("SendTerminal: %v", err)
	}
	got, err := coord.RecvTerminal(mustRead(t, stat))
	if err != nil {
		t.Fatalf("RecvTerminal: %v", err)
	}
	if got != want {
		t.Fatalf("terminal = %+v, want %+v", got, want)
	}
	// The point of carrying exit facts in a frame: an exit 127 is a statement about the command, and
	// is distinguishable from a supervisor that simply died (EOF on stat with no TERMINAL).
	if !got.CommandStarted || got.ExitCode != 127 {
		t.Fatalf("exit 127 must survive as a command fact, got %+v", got)
	}
}

// TestDuplicateGoIsNotASecondLaunch is the reason the protocol validates state and not just type: a
// well-formed duplicate GO is exactly what a type-only check would wave through.
func TestDuplicateGoIsNotASecondLaunch(t *testing.T) {
	sup, coord, ctrl, _ := handshake(t)
	// Re-send the same well-formed GO by replaying the coordinator's challenge.
	if err := WriteFrame(ctrl, Frame{Type: TypeGo, Payload: coord.challenge}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if _, err := sup.Recv(mustRead(t, ctrl)); !errors.Is(err, ErrOutOfState) {
		t.Fatalf("second GO: err = %v, want ErrOutOfState", err)
	}
}

func TestCancelBeforeGoAborts(t *testing.T) {
	var ctrl, stat bytes.Buffer
	sup := &SupervisorProtocol{}
	coord := &CoordinatorProtocol{}
	if err := sup.SendReady(&stat); err != nil {
		t.Fatalf("SendReady: %v", err)
	}
	if err := coord.RecvReady(mustRead(t, &stat)); err != nil {
		t.Fatalf("RecvReady: %v", err)
	}
	if err := coord.SendCancel(&ctrl); err != nil {
		t.Fatalf("SendCancel: %v", err)
	}
	act, err := sup.Recv(mustRead(t, &ctrl))
	if err != nil {
		t.Fatalf("Recv CANCEL: %v", err)
	}
	if act != ActionAbort {
		t.Fatalf("action = %v, want ActionAbort", act)
	}
	// An aborted supervisor still owes a terminal, and it must say no command started.
	if err := sup.SendTerminal(&stat, Terminal{CommandStarted: false}); err != nil {
		t.Fatalf("SendTerminal after abort: %v", err)
	}
}

func TestCancelAfterGoIsIdempotent(t *testing.T) {
	sup, coord, ctrl, _ := handshake(t)
	for i := 0; i < 3; i++ {
		if err := coord.SendCancel(ctrl); err != nil {
			t.Fatalf("SendCancel %d: %v", i, err)
		}
		act, err := sup.Recv(mustRead(t, ctrl))
		if err != nil {
			t.Fatalf("Recv CANCEL %d: %v", i, err)
		}
		if act != ActionStop {
			t.Fatalf("action %d = %v, want ActionStop", i, act)
		}
	}
}

func TestDuplicateTerminalIsRefused(t *testing.T) {
	sup, coord, _, stat := handshake(t)
	term := Terminal{CommandStarted: true, Exited: true, CommandPGID: 7}
	if err := sup.SendTerminal(stat, term); err != nil {
		t.Fatalf("SendTerminal: %v", err)
	}
	if _, err := coord.RecvTerminal(mustRead(t, stat)); err != nil {
		t.Fatalf("RecvTerminal: %v", err)
	}
	// The supervisor will not send a second one...
	if err := sup.SendTerminal(stat, term); !errors.Is(err, ErrOutOfState) {
		t.Fatalf("second SendTerminal: err = %v, want ErrOutOfState", err)
	}
	// ...and the coordinator refuses one regardless of who produced it, because it cannot decide
	// which of two terminals describes the run.
	payload, err := term.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := WriteFrame(stat, Frame{Type: TypeTerminal, Payload: payload}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if _, err := coord.RecvTerminal(mustRead(t, stat)); !errors.Is(err, ErrDuplicateTerminal) {
		t.Fatalf("err = %v, want ErrDuplicateTerminal", err)
	}
}

func TestChallengeMismatchIsRefused(t *testing.T) {
	var ctrl, stat bytes.Buffer
	sup := &SupervisorProtocol{}
	if err := sup.SendReady(&stat); err != nil {
		t.Fatalf("SendReady: %v", err)
	}
	mustRead(t, &stat)
	if err := WriteFrame(&ctrl, Frame{Type: TypeGo, Payload: bytes.Repeat([]byte{0}, ChallengeBytes)}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if _, err := sup.Recv(mustRead(t, &ctrl)); !errors.Is(err, ErrChallengeMismatch) {
		t.Fatalf("err = %v, want ErrChallengeMismatch", err)
	}
}

// TestFailedGoWriteDoesNotAdvanceState pins the reason SendGo returns before setting coordRunning: a
// coordinator that saw a write error does not know what the supervisor received, so it must not have
// recorded that a command started. Moving the state assignment above the error return makes this fail.
func TestFailedGoWriteDoesNotAdvanceState(t *testing.T) {
	var ctrl, stat bytes.Buffer
	sup := &SupervisorProtocol{}
	coord := &CoordinatorProtocol{}
	if err := sup.SendReady(&stat); err != nil {
		t.Fatalf("SendReady: %v", err)
	}
	if err := coord.RecvReady(mustRead(t, &stat)); err != nil {
		t.Fatalf("RecvReady: %v", err)
	}
	sentinel := errors.New("pipe is gone")
	if err := coord.SendGo(failWriter{err: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("SendGo: err = %v, want the write error", err)
	}
	if err := coord.SendGo(&ctrl); err != nil {
		t.Fatalf("state advanced despite a failed write: %v", err)
	}
}

func TestOutOfStateFrames(t *testing.T) {
	var buf bytes.Buffer

	// GO before READY: the supervisor has generated no challenge, so nothing can authorise a launch.
	sup := &SupervisorProtocol{}
	if err := WriteFrame(&buf, Frame{Type: TypeGo, Payload: make([]byte, ChallengeBytes)}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if _, err := sup.Recv(mustRead(t, &buf)); !errors.Is(err, ErrOutOfState) {
		t.Fatalf("GO before READY: err = %v, want ErrOutOfState", err)
	}

	// A stat-direction frame arriving on ctrl.
	sup2 := &SupervisorProtocol{}
	if err := sup2.SendReady(&buf); err != nil {
		t.Fatalf("SendReady: %v", err)
	}
	mustRead(t, &buf)
	if err := WriteFrame(&buf, Frame{Type: TypeTerminal, Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if _, err := sup2.Recv(mustRead(t, &buf)); !errors.Is(err, ErrOutOfState) {
		t.Fatalf("TERMINAL on ctrl: err = %v, want ErrOutOfState", err)
	}

	// GO before READY on the coordinator half.
	coord := &CoordinatorProtocol{}
	if err := coord.SendGo(&buf); !errors.Is(err, ErrOutOfState) {
		t.Fatalf("SendGo before READY: err = %v, want ErrOutOfState", err)
	}
}

// TestTerminalContradictionsRefused checks both directions. The decode side is the one that matters:
// a coordinator must not accept a self-contradictory terminal merely because it is well-formed JSON.
func TestTerminalContradictionsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    wireTerminal
	}{
		{"no command but a PGID", wireTerminal{SchemaVersion: 1, CommandStarted: false, CommandPGID: 99}},
		{"no command but exit facts", wireTerminal{SchemaVersion: 1, CommandStarted: false, Exited: true}},
		{"started and a spawn error", wireTerminal{SchemaVersion: 1, CommandStarted: true, SpawnErrorClass: "not_found"}},
		{"exited and signalled", wireTerminal{SchemaVersion: 1, CommandStarted: true, Exited: true, Signal: 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := canonjson.CanonicalizeValue(tc.w)
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if _, err := decodeTerminal(raw); err == nil {
				t.Fatal("decode accepted a self-contradictory terminal")
			}
			out := Terminal{
				CommandStarted:  tc.w.CommandStarted,
				Exited:          tc.w.Exited,
				Signal:          tc.w.Signal,
				SpawnErrorClass: tc.w.SpawnErrorClass,
				CommandPGID:     tc.w.CommandPGID,
			}
			if _, err := out.encode(); err == nil {
				t.Fatal("encode accepted a self-contradictory terminal")
			}
		})
	}
}

func TestTerminalSchemaVersionEnforced(t *testing.T) {
	raw, err := canonjson.CanonicalizeValue(wireTerminal{SchemaVersion: 99, CommandStarted: false})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if _, err := decodeTerminal(raw); err == nil {
		t.Fatal("decode accepted an unknown schema version")
	}
}

// TestTerminalAlwaysFitsAFrame keeps the PIPE_BUF guarantee true by construction rather than by
// hoping the fields stay small: the only variable-length field is bounded, so a maximal terminal is
// still one atomic write.
func TestTerminalAlwaysFitsAFrame(t *testing.T) {
	maximal := Terminal{
		CommandStarted:  false,
		SpawnErrorClass: string(bytes.Repeat([]byte("x"), MaxSpawnErrorClassBytes)),
	}
	payload, err := maximal.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	buf, err := EncodeFrame(Frame{Type: TypeTerminal, Payload: payload})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if len(buf) > MaxFrameBytes {
		t.Fatalf("maximal terminal frame is %d bytes, exceeds %d", len(buf), MaxFrameBytes)
	}

	oversize := Terminal{SpawnErrorClass: string(bytes.Repeat([]byte("x"), MaxSpawnErrorClassBytes+1))}
	if _, err := oversize.encode(); err == nil {
		t.Fatal("encode accepted an over-length spawn error class")
	}
}
