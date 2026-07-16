//go:build windows

package atomicfile

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// replace renames oldpath onto newpath. os.Rename uses MoveFileEx with
// MOVEFILE_REPLACE_EXISTING. Go does not guarantee this is atomic on Windows
// (see the package doc); it retries only the transient sharing/access errors
// that occur when another process momentarily has the target open, and returns
// immediately on any other (permanent) error.
func replace(oldpath, newpath string) error {
	const attempts = 20
	for i := 0; ; i++ {
		err := os.Rename(oldpath, newpath)
		if err == nil {
			return nil
		}
		if i >= attempts-1 || !isTransientRename(err) {
			return err
		}
		time.Sleep(renameBackoff)
	}
}

func isTransientRename(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// syncDir is a no-op on Windows: there is no directory fsync. Power-loss
// durability would additionally require MOVEFILE_WRITE_THROUGH (docs/decisions.md
// D015), which the default os.Rename does not request.
func syncDir(string) error {
	return nil
}
