package proctree

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"

	"github.com/David-c0degeek/claudex/internal/canonjson"
)

// ChallengeBytes is the length of the supervisor-generated challenge carried in READY and echoed in
// GO.
//
// What the challenge does and does not prove is worth stating, because an earlier draft of the design
// overclaimed it. The CAPABILITY is exclusive endpoint inheritance: only the coordinator holds the
// ctrl write end, so only the coordinator can send anything at all. The challenge adds one thing on
// top of that — proof that the GO came from a party that read THIS instance's READY — which catches a
// coordinator bug that reused or misrouted a write end across attempts. It adds no authority against
// a party that already holds the write end.
const ChallengeBytes = 32

var (
	// ErrOutOfState means a well-formed frame arrived where the protocol does not admit it. It is
	// fatal to the receiving side: typed frames without state validation would let a duplicate GO be
	// read as a second launch.
	ErrOutOfState = errors.New("proctree: frame out of protocol state")

	// ErrChallengeMismatch means GO did not echo the challenge this supervisor generated.
	ErrChallengeMismatch = errors.New("proctree: GO challenge does not match READY")

	// ErrDuplicateTerminal means a second TERMINAL arrived. The coordinator cannot decide which one
	// describes the run, so the attempt becomes recovery-required rather than resolved by preference.
	ErrDuplicateTerminal = errors.New("proctree: duplicate TERMINAL")
)

// Action is what the supervisor must do in response to a received ctrl frame.
type Action int

const (
	// ActionStart: a valid GO. Spawn the command.
	ActionStart Action = iota + 1
	// ActionAbort: CANCEL before GO. Nothing has started, so cleanup publishes a no-command receipt.
	ActionAbort
	// ActionStop: CANCEL after GO. Stop the owned group. Idempotent: repeats change nothing.
	ActionStop
)

// SupervisorProtocol is the supervisor's half: it reads ctrl and writes stat.
type SupervisorProtocol struct {
	challenge    []byte
	state        supState
	terminalSent bool
}

type supState int

const (
	supInit supState = iota
	supArmed
	supRunning
	supDone
)

func (s supState) String() string {
	switch s {
	case supInit:
		return "init"
	case supArmed:
		return "armed"
	case supRunning:
		return "running"
	case supDone:
		return "done"
	}
	return "unknown"
}

// SendReady generates the challenge and writes READY. It is valid exactly once.
func (p *SupervisorProtocol) SendReady(w io.Writer) error {
	if p.state != supInit {
		return fmt.Errorf("%w: READY already sent", ErrOutOfState)
	}
	c := make([]byte, ChallengeBytes)
	if _, err := rand.Read(c); err != nil {
		return fmt.Errorf("proctree: generate challenge: %w", err)
	}
	if err := WriteFrame(w, Frame{Type: TypeReady, Payload: c}); err != nil {
		return err
	}
	p.challenge = c
	p.state = supArmed
	return nil
}

// Recv classifies one received ctrl frame.
func (p *SupervisorProtocol) Recv(f Frame) (Action, error) {
	switch f.Type {
	case TypeGo:
		if p.state != supArmed {
			// Covers the duplicate GO explicitly: in supRunning a second GO would otherwise be a
			// second launch, which is the exact misreading state validation exists to prevent.
			return 0, fmt.Errorf("%w: GO in state %v", ErrOutOfState, p.state)
		}
		if subtle.ConstantTimeCompare(f.Payload, p.challenge) != 1 {
			return 0, ErrChallengeMismatch
		}
		p.state = supRunning
		return ActionStart, nil
	case TypeCancel:
		switch p.state {
		case supArmed:
			p.state = supDone
			return ActionAbort, nil
		case supRunning:
			// Deliberately not a state change: cancel after GO is idempotent, so a coordinator that
			// re-sends on a retry loop does not trip the protocol.
			return ActionStop, nil
		default:
			return 0, fmt.Errorf("%w: CANCEL in state %v", ErrOutOfState, p.state)
		}
	default:
		return 0, fmt.Errorf("%w: %s is not a ctrl frame", ErrOutOfState, f.Type)
	}
}

