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
// tolerated (and the parent fsync re-confirms on a retry), but a name occupied by a
// non-directory (a file/symlink that raced in) is refused as ErrNotDirectory.
func publishDirInRoot(root *os.Root, name string, perm os.FileMode) error {
	if err := root.Mkdir(name, perm); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		// Lstat (not Stat): an in-root symlink to a directory must not be accepted, and a
		// file/symlink that occupied the name between the missing-check and here is refused.
		info, serr := root.Lstat(name)
		if serr != nil {
			return serr
		}
		if !info.IsDir() {
			return ErrNotDirectory
		}
	}
	if serr := syncRootDir(root, rootParent(name)); serr != nil {
		return &PostCommitSyncError{Path: name, Err: serr}
	}
	return nil
}

// confirmParentInRoot forces the immediate rooted parent of `name` durable by fsyncing it,
// so an already-present entry becomes power-safe. Re-runnable and idempotent.
func confirmParentInRoot(root *os.Root, name string) error {
	return syncRootDir(root, rootParent(name))
}

// publishFileInRoot hard-links the already-written temp into place (no-clobber) and forces
// the file's directory and the root durable. The link leaves the temp behind (not
// consumed), so the caller removes it.
func publishFileInRoot(root *os.Root, tmp, name, dir string) (bool, error) {
	if err := moveWithRetry(root.Link, tmp, name); err != nil {
		return false, err // fs.ErrExist for a no-clobber conflict, else a real error
	}
	if dir != "" {
		if serr := syncRootDir(root, dir); serr != nil {
			return false, &PostCommitSyncError{Path: name, Err: serr}
		}
	}
	if serr := syncRootDir(root, ""); serr != nil {
		return false, &PostCommitSyncError{Path: name, Err: serr}
	}
	return false, nil
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
// (parentBarrier = fsync the parent on POSIX). Idempotent and re-runnable: a create-then-
// barrier where the barrier failed is repaired on a retry (Mkdir's EEXIST is tolerated for
// a real directory, and the parent barrier re-runs); a name occupied by a non-directory is
// refused.
func ensureDirDurableImpl(dir string, perm os.FileMode) error {
	if err := os.Mkdir(dir, perm); err != nil {
		if !os.IsExist(err) {
			return err
		}
		if fi, serr := os.Lstat(dir); serr != nil {
			return serr
		} else if !fi.IsDir() {
			return ErrNotDirectory
		}
	}
	return parentBarrier(dir)
}

// parentBarrier forces the immediate parent of dir (and thus dir's entry) durable by
// fsyncing the parent. Re-runnable and idempotent; the path-based analogue of
// confirmParentInRoot.
func parentBarrier(dir string) error {
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
