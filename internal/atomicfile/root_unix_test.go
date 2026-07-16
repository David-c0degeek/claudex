//go:build !windows

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO is refused as non-regular content, and the read returns promptly
// without a peer writer — proving the non-blocking open path.
func TestReadInRootFifoDoesNotBlock(t *testing.T) {
	r, dir := openRoot(t)
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadInRoot(r, "pipe", 1<<20)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("fifo read err = %v, want ErrNotRegular", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("ReadInRoot blocked on a FIFO with no writer")
	}
}

// An in-root symlink to a regular file is refused (ErrNotRegular), so the reader
// never follows a link even though os.Root would traverse one that stays inside.
func TestReadInRootRejectsSymlink(t *testing.T) {
	r, dir := openRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "real.json"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink("real.json", filepath.Join(dir, "link.json")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := ReadInRoot(r, "link.json", 1<<20); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("symlink read err = %v, want ErrNotRegular", err)
	}
}