// SendTerminal writes the one TERMINAL frame. Valid once, and only after the supervisor has reached a
// state where a terminal statement is meaningful.
func (p *SupervisorProtocol) SendTerminal(w io.Writer, t Terminal) error {
	if p.state == supInit {
		return fmt.Errorf("%w: TERMINAL before READY", ErrOutOfState)
	}
	if p.terminalSent {
		return fmt.Errorf("%w: TERMINAL already sent", ErrOutOfState)
	}
	payload, err := t.encode()
	if err != nil {
		return err
	}
	if err := WriteFrame(w, Frame{Type: TypeTerminal, Payload: payload}); err != nil {
		return err
	}
	p.terminalSent = true
	p.state = supDone
	return nil
}

// CoordinatorProtocol is the coordinator's half: it writes ctrl and reads stat.
type CoordinatorProtocol struct {
	challenge []byte
	state     coordState
}

type coordState int

const (
	coordInit coordState = iota
	coordArmed
	coordRunning
	// coordAborted is reached by a CANCEL sent BEFORE GO. It is a distinct state rather than a flag
	// because the supervisor's matching state is terminal: it has already classified that CANCEL as an
	// abort and moved to done, so a later GO would be refused and a repeated CANCEL would be fatal. A
	// coordinator that stayed "armed" could still launch a command it had just aborted.
	coordAborted
	coordDone
)

func (s coordState) String() string {
	switch s {
	case coordInit:
		return "init"
	case coordArmed:
		return "armed"
	case coordRunning:
		return "running"
	case coordAborted:
		return "aborted"
	case coordDone:
		return "done"
	}
	return "unknown"
}

// RecvReady consumes the READY frame and retains the challenge for GO.
func (p *CoordinatorProtocol) RecvReady(f Frame) error {
	if p.state != coordInit {
		return fmt.Errorf("%w: READY in state %v", ErrOutOfState, p.state)
	}
	if f.Type != TypeReady {
		return fmt.Errorf("%w: expected READY, got %s", ErrOutOfState, f.Type)
	}
	if len(f.Payload) != ChallengeBytes {
		return fmt.Errorf("%w: challenge is %d bytes, want %d", ErrOutOfState, len(f.Payload), ChallengeBytes)
	}
	p.challenge = append([]byte(nil), f.Payload...)
	p.state = coordArmed
	return nil
}

// SendGo echoes the challenge. Valid exactly once, and only after READY.
func (p *CoordinatorProtocol) SendGo(w io.Writer) error {
	if p.state != coordArmed {
		return fmt.Errorf("%w: GO in state %v", ErrOutOfState, p.state)
	}
	if err := WriteFrame(w, Frame{Type: TypeGo, Payload: p.challenge}); err != nil {
		// State is deliberately NOT advanced. A failed GO write means the coordinator does not know
		// what the supervisor saw, and the PIPE_BUF bound is what lets the caller conclude no
		// command started; advancing here would assert the opposite.
		return err
	}
	p.state = coordRunning
	return nil
}

// SendCancel stops the attempt. Its effect differs either side of GO, mirroring the supervisor:
//
//   - BEFORE GO it is a no-command abort and is terminal for the launch. The supervisor moves to done
//     on that frame, so no GO may follow and a repeat must NOT be emitted — the peer classifies a
//     second CANCEL as fatal.
//   - AFTER GO it is a stop request and is idempotent, so a coordinator retry loop is safe.
func (p *CoordinatorProtocol) SendCancel(w io.Writer) error {
	switch p.state {
	case coordArmed:
		if err := WriteFrame(w, Frame{Type: TypeCancel, Payload: nil}); err != nil {
			return err
		}
		p.state = coordAborted
		return nil
	case coordRunning:
		return WriteFrame(w, Frame{Type: TypeCancel, Payload: nil})
	case coordAborted:
		// Idempotent by doing nothing: the abort is already in flight and the peer is done.
		return nil
	default:
		return fmt.Errorf("%w: CANCEL in state %v", ErrOutOfState, p.state)
	}
}

