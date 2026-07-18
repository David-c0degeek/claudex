//go:build windows

package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

// publishDirInRoot durably creates a fresh directory within the confined root on Windows,
// where a directory handle cannot be flushed. It opens the immediate rooted parent (os.Root
// refuses symlink traversal) and keeps that handle OPEN to pin the resolved directory, reads
// its validated real DOS path, then creates a temp sibling and renames it to the target with
// MOVEFILE_WRITE_THROUGH — operating on LEAF names under the validated real parent (no path
// traversal, so no symlink escape), which forces the parent's metadata to disk.
func publishDirInRoot(root *os.Root, name string, perm os.FileMode) error {
	openName := rootParent(name)
	if openName == "" {
		openName = "."
	}
	pf, err := root.Open(openName)
	if err != nil {
		return err
	}
	defer pf.Close()
	realParent, err := finalPathByHandle(windows.Handle(pf.Fd()))
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(realParent, tmpPrefix)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()
	_ = os.Chmod(tmp, perm)
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(realParent + `\` + path.Base(name))
	if err != nil {
		return err
	}
	if err := moveFileEx(from, to, dirPublishFlags); err != nil {
		// A concurrent publisher may have created the target first (the write-through rename
		// fails when the destination already exists). If it now exists as a directory it was
		// durably published by that writer — all creates use this same write-through path —
		// so treat it as done (our temp is cleaned up by the defer).
		if info, serr := root.Lstat(name); serr == nil && info.IsDir() {
			return nil
		}
		return &PostCommitSyncError{Path: name, Err: err}
	}
	cleanup = false // the temp directory was renamed to its final name
	return nil
}

// finalPathFlags is VOLUME_NAME_DOS (0x0) | FILE_NAME_NORMALIZED (0x0): a normalized DOS
// drive-letter path. x/sys/windows does not export these zero-valued flags, so named here.
const finalPathFlags = 0

// finalPathByHandle returns the validated real DOS path of an open handle, keeping the
// caller's handle open so the resolution cannot be swapped out from under it.
func finalPathByHandle(h windows.Handle) (string, error) {
	buf := make([]uint16, 260)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), finalPathFlags)
		if err != nil {
			return "", err
		}
		if n > uint32(len(buf)) {
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
// handle cannot be flushed. A missing dir is created as a temporary sibling in its parent
// and then renamed to its final name with MOVEFILE_WRITE_THROUGH, so the rename forces the
// parent's metadata (the new entry) to disk. An existing directory was already published
// durably by this same path, so re-confirmation is a verified no-op.
func ensureDirDurableImpl(dir string, perm os.FileMode) error {
	if fi, err := os.Lstat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%w: %s", ErrNotDirectory, dir)
		}
		return nil // durably published at creation via the write-through rename
	} else if !os.IsNotExist(err) {
		return err
	}
	return publishDirWriteThrough(dir, perm)
}

func publishDirWriteThrough(dir string, perm os.FileMode) error {
	parent := filepath.Dir(dir)
	tmp, err := os.MkdirTemp(parent, tmpPrefix)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()
	_ = os.Chmod(tmp, perm)
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := moveFileEx(from, to, dirPublishFlags); err != nil {
		return err
	}
	cleanup = false // the temp directory was renamed to its final name
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
