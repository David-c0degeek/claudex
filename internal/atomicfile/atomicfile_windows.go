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
// traversal) and returns its validated real DOS path and the open *os.File. NOTE: keeping
// this handle open does NOT pin the object — os.Root opens with FILE_SHARE_DELETE, so the
// directory can still be renamed/replaced. Callers PROVE a by-path reopen is the same object
// via its identity (identOf) and, for multi-step work, hold a no-share-delete handle
// (openDirNoDelete) across the ops. The caller closes the returned file.
func openRealParent(root *os.Root, name string) (realParent string, pf *os.File, err error) {
	openName := rootParent(name)
	if openName == "" {
		openName = "."
	}
	pf, err = root.Open(openName)
	if err != nil {
		return "", nil, err
	}
	realParent, err = finalPathByHandle(windows.Handle(pf.Fd()))
	if err != nil {
		_ = pf.Close()
		return "", nil, err
	}
	return realParent, pf, nil
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

// fileIdent identifies a filesystem object by volume serial + file index — stable across
// reopens of the SAME object, so a by-path reopen can be proven to be the object a validated
// os.Root handle resolved (guarding a directory swap between resolution and the reopen).
type fileIdent struct{ vol, idxHi, idxLo uint32 }

func identOf(h windows.Handle) (fileIdent, error) {
	var bhfi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bhfi); err != nil {
		return fileIdent{}, err
	}
	return fileIdent{bhfi.VolumeSerialNumber, bhfi.FileIndexHigh, bhfi.FileIndexLow}, nil
}

// openDirNoDelete opens the directory at dirPath with `access` and WITHOUT FILE_SHARE_DELETE
// (so it cannot be renamed/deleted while the returned handle is held), requiring it to be the
// SAME object as `want`. ReOpenFile is refused (ACCESS_DENIED) on an os.Root directory handle
// on this platform, so identity is proven via GetFileInformationByHandle rather than by
// handle. FILE_FLAG_OPEN_REPARSE_POINT refuses a final-component symlink swap.
func openDirNoDelete(dirPath string, access uint32, want fileIdent) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(dirPath)
	if err != nil {
		return windows.InvalidHandle, err
	}
	h, err := windows.CreateFile(
		p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, // NO FILE_SHARE_DELETE — pins the object
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	got, ierr := identOf(h)
	if ierr != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, ierr
	}
	if got != want {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, fmt.Errorf("atomicfile: directory %q changed identity before the operation", dirPath)
	}
	return h, nil
}

// flushDirIdent is the rooted directory-metadata barrier seam: it opens realParent writable
// and no-share-delete, PROVES it is the same object as `want`, and FlushFileBuffers it —
// forcing the directory's entries to disk. Behind a package var so a test can drive a
// fail-once-then-succeed retry.
var flushDirIdent = flushDirIdentImpl

func flushDirIdentImpl(realParent string, want fileIdent) error {
	h, err := openDirNoDelete(realParent, windows.FILE_APPEND_DATA|windows.SYNCHRONIZE, want)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.FlushFileBuffers(h)
}

// confirmParentInRoot forces the immediate rooted parent of `name` (and thus `name`'s
// already-present entry) durable with a REAL writable-handle FlushFileBuffers: it resolves
// the parent's validated real path via os.Root (symlink-safe), captures its identity, and
// flushes a fresh no-share-delete handle proven to be the SAME object (guarding a swap
// between resolution and the flush). Re-runnable and idempotent — used on every
// re-confirm/recovery path and after an own-move-visible create error.
func confirmParentInRoot(root *os.Root, name string) error {
	realParent, pf, err := openRealParent(root, name)
	if err != nil {
		return err
	}
	defer pf.Close()
	want, ierr := identOf(windows.Handle(pf.Fd()))
	if ierr != nil {
		return ierr
	}
	return flushDirIdent(realParent, want)
}

