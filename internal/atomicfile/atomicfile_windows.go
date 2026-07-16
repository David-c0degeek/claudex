//go:build windows

package atomicfile

import (
	"os"
	"time"
)

// replace renames oldpath onto newpath. os.Rename uses MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, a crash-atomic replacement. Windows can transiently
// fail with a sharing violation while another process momentarily has the target
// open, so the rename is retried with a short bounded backoff.
func replace(oldpath, newpath string) error {
	const attempts = 20
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.Rename(oldpath, newpath); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

// syncDir is a no-op on Windows: there is no directory fsync. MoveFileEx gives
// crash-atomic replacement; power-loss durability would additionally require
// MOVEFILE_WRITE_THROUGH (see docs/decisions.md D015), which the default
// os.Rename does not request.
func syncDir(string) error {
	return nil
}
