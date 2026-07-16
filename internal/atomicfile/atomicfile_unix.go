//go:build !windows

package atomicfile

import (
	"errors"
	"os"
	"syscall"
)

// replace renames oldpath onto newpath. POSIX rename(2) is an atomic replace.
func replace(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// readOpenExtraFlags hardens a content read-open on POSIX: O_NOFOLLOW refuses a
// symlink final component (ELOOP), and O_NONBLOCK guarantees opening a FIFO
// returns immediately instead of blocking for a peer writer. The type is then
// confirmed regular via Stat before any bytes are read.
const readOpenExtraFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// isSymlinkOpenErr reports whether an open failed because the final component is
// a symlink (O_NOFOLLOW → ELOOP), so the caller can report it as non-regular.
func isSymlinkOpenErr(err error) bool { return errors.Is(err, syscall.ELOOP) }

// isTransientOpen is always false on POSIX: rename and link are atomic and a
// reader never observes a sharing/access transient.
func isTransientOpen(error) bool { return false }

// syncRootDir fsyncs a directory within a confined root ("" is the root itself).
func syncRootDir(root *os.Root, name string) error {
	if name == "" {
		name = "."
	}
	d, err := root.Open(name)
	if err != nil {
		return err
	}
	serr := d.Sync()
	_ = d.Close()
	return serr
}

// syncDir fsyncs the directory so the rename is durable across a power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