// RecvTerminal consumes the one TERMINAL frame.
func (p *CoordinatorProtocol) RecvTerminal(f Frame) (Terminal, error) {
	if f.Type != TypeTerminal {
		return Terminal{}, fmt.Errorf("%w: expected TERMINAL, got %s", ErrOutOfState, f.Type)
	}
	if p.state == coordDone {
		return Terminal{}, ErrDuplicateTerminal
	}
	// An aborted launch still owes a terminal, so coordAborted is admissible here.
	if p.state != coordRunning && p.state != coordArmed && p.state != coordAborted {
		return Terminal{}, fmt.Errorf("%w: TERMINAL in state %v", ErrOutOfState, p.state)
	}
	t, err := decodeTerminal(f.Payload)
	if err != nil {
		return Terminal{}, err
	}
	p.state = coordDone
	return t, nil
}

// MaxSpawnErrorClassBytes bounds the only variable-length field in TERMINAL, which is what keeps the
// whole frame inside the PIPE_BUF bound by construction rather than by hoping.
const MaxSpawnErrorClassBytes = 256

// Terminal carries the runner facts. It is the ONLY authoritative source of them: an exit 127 arrives
// inside a well-formed TERMINAL, while a supervisor that died is EOF on stat with no TERMINAL. Reading
// the facts from the supervisor's own exit status would make those two indistinguishable.
//
// It carries bounded scalars and never stream data, both because the streams go directly to the
// coordinator and because the PIPE_BUF bound admits nothing larger.
type Terminal struct {
	// CommandStarted is false when the command never ran: aborted before GO, or spawn failed.
	CommandStarted bool
	// Exited reports whether the command exited normally; ExitCode is meaningful only then.
	Exited   bool
	ExitCode int
	// Signal is the terminating signal when the command was killed, 0 otherwise.
	Signal int
	// SpawnErrorClass is set only when CommandStarted is false and a spawn was attempted.
	SpawnErrorClass string
	// CommandPGID is the owned group. Zero when no command was started, which is how a no-command
	// terminal is distinguishable structurally rather than by trusting the boolean alone.
	CommandPGID int
	// Reaped counts descendants reaped from the owned group.
	Reaped int
}

type wireTerminal struct {
	SchemaVersion   int    `json:"schema_version"`
	CommandStarted  bool   `json:"command_started"`
	Exited          bool   `json:"exited"`
	ExitCode        int    `json:"exit_code"`
	Signal          int    `json:"signal"`
	SpawnErrorClass string `json:"spawn_error_class"`
	CommandPGID     int    `json:"command_pgid"`
	Reaped          int    `json:"reaped"`
}

const terminalSchemaVersion = 1

// MaxExitCode is the widest exit status a POSIX wait status can carry.
const MaxExitCode = 255

// MaxSignal is the highest signal number Linux defines, including the real-time range.
const MaxSignal = 64

