package proctree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ErrArmingDeadline means the arming handshake did not complete in time.
//
// Arming happens under the run guard, so anything unbounded there holds a lock shared with the rest
// of the run. The spec transfer is the specific hazard: a spec larger than pipe capacity plus a
// supervisor that never reads it would block the coordinator's write, and a blocked coordinator
// cannot run the very deadline that is supposed to bound arming.
var ErrArmingDeadline = errors.New("proctree: arming deadline exceeded")

// deadlineWriter is what an interruptible spec write requires. *os.File satisfies it for pipes
// created by os.Pipe, which are registered with the runtime poller.
type deadlineWriter interface {
	io.WriteCloser
	SetWriteDeadline(t time.Time) error
}

var _ deadlineWriter = (*os.File)(nil)

// WriteSpec writes the canonical spec and closes the write end so the reader sees EOF.
//
// The deadline is not advisory. A bare blocking write on a goroutine nobody can stop would reproduce
// the unbounded-under-guard defect this exists to prevent, so the write end is put under a deadline
// and closed on every path.
func WriteSpec(w deadlineWriter, canonical []byte, deadline time.Time) (err error) {
	defer func() {
		cerr := w.Close()
		if err == nil && cerr != nil {
			err = fmt.Errorf("proctree: close spec pipe: %w", cerr)
		}
	}()
	if len(canonical) > MaxSpecBytes {
		return fmt.Errorf("%w: spec %d bytes exceeds %d", ErrSpecInvalid, len(canonical), MaxSpecBytes)
	}
	if err := w.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("proctree: set spec write deadline: %w", err)
	}
	// Written in a LOOP, unlike a control frame. A frame is one atomic PIPE_BUF write, so a single
	// call settles it; the spec deliberately may exceed PIPE_BUF, so one Write is not a completion
	// guarantee. Discarding n left a supervisor holding a truncated spec while the coordinator
	// believed the transfer had succeeded — and a partial write followed by the deadline firing is a
	// real case on a pipe, not a hypothetical.
	for off := 0; off < len(canonical); {
		n, werr := w.Write(canonical[off:])
		off += n
		if werr != nil {
			if errors.Is(werr, os.ErrDeadlineExceeded) {
				return fmt.Errorf("%w: wrote %d of %d spec bytes", ErrArmingDeadline, off, len(canonical))
			}
			return fmt.Errorf("proctree: write execution spec: %w", werr)
		}
		if n <= 0 {
			return fmt.Errorf("proctree: spec write stalled after %d of %d bytes", off, len(canonical))
		}
	}
	return nil
}

// ReadSpec reads the canonical spec to EOF under the transport ceiling.
//
// It reads one byte past the ceiling deliberately: stopping exactly at the limit cannot distinguish
// "exactly at the ceiling" from "truncated at the ceiling", and silently accepting a truncated spec
// would hand the validator something the writer never sent.
func ReadSpec(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxSpecBytes+1))
	if err != nil {
		return nil, fmt.Errorf("proctree: read execution spec: %w", err)
	}
	if len(b) > MaxSpecBytes {
		return nil, fmt.Errorf("%w: spec exceeds %d bytes", ErrSpecInvalid, MaxSpecBytes)
	}
	return b, nil
}
