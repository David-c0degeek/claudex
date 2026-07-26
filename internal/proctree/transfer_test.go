package proctree

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"
)

// noDeadlineWriter reports that deadlines are unsupported, which must fail closed: proceeding with an
// unbounded write is exactly the unbounded-under-guard defect.
type noDeadlineWriter struct {
	wrote  bool
	closed bool
}

func (w *noDeadlineWriter) Write(p []byte) (int, error)      { w.wrote = true; return len(p), nil }
func (w *noDeadlineWriter) Close() error                     { w.closed = true; return nil }
func (w *noDeadlineWriter) SetWriteDeadline(time.Time) error { return os.ErrNoDeadline }

func TestWriteSpecRoundTrip(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	canonical, err := baseSpec().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- WriteSpec(client, canonical, time.Now().Add(10*time.Second)) }()

	got, err := ReadSpec(server)
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	if !bytes.Equal(got, canonical) {
		t.Fatal("spec bytes differ across the transport")
	}
	if _, err := DecodeSpec(got); err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
}

// TestWriteSpecOverRealPipe covers the descriptor the supervisor actually inherits. It skips where
// os.Pipe cannot carry a deadline — which is not a gap in coverage so much as a restatement of the
// platform boundary: WriteSpec refuses to write at all without a bound, and the supervisor that uses
// it exists only on Linux.
func TestWriteSpecOverRealPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	if derr := w.SetWriteDeadline(time.Now().Add(time.Second)); derr != nil {
		w.Close()
		if errors.Is(derr, os.ErrNoDeadline) {
			t.Skipf("os.Pipe write deadlines unsupported on %s; the supervisor path is Linux-only", runtime.GOOS)
		}
		t.Fatalf("SetWriteDeadline: %v", derr)
	}

	canonical, err := baseSpec().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- WriteSpec(w, canonical, time.Now().Add(10*time.Second)) }()

	got, err := ReadSpec(r)
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	if !bytes.Equal(got, canonical) {
		t.Fatal("spec bytes differ across the pipe")
	}
}