// validate enforces the COMPLETE sum type, not a sample of contradictions.
//
// These are the authoritative runner facts — the only statement anyone ever gets about how the command
// ended — so "not obviously self-contradictory" is the wrong bar. A terminal claiming a command
// started while carrying neither an exit code nor a signal describes no outcome at all, and would be
// consumed downstream as though it did.
func (t Terminal) validate() error {
	if len(t.SpawnErrorClass) > MaxSpawnErrorClassBytes {
		return fmt.Errorf("proctree: spawn error class %d bytes exceeds %d", len(t.SpawnErrorClass), MaxSpawnErrorClassBytes)
	}
	if t.Reaped < 0 {
		return errors.New("proctree: negative reap count")
	}
	if t.CommandPGID < 0 {
		return errors.New("proctree: negative command PGID")
	}

	if !t.CommandStarted {
		// Nothing ran, so every field describing a run must be at its zero. This covers the abort
		// (CANCEL before GO) and the spawn failure alike; the two differ only by SpawnErrorClass.
		if t.CommandPGID != 0 {
			return errors.New("proctree: terminal reports no command but carries a command PGID")
		}
		if t.Exited || t.Signal != 0 || t.ExitCode != 0 {
			return errors.New("proctree: terminal reports no command but carries exit facts")
		}
		if t.Reaped != 0 {
			return errors.New("proctree: terminal reports no command but claims reaped descendants")
		}
		return nil
	}

	if t.SpawnErrorClass != "" {
		return errors.New("proctree: terminal reports a started command and a spawn error")
	}
	// A started command has a real group: setpgid runs in the child before any user code.
	if t.CommandPGID <= 0 {
		return errors.New("proctree: terminal reports a started command without a command PGID")
	}
	// The leader is a member of the group the supervisor reaps, so a started command always accounts
	// for at least one reaped process. Zero means the reap loop did not run.
	if t.Reaped < 1 {
		return errors.New("proctree: terminal reports a started command with no reaped descendants")
	}
	// Exactly one of exited/signalled. "Neither" is the case that was accepted before: it describes no
	// outcome whatsoever while looking well formed.
	switch {
	case t.Exited && t.Signal != 0:
		return errors.New("proctree: terminal reports both a normal exit and a signal")
	case !t.Exited && t.Signal == 0:
		return errors.New("proctree: terminal reports a started command with neither an exit code nor a signal")
	case t.Exited:
		if t.ExitCode < 0 || t.ExitCode > MaxExitCode {
			return fmt.Errorf("proctree: exit code %d outside 0..%d", t.ExitCode, MaxExitCode)
		}
	default:
		if t.Signal < 1 || t.Signal > MaxSignal {
			return fmt.Errorf("proctree: signal %d outside 1..%d", t.Signal, MaxSignal)
		}
		if t.ExitCode != 0 {
			return errors.New("proctree: terminal reports a signalled command carrying an exit code")
		}
	}
	return nil
}

func (t Terminal) encode() ([]byte, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	b, err := canonjson.CanonicalizeValue(wireTerminal{
		SchemaVersion:   terminalSchemaVersion,
		CommandStarted:  t.CommandStarted,
		Exited:          t.Exited,
		ExitCode:        t.ExitCode,
		Signal:          t.Signal,
		SpawnErrorClass: t.SpawnErrorClass,
		CommandPGID:     t.CommandPGID,
		Reaped:          t.Reaped,
	})
	if err != nil {
		return nil, fmt.Errorf("proctree: encode terminal: %w", err)
	}
	if len(b) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: terminal payload %d", ErrFrameTooLarge, len(b))
	}
	return b, nil
}

func decodeTerminal(payload []byte) (Terminal, error) {
	var w wireTerminal
	if err := decodeExactlyOne(payload, &w); err != nil {
		return Terminal{}, fmt.Errorf("proctree: decode terminal: %w", err)
	}
	if w.SchemaVersion != terminalSchemaVersion {
		return Terminal{}, fmt.Errorf("proctree: terminal schema version %d, want %d", w.SchemaVersion, terminalSchemaVersion)
	}
	t := Terminal{
		CommandStarted:  w.CommandStarted,
		Exited:          w.Exited,
		ExitCode:        w.ExitCode,
		Signal:          w.Signal,
		SpawnErrorClass: w.SpawnErrorClass,
		CommandPGID:     w.CommandPGID,
		Reaped:          w.Reaped,
	}
	// Validated on the way IN as well as out: the coordinator must not accept a self-contradictory
	// terminal just because it is well-formed JSON.
	if err := t.validate(); err != nil {
		return Terminal{}, err
	}
	canonical, err := t.encode()
	if err != nil {
		return Terminal{}, err
	}
	if !bytes.Equal(canonical, payload) {
		return Terminal{}, errors.New("proctree: terminal bytes are not their own canonical encoding")
	}
	return t, nil
}
