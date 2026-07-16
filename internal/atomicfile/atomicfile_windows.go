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

// replace renames oldpath onto newpath. os.Rename uses MoveFileEx with
// MOVEFILE_REPLACE_EXISTING (not guaranteed atomic on Windows — see the package
// doc). It retries only the transient sharing/access errors that occur when
// another process momentarily has the target open.
func replace(oldpath, newpath string) error {
	return replaceWith(
		func() error { return os.Rename(oldpath, newpath) },
		func() { time.Sleep(renameBackoff) },
		isTransientRename,
	)
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

// syncDir is a no-op on Windows: there is no directory fsync. Power-loss
// durability would additionally require MOVEFILE_WRITE_THROUGH, which the
// default os.Rename does not request.
func syncDir(string) error {
	return nil
}