// TestWriteSpecDeadlineFires is the pinned case: a peer that accepts the descriptors and then never
// reads the spec must not be able to hold the writer — and therefore the run guard — indefinitely.
//
// net.Pipe is used rather than os.Pipe because it is synchronous and supports deadlines on every
// platform, so this exercises WriteSpec's real interruptibility path in CI everywhere. The os.Pipe
// variant below covers the descriptor the supervisor actually inherits, where support is
// platform-dependent.
func TestWriteSpecDeadlineFires(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close() // the peer that never reads

	canonical, err := baseSpec().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	start := time.Now()
	err = WriteSpec(client, canonical, time.Now().Add(150*time.Millisecond))
	if !errors.Is(err, ErrArmingDeadline) {
		t.Fatalf("err = %v, want ErrArmingDeadline", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("write blocked for %v; the deadline is not interrupting it", elapsed)
	}
}

func TestWriteSpecOverRealPipeDeadline(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	if err := w.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		w.Close()
		if errors.Is(err, os.ErrNoDeadline) {
			t.Skipf("os.Pipe write deadlines unsupported on %s; the supervisor path is Linux-only", runtime.GOOS)
		}
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	// Larger than any pipe buffer, with nothing draining it.
	big := make([]byte, MaxSpecBytes)
	start := time.Now()
	err = WriteSpec(w, big, time.Now().Add(150*time.Millisecond))
	if !errors.Is(err, ErrArmingDeadline) {
		t.Fatalf("err = %v, want ErrArmingDeadline", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("write blocked for %v", elapsed)
	}
}

// TestWriteSpecFailsClosedWithoutDeadlineSupport: if the bound cannot be armed, the write must not
// happen at all. Writing anyway would be strictly worse than refusing, because it would reintroduce
// the unbounded case while looking like it had been handled.
func TestWriteSpecFailsClosedWithoutDeadlineSupport(t *testing.T) {
	w := &noDeadlineWriter{}
	err := WriteSpec(w, []byte("{}"), time.Now().Add(time.Second))
	if err == nil {
		t.Fatal("WriteSpec proceeded without an armed deadline")
	}
	if w.wrote {
		t.Fatal("WriteSpec wrote despite being unable to bound the write")
	}
	if !w.closed {
		t.Fatal("WriteSpec left the write end open; the reader would never see EOF")
	}
}

func TestReadSpecRefusesOverLength(t *testing.T) {
	_, err := ReadSpec(bytes.NewReader(make([]byte, MaxSpecBytes+1)))
	if !errors.Is(err, ErrSpecInvalid) {
		t.Fatalf("err = %v, want ErrSpecInvalid", err)
	}
	// Exactly at the ceiling is admissible; the reader reads one byte past it precisely so that
	// "at the ceiling" and "truncated at the ceiling" stay distinguishable.
	got, err := ReadSpec(bytes.NewReader(make([]byte, MaxSpecBytes)))
	if err != nil {
		t.Fatalf("at-ceiling read: %v", err)
	}
	if len(got) != MaxSpecBytes {
		t.Fatalf("read %d bytes, want %d", len(got), MaxSpecBytes)
	}
}

func TestWriteSpecRefusesOverLength(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	if err := WriteSpec(w, make([]byte, MaxSpecBytes+1), time.Now().Add(time.Second)); !errors.Is(err, ErrSpecInvalid) {
		t.Fatalf("err = %v, want ErrSpecInvalid", err)
	}
	// The write end is closed on every path, so a supervisor blocked on ReadSpec sees EOF rather
	// than hanging.
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read after refused write: %v", err)
	}
}

// shortStreamWriter transfers only part of what it is given and reports success, which io.Writer
// permits implementations to get wrong and which a pipe under a deadline does for real.
type shortStreamWriter struct {
	got    []byte
	closed bool
}

func (w *shortStreamWriter) Write(p []byte) (int, error) {
	n := len(p) / 2
	if n == 0 {
		n = len(p)
	}
	w.got = append(w.got, p[:n]...)
	return n, nil
}
func (w *shortStreamWriter) Close() error                     { w.closed = true; return nil }
func (w *shortStreamWriter) SetWriteDeadline(time.Time) error { return nil }

// TestWriteSpecTransfersEverything. A control frame is one atomic PIPE_BUF write, so a single call
// settles it — but the spec may exceed PIPE_BUF by design, so one Write proves nothing. Discarding n
// left the supervisor holding a truncated spec while the coordinator reported success.
func TestWriteSpecTransfersEverything(t *testing.T) {
	canonical, err := baseSpec().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	w := &shortStreamWriter{}
	if err := WriteSpec(w, canonical, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	if !bytes.Equal(w.got, canonical) {
		t.Fatalf("transferred %d of %d bytes", len(w.got), len(canonical))
	}
	if !w.closed {
		t.Fatal("write end left open")
	}
}

// stalledWriter accepts nothing and reports no error, the shape that would spin forever.
type stalledWriter struct{}

func (stalledWriter) Write([]byte) (int, error)        { return 0, nil }
func (stalledWriter) Close() error                     { return nil }
func (stalledWriter) SetWriteDeadline(time.Time) error { return nil }

func TestWriteSpecRefusesAStalledWriter(t *testing.T) {
	canonical, err := baseSpec().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := WriteSpec(stalledWriter{}, canonical, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("WriteSpec reported success against a writer that accepted nothing")
	}
}

// partialThenDeadlineWriter is the real pipe case: some bytes go through, then the deadline fires.
// The partial transfer must be an error, never a success.
type partialThenDeadlineWriter struct{ wrote int }

func (w *partialThenDeadlineWriter) Write(p []byte) (int, error) {
	if w.wrote == 0 && len(p) > 4 {
		w.wrote = 4
		return 4, nil
	}
	return 0, os.ErrDeadlineExceeded
}
func (w *partialThenDeadlineWriter) Close() error                     { return nil }
func (w *partialThenDeadlineWriter) SetWriteDeadline(time.Time) error { return nil }

func TestWriteSpecPartialThenDeadline(t *testing.T) {
	canonical, err := baseSpec().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	err = WriteSpec(&partialThenDeadlineWriter{}, canonical, time.Now().Add(time.Minute))
	if !errors.Is(err, ErrArmingDeadline) {
		t.Fatalf("err = %v, want ErrArmingDeadline", err)
	}
}