// resolveDirWriteThroughFailure classifies a failed no-clobber write-through DIRECTORY
// publish of `name`, given a parent already pinned by the caller. A raced-in non-directory
// (file/symlink) is ErrNotDirectory — NOT *PostCommitSyncError, whose Committed() would
// falsely assert OUR effect is visible. A visible directory — whether a concurrent winner or
// our own move — is not assumed durable: the REAL parent flush is RUN before accepting it
// (nil on success, *PostCommitSyncError on barrier failure). A target not visible is
// uncommitted (raw error).
func resolveDirWriteThroughFailure(root *os.Root, name string, merr error) error {
	info, serr := root.Lstat(name)
	if serr != nil {
		return merr // not visible → the move did not commit (uncommitted / resolved race)
	}
	if !info.IsDir() {
		return ErrNotDirectory // a raced-in file/symlink occupies the name — not our effect
	}
	if berr := confirmParentInRoot(root, name); berr != nil {
		return &PostCommitSyncError{Path: name, Err: berr}
	}
	return nil
}

// publishDirInRoot durably creates a fresh directory within the confined root. It resolves
// the validated real parent, PINS it with a no-share-delete handle across the create+rename
// (so it cannot be renamed/replaced mid-operation), creates a temp sibling and renames it to
// the target leaf with MOVEFILE_WRITE_THROUGH (no REPLACE_EXISTING → no-clobber). A failed
// move is classified by resolveDirWriteThroughFailure.
func publishDirInRoot(root *os.Root, name string, perm os.FileMode) error {
	realParent, pf, err := openRealParent(root, name)
	if err != nil {
		return err
	}
	defer pf.Close()
	want, ierr := identOf(windows.Handle(pf.Fd()))
	if ierr != nil {
		return ierr
	}
	hold, err := openDirNoDelete(realParent, windows.FILE_LIST_DIRECTORY|windows.SYNCHRONIZE, want)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(hold)
	tmp, err := os.MkdirTemp(realParent, tmpPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // a no-op once tmp is renamed to the target
	_ = os.Chmod(tmp, perm)
	if merr := moveFileExPath(tmp, realParent+`\`+path.Base(name), dirPublishFlags); merr != nil {
		return resolveDirWriteThroughFailure(root, name, merr)
	}
	return nil
}

// publishFileInRoot durably publishes an already-written temp file to `name` within the
// confined root via a MOVEFILE_WRITE_THROUGH no-clobber rename (no REPLACE_EXISTING), so the
// file's directory ENTRY is power-safe. Returns whether OUR temp was consumed (renamed into
// place). A pre-existing target is an fs.ErrExist no-clobber conflict; transient
// sharing/access errors are retried. Consumption is decided by inspecting the UNIQUE SOURCE
// temp, never target visibility alone.
func publishFileInRoot(root *os.Root, tmp, name, dir string) (bool, error) {
	realParent, pf, err := openRealParent(root, name)
	if err != nil {
		return false, err
	}
	defer pf.Close()
	want, ierr := identOf(windows.Handle(pf.Fd()))
	if ierr != nil {
		return false, ierr
	}
	// Pin the parent (no-share-delete, same object) across the rename so it cannot be
	// renamed/replaced mid-operation.
	hold, herr := openDirNoDelete(realParent, windows.FILE_LIST_DIRECTORY|windows.SYNCHRONIZE, want)
	if herr != nil {
		return false, herr
	}
	defer windows.CloseHandle(hold)
	tmpAbs := realParent + `\` + path.Base(tmp)
	target := realParent + `\` + path.Base(name)
	merr := replaceWith(
		func() error { return moveFileExPath(tmpAbs, target, dirPublishFlags) },
		func() { time.Sleep(renameBackoff) },
		isTransientRename,
	)
	if merr == nil {
		return true, nil // renamed durably (our source consumed)
	}
	if isAlreadyExists(merr) {
		return false, fs.ErrExist // no-clobber conflict; our source not consumed
	}
	if _, serr := root.Lstat(name); serr != nil {
		return false, merr // target not visible → uncommitted; our source not consumed
	}
	// Target visible: decide consumption from OUR unique source temp, not target visibility.
	if _, serr := root.Lstat(tmp); serr == nil {
		// Source still present → the visible target is a conflict/foreign winner, not ours;
		// report a no-clobber conflict WITHOUT leaking our temp (caller removes it).
		return false, fs.ErrExist
	}
	// Source absent + target visible → OUR move consumed it; durability unconfirmed, so force
	// the parent metadata durable with the real barrier.
	if berr := confirmParentInRoot(root, name); berr != nil {
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

// ensureDirDurableImpl durably publishes a fresh directory on Windows. A missing dir is
// published via a write-through rename; an EXISTING dir is RE-confirmed durable via the real
// parent-metadata flush (never a swallowed no-op), so a retry after a prior partial failure
// re-forces the entry.
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

// flushDirByPath is the PATH-based directory-metadata barrier seam: it opens dir writable and
// no-share-delete and FlushFileBuffers it. It does NOT identity-pin, because the genstore
// store dirs it serves are trusted internal paths created and mutated under the run lock (not
// attacker-influenced), so a swap is not in scope; it still drops FILE_SHARE_DELETE on the
// working handle. Behind a package var so a test can drive a fail-once-then-succeed retry.
var flushDirByPath = flushDirByPathImpl

func flushDirByPathImpl(dir string) error {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(
		p,
		windows.FILE_APPEND_DATA|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, // NO FILE_SHARE_DELETE
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.FlushFileBuffers(h)
}

// parentBarrier forces the immediate parent of dir (and thus dir's already-present entry)
// durable with a REAL writable-handle FlushFileBuffers. The path-based analogue of
// confirmParentInRoot (trusted internal paths, no identity pin — see flushDirByPath).
func parentBarrier(dir string) error {
	return flushDirByPath(filepath.Dir(dir))
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
		// A raced-in non-directory (file/symlink) is ErrNotDirectory — not *PostCommitSyncError
		// (whose Committed() would falsely assert OUR effect is visible). A visible directory,
		// whether a concurrent winner or our own move, is not assumed durable: RUN the real
		// parent barrier before accepting it. Target not visible → uncommitted (raw error).
		fi, serr := os.Lstat(dir)
		if serr != nil {
			return merr
		}
		if !fi.IsDir() {
			return ErrNotDirectory
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

// syncRootDir is a best-effort READ-ONLY directory sync within the confined root (os.Root
// opens the directory read-only, so its FlushFileBuffers is refused and swallowed — see
// swallowDirFlush). It is NOT the entry barrier: ConfirmParentInRoot flushes a WRITABLE
// parent handle to force an entry durable. syncRootDir remains only in the mutable
// ReplaceInRoot path (a rebuildable projection whose entry durability is not required).
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

// swallowDirFlush treats Windows's refusal to flush a READ-ONLY directory handle
// (ERROR_ACCESS_DENIED from FlushFileBuffers) as satisfied. FlushFileBuffers needs WRITE
// access; the real entry-durability barrier (ParentBarrier / confirmParentInRoot) opens the
// parent with FILE_APPEND_DATA and DOES flush it. This helper backs only the intentionally
// read-only best-effort syncDir/syncRootDir, used where entries are independently durable
// (write-through moves) or for a content re-sync — not to force an entry. Any error other
// than the read-only refusal is a real failure and is returned.
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

// syncDir is a best-effort READ-ONLY directory sync (Go opens the directory read-only, so
// its FlushFileBuffers is refused and swallowed — see swallowDirFlush). It is NOT the entry
// barrier: to force a directory's entry durable, ParentBarrier flushes a WRITABLE handle.
// syncDir is used only where entries are independently write-through-durable or for a content
// re-sync.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	_ = d.Close()
	return swallowDirFlush(serr)
}
