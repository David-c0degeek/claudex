//go:build !windows

package proctree

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestReadReceiptDoesNotBlockOnAFIFO is the adversarial case: a FIFO at the authoritative name.
//
// Opening a FIFO read-only blocks until a writer appears, and it blocks in open(2) — before any byte
// ceiling can help. Recovery is exactly where an unbounded filesystem read cannot be tolerated, so
// this must fail promptly rather than wait for a writer that may never come. The test carries its own
// deadline because a regression here does not fail, it hangs.
func TestReadReceiptDoesNotBlockOnAFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, ReceiptFileName), 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	done := make(chan error, 1)
	go func() {
		_, rerr := ReadReceipt(root, "att-1")
		done <- rerr
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrReceiptInvalid) {
			t.Fatalf("err = %v, want ErrReceiptInvalid", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadReceipt blocked opening a FIFO")
	}
}

// TestReadReceiptRefusesASymlink: a symlink at the authoritative name could bless a different in-root
// file after the real no-clobber publish had conflicted, which is precisely the case no-clobber
// exists to make visible.
func TestReadReceiptRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// A genuine, valid receipt under a different name inside the same root.
	raw, err := startedReceipt().encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "decoy.json"), raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink("decoy.json", filepath.Join(dir, ReceiptFileName)); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := ReadReceipt(root, "att-1"); !errors.Is(err, ErrReceiptInvalid) {
		t.Fatalf("err = %v, want ErrReceiptInvalid — a symlink must not stand in for the receipt", err)
	}
}

// TestReadReceiptRefusesADirectory covers the remaining non-regular shape.
func TestReadReceiptRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ReceiptFileName), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	if _, err := ReadReceipt(root, "att-1"); !errors.Is(err, ErrReceiptInvalid) {
		t.Fatalf("err = %v, want ErrReceiptInvalid", err)
	}
}
