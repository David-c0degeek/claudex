//go:build windows

package atomicfile

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// maxRenameAttempts bounds the transient-error retry loop on Windows.
const maxRenameAttempts = 20

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

// moveFileWriteThrough is the durable MoveFileEx used by replace. Isolated so a test
// can assert the write-through flag path moves a file.
func moveFileWriteThrough(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
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
// (ERROR_ACCESS_DENIED from FlushFileBuffers) as satisfied: there is no user-space
// directory-metadata flush on Windows. Directory-entry durability rests instead on
// MOVEFILE_WRITE_THROUGH renames (replace) and NTFS metadata journaling. Any other
// error is a real durability failure and is returned.
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

// syncDir flushes the directory `dir` so entries created or renamed inside it are
// durable. Go opens a directory with FILE_FLAG_BACKUP_SEMANTICS, so Sync issues
// FlushFileBuffers on the handle; Windows refuses to flush a directory handle with
// ERROR_ACCESS_DENIED, treated as satisfied (see swallowDirFlush). The load-bearing
// durability on Windows is the MOVEFILE_WRITE_THROUGH rename in replace.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	_ = d.Close()
	return swallowDirFlush(serr)
}
