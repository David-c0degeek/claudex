//go:build windows

package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

// openRealParent opens the immediate rooted parent of `name` (os.Root refuses symlink
// traversal) and returns its validated real DOS path plus a close func that KEEPS the
// handle pinned until called — so the resolved directory cannot be swapped between
// resolution and the leaf-name operations performed under it.
func openRealParent(root *os.Root, name string) (realParent string, closeFn func() error, err error) {
	openName := rootParent(name)
	if openName == "" {
		openName = "."
	}
	pf, err := root.Open(openName)
	if err != nil {
		return "", nil, err
	}
	realParent, err = finalPathByHandle(windows.Handle(pf.Fd()))
	if err != nil {
		_ = pf.Close()
		return "", nil, err
	}
	return realParent, pf.Close, nil
}

// moveFileExPath is moveFileEx over string paths.
func moveFileExPath(from, to string, flags uint32) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return moveFileEx(f, t, flags)
}

// isAlreadyExists reports a no-replace move that failed because the destination ALREADY
// EXISTED before the move — a genuine concurrent/no-clobber conflict, distinct from our own
// move becoming visible before a later flush error.
func isAlreadyExists(err error) bool {
	return errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS)
}

// parentBarrierInRoot forces the immediate rooted parent of `name` (and thus `name`'s
// already-present entry) durable to disk by performing a real MOVEFILE_WRITE_THROUGH rename
// of a throwaway temp directory WITHIN the validated real parent — a directory-handle flush
// is refused by Windows, so the write-through rename is the metadata barrier. Re-runnable
// and idempotent.
func parentBarrierInRoot(root *os.Root, name string) error {
	realParent, closeParent, err := openRealParent(root, name)
	if err != nil {
		return err
	}
	defer closeParent()
	a, err := os.MkdirTemp(realParent, tmpPrefix)
	if err != nil {
		return err
	}
	b := a + "b" // a is unique, so a+"b" does not exist
	defer func() {
		_ = os.RemoveAll(a)
		_ = os.RemoveAll(b)
	}()
	return moveFileExPath(a, b, dirPublishFlags)
}

// confirmParentInRoot re-confirms an existing directory's entry durable via the real parent
// barrier (used by mkdirInRoot's exists/recovery branch).
func confirmParentInRoot(root *os.Root, name string) error {
	return parentBarrierInRoot(root, name)
}

// resolveWriteThroughFailure classifies a failed no-clobber write-through publish of `name`.
// isFile selects the conflict semantics: a directory publish is idempotent (a pre-existing
// directory is a durable concurrent winner), a file publish is strict no-clobber (a
// pre-existing target is an fs.ErrExist conflict). Only a GENUINE already-exists error is a
// concurrent winner; any other failure with the target nonetheless visible is OUR own move
// whose durability is unconfirmed, forced durable by the parent barrier (else committed
// *PostCommitSyncError); a target not visible is uncommitted (raw error).
func resolveWriteThroughFailure(root *os.Root, name string, merr error, isFile bool) error {
	if isAlreadyExists(merr) {
		if isFile {
			return fs.ErrExist // no-clobber conflict
		}
		if info, serr := root.Lstat(name); serr == nil && info.IsDir() {
			return nil // a concurrent winner via this same write-through path is durable
		}
		return &PostCommitSyncError{Path: name, Err: merr}
	}
	if _, serr := root.Lstat(name); serr != nil {
		return merr // not visible → the move did not commit (uncommitted, raw error)
	}
	if berr := parentBarrierInRoot(root, name); berr != nil {
		return &PostCommitSyncError{Path: name, Err: berr}
	}
	return nil
}

