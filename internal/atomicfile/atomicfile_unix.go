//go:build !windows

package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// publishDirInRoot creates a fresh directory within the confined root and forces its entry
// durable by fsyncing the immediate rooted parent. Re-runnable: os.Root.Mkdir's EEXIST is
// ignored and the parent fsync re-confirms on a retry.
func publishDirInRoot(root *os.Root, name string, perm os.FileMode) error {
	if err := root.Mkdir(name, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if serr := syncRootDir(root, rootParent(name)); serr != nil {
		return &PostCommitSyncError{Path: name, Err: serr}
	}
	return nil
}

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

// ensureDirDurableImpl creates dir if missing and forces its entry durable in its parent
// by fsyncing the parent. It is idempotent and re-runnable: a create-then-fsync where the
// fsync failed is repaired on a retry (os.Mkdir returns EEXIST, ignored, and the parent
// fsync runs again).
func ensureDirDurableImpl(dir string, perm os.FileMode) error {
	if err := os.Mkdir(dir, perm); err != nil && !os.IsExist(err) {
		return err
	}
	return syncDir(filepath.Dir(dir))
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
