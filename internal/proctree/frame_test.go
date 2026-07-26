package proctree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// countingWriter records how many Write calls a frame costs. The single-write property is the whole
// basis for "a failed GO write means no command started", so it is asserted rather than assumed.
type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

// shortWriter reports fewer bytes than it consumed, simulating a non-atomic delivery.
type shortWriter struct{ short int }

func (w *shortWriter) Write(p []byte) (int, error) { return len(p) - w.short, nil }

func TestFrameRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     Type
		payload []byte
	}{
		{"ready", TypeReady, bytes.Repeat([]byte{0xAB}, ChallengeBytes)},
		{"go", TypeGo, bytes.Repeat([]byte{0x01}, ChallengeBytes)},
		{"cancel", TypeCancel, nil},
		{"terminal", TypeTerminal, []byte(`{"a":1}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w countingWriter
			if err := WriteFrame(&w, Frame{Type: tc.typ, Payload: tc.payload}); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			if w.writes != 1 {
				t.Fatalf("frame took %d Write calls, want exactly 1", w.writes)
			}
			if w.buf.Len() > MaxFrameBytes {
				t.Fatalf("frame is %d bytes, exceeds the PIPE_BUF bound %d", w.buf.Len(), MaxFrameBytes)
			}
			got, err := ReadFrame(&w.buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if got.Type != tc.typ {
				t.Fatalf("type = %v, want %v", got.Type, tc.typ)
			}
			if !bytes.Equal(got.Payload, tc.payload) {
				t.Fatalf("payload = %x, want %x", got.Payload, tc.payload)
			}
		})
	}
}

// TestShortWriteIsRefused is the adversarial seam CX pinned: without it, a coordinator could observe
// a partial write while the supervisor had already assembled a complete GO.
func TestShortWriteIsRefused(t *testing.T) {
	err := WriteFrame(&shortWriter{short: 1}, Frame{Type: TypeGo, Payload: []byte("x")})
	if !errors.Is(err, ErrShortWrite) {
		t.Fatalf("err = %v, want ErrShortWrite", err)
	}
}

func TestCleanEOFAtFrameBoundary(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// TestTruncatedFrameIsNotTheFrame proves a partial GO is refused rather than acted on. The two cuts
// are distinct code paths: a short header never reaches payload handling at all.
func TestTruncatedFrameIsNotTheFrame(t *testing.T) {
	full, err := EncodeFrame(Frame{Type: TypeGo, Payload: bytes.Repeat([]byte{7}, ChallengeBytes)})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"header cut", headerBytes - 1},
		{"payload cut", len(full) - 1},
		{"no payload at all", headerBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(full[:tc.n]))
			if !errors.Is(err, ErrIncompleteFrame) {
				t.Fatalf("err = %v, want ErrIncompleteFrame", err)
			}
		})
	}
}

func TestFramingErrorsAreFatal(t *testing.T) {
	good, err := EncodeFrame(Frame{Type: TypeCancel})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}

	badMagic := append([]byte(nil), good...)
	badMagic[0] ^= 0xFF
	if _, err := ReadFrame(bytes.NewReader(badMagic)); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("magic: err = %v, want ErrBadMagic", err)
	}

	badVersion := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(badVersion[4:6], Version+1)
	if _, err := ReadFrame(bytes.NewReader(badVersion)); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("version: err = %v, want ErrBadVersion", err)
	}

	badType := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(badType[6:8], 9999)
	if _, err := ReadFrame(bytes.NewReader(badType)); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("type: err = %v, want ErrUnknownType", err)
	}

	// A declared length beyond the bound must be refused from the HEADER, before any allocation:
	// an over-length declaration is exactly how a corrupt stream would try to make the reader
	// reserve memory it should not.
	huge := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(huge[8:12], MaxPayloadBytes+1)
	if _, err := ReadFrame(bytes.NewReader(huge)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("length: err = %v, want ErrFrameTooLarge", err)
	}
}

func TestEncodeRefusesOversizePayload(t *testing.T) {
	_, err := EncodeFrame(Frame{Type: TypeTerminal, Payload: make([]byte, MaxPayloadBytes+1)})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if _, err := EncodeFrame(Frame{Type: TypeTerminal, Payload: make([]byte, MaxPayloadBytes)}); err != nil {
		t.Fatalf("payload exactly at the bound must be accepted: %v", err)
	}
}

func TestEncodeRefusesUnknownType(t *testing.T) {
	if _, err := EncodeFrame(Frame{Type: Type(42)}); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("err = %v, want ErrUnknownType", err)
	}
}
