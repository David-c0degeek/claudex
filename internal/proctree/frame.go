// Package proctree owns the containment of the mechanical test command: the process group or job
// object the command runs in, the supervisor that outlives a dying coordinator on Linux, and the
// wire protocol between the two.
//
// This file is the wire protocol. Its shape is load-bearing rather than incidental: every frame is
// one blocking write of at most PIPE_BUF bytes, because POSIX makes a pipe write of at most
// PIPE_BUF atomic and that atomicity is what lets the coordinator conclude "a failed GO write means
// no command started". A frame assembled across two writes would let the coordinator observe an
// error while the supervisor had already read a complete GO and spawned, which would make the
// attempt record say command_started:false about a command that ran.
package proctree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Magic tags every frame so a stream carrying something else fails immediately rather than being
// interpreted as a very large length.
var magic = [4]byte{'C', 'X', 'P', 'T'}

// Version is the frame-format version. A reader refuses anything it does not know: the two peers are
// the same binary, so a mismatch means a mixed-version deployment, not a compatibility case to
// tolerate.
const Version uint16 = 1

// MaxFrameBytes is PIPE_BUF on Linux, the only platform that runs a supervisor. It bounds the WHOLE
// frame, header included, because the atomicity guarantee is about the write, not the payload.
const MaxFrameBytes = 4096

// headerBytes is magic(4) + version(2) + type(2) + length(4).
const headerBytes = 12

// MaxPayloadBytes is what is left for a payload once the header is accounted for.
const MaxPayloadBytes = MaxFrameBytes - headerBytes

// Type identifies a frame. ctrl (coordinator -> supervisor) carries Go and Cancel; stat (supervisor
// -> coordinator) carries Ready and Terminal. Direction is enforced by the state machines in
// protocol.go, not here: this layer only decides whether bytes are a well-formed frame.
type Type uint16

const (
	TypeReady    Type = 1
	TypeGo       Type = 2
	TypeCancel   Type = 3
	TypeTerminal Type = 4
)

func (t Type) String() string {
	switch t {
	case TypeReady:
		return "READY"
	case TypeGo:
		return "GO"
	case TypeCancel:
		return "CANCEL"
	case TypeTerminal:
		return "TERMINAL"
	default:
		return fmt.Sprintf("Type(%d)", uint16(t))
	}
}

func (t Type) known() bool {
	switch t {
	case TypeReady, TypeGo, TypeCancel, TypeTerminal:
		return true
	}
	return false
}

// Frame is one protocol message.
type Frame struct {
	Type    Type
	Payload []byte
}

var (
	// ErrShortWrite means the frame was not delivered whole. It is deliberately distinct from a
	// generic write error because it is the case the PIPE_BUF bound exists to make impossible: if it
	// is ever seen, the atomicity assumption has been violated and no conclusion may be drawn about
	// what the peer received.
	ErrShortWrite = errors.New("proctree: frame not written atomically")

	// ErrIncompleteFrame means the stream ended in the middle of a frame. The receiver treats this as
	// ABORT: a partial GO is not a GO, so nothing starts.
	ErrIncompleteFrame = errors.New("proctree: stream ended mid-frame")

	// ErrBadMagic, ErrBadVersion, ErrUnknownType and ErrFrameTooLarge are all fatal to the stream.
	// None is recoverable by resynchronising, because a stream whose framing cannot be trusted cannot
	// be trusted to carry facts either.
	ErrBadMagic      = errors.New("proctree: frame magic mismatch")
	ErrBadVersion    = errors.New("proctree: unsupported frame version")
	ErrUnknownType   = errors.New("proctree: unknown frame type")
	ErrFrameTooLarge = errors.New("proctree: frame exceeds PIPE_BUF bound")
)

// EncodeFrame renders a frame to its exact wire bytes. It is separate from WriteFrame so the
// single-write property can be asserted directly in tests.
func EncodeFrame(f Frame) ([]byte, error) {
	if !f.Type.known() {
		return nil, fmt.Errorf("%w: %d", ErrUnknownType, uint16(f.Type))
	}
	if len(f.Payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload %d exceeds %d", ErrFrameTooLarge, len(f.Payload), MaxPayloadBytes)
	}
	buf := make([]byte, headerBytes+len(f.Payload))
	copy(buf[0:4], magic[:])
	binary.BigEndian.PutUint16(buf[4:6], Version)
	binary.BigEndian.PutUint16(buf[6:8], uint16(f.Type))
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(f.Payload)))
	copy(buf[headerBytes:], f.Payload)
	return buf, nil
}

// WriteFrame writes a frame in exactly ONE Write call and treats anything less than the full length
// as a failure. Callers must not retry a short write: the peer's view is unknown, which is precisely
// what the caller is trying to avoid.
func WriteFrame(w io.Writer, f Frame) error {
	buf, err := EncodeFrame(f)
	if err != nil {
		return err
	}
	n, err := w.Write(buf)
	if err != nil {
		return fmt.Errorf("proctree: write %s: %w", f.Type, err)
	}
	if n != len(buf) {
		return fmt.Errorf("%w: %s wrote %d of %d", ErrShortWrite, f.Type, n, len(buf))
	}
	return nil
}

// ReadFrame reads one frame. It distinguishes three endings that mean different things:
//
//   - io.EOF returned with no bytes consumed: the peer closed cleanly between frames. For the ctrl
//     reader this is the owner-death signal; for the stat reader it means the supervisor is gone.
//   - ErrIncompleteFrame: the peer died mid-frame. Never interpreted as the frame it was becoming.
//   - a framing error: the stream is corrupt and is fatal to the side that read it.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [headerBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Frame{}, fmt.Errorf("%w: header truncated", ErrIncompleteFrame)
		}
		return Frame{}, fmt.Errorf("proctree: read frame header: %w", err)
	}
	if [4]byte(hdr[0:4]) != magic {
		return Frame{}, ErrBadMagic
	}
	if v := binary.BigEndian.Uint16(hdr[4:6]); v != Version {
		return Frame{}, fmt.Errorf("%w: %d", ErrBadVersion, v)
	}
	t := Type(binary.BigEndian.Uint16(hdr[6:8]))
	if !t.known() {
		return Frame{}, fmt.Errorf("%w: %d", ErrUnknownType, uint16(t))
	}
	n := binary.BigEndian.Uint32(hdr[8:12])
	// Checked before allocating: an over-length header is exactly how a corrupt stream would try to
	// make the reader reserve memory it should not.
	if n > MaxPayloadBytes {
		return Frame{}, fmt.Errorf("%w: declared %d exceeds %d", ErrFrameTooLarge, n, MaxPayloadBytes)
	}
	f := Frame{Type: t}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return Frame{}, fmt.Errorf("%w: payload truncated", ErrIncompleteFrame)
			}
			return Frame{}, fmt.Errorf("proctree: read frame payload: %w", err)
		}
	}
	return f, nil
}