// publishDirInRoot durably creates a fresh directory within the confined root. It creates a
// temp sibling under the validated real parent and renames it to the target leaf with
// MOVEFILE_WRITE_THROUGH (no REPLACE_EXISTING → no-clobber), which forces the parent's
// metadata to disk. A failed move is classified by resolveWriteThroughFailure.
func publishDirInRoot(root *os.Root, name string, perm os.FileMode) error {
	realParent, closeParent, err := openRealParent(root, name)
	if err != nil {
		return err
	}
	defer closeParent()
	tmp, err := os.MkdirTemp(realParent, tmpPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // a no-op once tmp is renamed to the target
	_ = os.Chmod(tmp, perm)
	if merr := moveFileExPath(tmp, realParent+`\`+path.Base(name), dirPublishFlags); merr != nil {
		return resolveWriteThroughFailure(root, name, merr, false)
	}
	return nil
}

// publishFileInRoot durably publishes an already-written temp file to `name` within the
// confined root via a MOVEFILE_WRITE_THROUGH no-clobber rename (no REPLACE_EXISTING), so the
// file's directory ENTRY is power-safe. Returns whether the temp was consumed (renamed into
// place). A pre-existing target is an fs.ErrExist no-clobber conflict; transient
// sharing/access errors are retried.
func publishFileInRoot(root *os.Root, tmp, name, dir string) (bool, error) {
	realParent, closeParent, err := openRealParent(root, name)
	if err != nil {
		return false, err
	}
	defer closeParent()
	tmpAbs := realParent + `\` + path.Base(tmp)
	target := realParent + `\` + path.Base(name)
	merr := replaceWith(
		func() error { return moveFileExPath(tmpAbs, target, dirPublishFlags) },
		func() { time.Sleep(renameBackoff) },
		isTransientRename,
	)
	if merr == nil {
		return true, nil // renamed durably (temp consumed)
	}
	if isAlreadyExists(merr) {
		return false, fs.ErrExist // no-clobber conflict; temp not consumed
	}
	if _, serr := root.Lstat(name); serr != nil {
		return false, merr // not visible → uncommitted; temp not consumed
	}
	// Own move visible but flush unconfirmed: the temp is now the target (consumed). Force
	// durability via the barrier.
	if berr := parentBarrierInRoot(root, name); berr != nil {
		return true, &PostCommitSyncError{Path: name, Err: berr}
	}
	return true, nil
}

// finalPathFlags is VOLUME_NAME_DOS (0x0) | FILE_NAME_NORMALIZED (0x0): a normalized DOS
// drive-letter path. x/sys/windows does not export these zero-valued flags, so named here.
const finalPathFlags = 0

// finalPathByHandle returns the validated real DOS path of an open handle, keeping the
// caller's handle open so the resolution cannot be swapped out from under it.
func finalPathByHandle(h windows.Handle) (string, error) {
	return finalPathByHandleBuf(h, make([]uint16, 260))
}

// finalPathByHandleBuf is the growable core (buf injectable so a test can force the grow
// path with a tiny initial buffer).
func finalPathByHandleBuf(h windows.Handle, buf []uint16) (string, error) {
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), finalPathFlags)
		if err != nil {
			return "", err
		}
		// The insufficient-buffer return INCLUDES the terminating NUL; the success return
		// EXCLUDES it. So n >= len(buf) is the insufficient case (grow to n and retry);
		// success is n < len(buf).
		if n >= uint32(len(buf)) {
			buf = make([]uint16, n)
			continue
		}
		return windows.UTF16ToString(buf[:n]), nil
	}
}

// maxRenameAttempts bounds the transient-error retry loop on Windows.
const maxRenameAttempts = 20

// moveFileEx is the Windows durable-move syscall behind a seam, so a test can assert the
// EXACT flags the file replace and the directory publish request. Production wires
// windows.MoveFileEx.
var moveFileEx = windows.MoveFileEx

// replace renames oldpath onto newpath durably. It uses MoveFileEx with
// MOVEFILE_REPLACE_EXISTING|MOVEFILE_WRITE_THROUGH so the move (both the new file's
// contents and the directory entry) is flushed to disk before it returns — the
// power-loss durability that plain os.Rename does not provide on Windows. It retries
// only the transient sharing/access errors that occur when another process
// momentarily has the target open.
func replace(oldpath, newpath string) error {
	return replaceWith(
		func() error { return moveFileWriteThrough(oldpath, newpath) },
		func() { time.Sleep(renameBackoff) },
		isTransientRename,
	)
}

// fileReplaceFlags replaces an existing target file durably.
const fileReplaceFlags = windows.MOVEFILE_REPLACE_EXISTING | windows.MOVEFILE_WRITE_THROUGH

// dirPublishFlags publishes a fresh directory durably. MOVEFILE_REPLACE_EXISTING is
// deliberately omitted: the target does not exist yet, and MoveFileEx documents
// REPLACE_EXISTING as invalid when either path names a directory. WRITE_THROUGH forces
// the parent's directory-entry metadata to disk before the rename returns.
const dirPublishFlags = windows.MOVEFILE_WRITE_THROUGH

// moveFileWriteThrough is the durable file replace used by replace. Isolated so a test
// can assert the exact write-through flags.
func moveFileWriteThrough(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	return moveFileEx(from, to, fileReplaceFlags)
}

// ensureDirDurableImpl durably publishes a fresh directory on Windows, where a directory
// handle cannot be flushed. A missing dir is published via a write-through rename; an
// EXISTING dir is RE-confirmed durable via the real parent barrier (never a swallowed
// no-op), so a retry after a prior partial failure re-forces the entry.
func ensureDirDurableImpl(dir string, perm os.FileMode) error {
	if fi, err := os.Lstat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%w: %s", ErrNotDirectory, dir)
		}
		return parentBarrier(dir)
	} else if !os.IsNotExist(err) {
		return err
	}
	return publishDirWriteThrough(dir, perm)
}

// parentBarrier forces the immediate parent of dir (and thus dir's already-present entry)
// durable: a real MOVEFILE_WRITE_THROUGH rename of a throwaway temp directory WITHIN the
// parent flushes the parent's directory metadata. The path-based analogue of
// parentBarrierInRoot. Re-runnable and idempotent.
func parentBarrier(dir string) error {
	parent := filepath.Dir(dir)
	a, err := os.MkdirTemp(parent, tmpPrefix)
	if err != nil {
		return err
	}
	b := a + "b" // a is unique, so a+"b" does not exist
	defer func() {
		_ = os.RemoveAll(a)
		_ = os.RemoveAll(b)
	}()
	return moveFileExPath(a, b, dirPublishFlags)
}

func publishDirWriteThrough(dir string, perm os.FileMode) error {
	parent := filepath.Dir(dir)
	tmp, err := os.MkdirTemp(parent, tmpPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // a no-op once tmp is renamed to the target
	_ = os.Chmod(tmp, perm)
	if merr := moveFileExPath(tmp, dir, dirPublishFlags); merr != nil {
		// Genuine already-exists → concurrent winner (durable if a directory). Any other
		// error with the target visible → our own visible-but-unconfirmed move: force
		// durability via the real parent barrier, *PostCommitSyncError if it fails. Target
		// not visible → uncommitted (raw error). Never bless a visible-but-unconfirmed dir.
		if isAlreadyExists(merr) {
			if fi, serr := os.Lstat(dir); serr == nil && fi.IsDir() {
				return nil
			}
			return &PostCommitSyncError{Path: dir, Err: merr}
		}
		if _, serr := os.Lstat(dir); serr != nil {
			return merr
		}
		if berr := parentBarrier(dir); berr != nil {
			return &PostCommitSyncError{Path: dir, Err: berr}
		}
		return nil
	}
	return nil
}

// replaceWith is the testable core: it calls renameFn, retrying (with sleepFn
// between attempts) only while transient reports the error retryable, up to
// maxRenameAttempts, and never sleeps after the final attempt.
func replaceWith(renameFn func() error, sleepFn func(), transient func(error) bool) error {
	for i := 0; ; i++ {
		err := renameFn()
		if err == nil {
			return nil
		}
		if i >= maxRenameAttempts-1 || !transient(err) {
			return err
		}
		sleepFn()
	}
}

func isTransientRename(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// isTransientOpen reports the transient sharing/access errors a reader or a
// hard-link/rename publisher hits while another process holds the target.
func isTransientOpen(err error) bool { return isTransientRename(err) }

// syncRootDir flushes the directory `name` within the confined root so a newly
// created entry inside it is durable. It opens the directory handle (Go opens dirs
// with FILE_FLAG_BACKUP_SEMANTICS) and FlushFileBuffers via Sync; Windows refuses to
// flush a directory handle with ERROR_ACCESS_DENIED, which is treated as satisfied
// (see swallowDirFlush).
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
	return swallowDirFlush(serr)
}

// swallowDirFlush treats Windows's refusal to flush a directory handle
// (ERROR_ACCESS_DENIED from FlushFileBuffers) as satisfied. This is NOT a durability
// shortcut: on Windows, directory-entry durability is established at WRITE time — new
// generation files are installed with MOVEFILE_WRITE_THROUGH file renames (replace) and
// new directories are published with MOVEFILE_WRITE_THROUGH directory renames
// (ensureDirDurableImpl), both of which force the parent's metadata to disk before
// returning. So a re-confirmation whose only obstacle is the handle-flush refusal has
// nothing left to force. Any other error is a real durability failure and is returned.
func swallowDirFlush(err error) error {
	if err == nil || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil
	}
	return err
}

// readOpenExtraFlags adds nothing on Windows: O_NOFOLLOW/O_NONBLOCK are not
// meaningful, and os.Root already refuses reparse points (symlinks) during
// traversal. Type is confirmed regular via Lstat/Stat.
const readOpenExtraFlags = 0

// isSymlinkOpenErr is always false on Windows; symlink rejection happens in
// os.Root traversal and the pre-open Lstat check.
func isSymlinkOpenErr(error) bool { return false }

// syncDir attempts to flush the directory `dir`. Go opens a directory with
// FILE_FLAG_BACKUP_SEMANTICS, so Sync issues FlushFileBuffers on the handle; Windows
// refuses that with ERROR_ACCESS_DENIED, treated as satisfied (see swallowDirFlush). The
// load-bearing durability on Windows is not this flush but the MOVEFILE_WRITE_THROUGH file
// renames (replace) and directory publishes (ensureDirDurableImpl) that force the parent's
// metadata to disk at write time; syncDir is the re-confirmation seam whose obstacle, when
// present, is only the handle-flush refusal.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	_ = d.Close()
	return swallowDirFlush(serr)
}
