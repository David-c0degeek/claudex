//go:build windows

package oslock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// acquire takes an immediate (non-blocking) exclusive byte-range lock via
// LockFileEx. The lock is released by the OS when the handle closes, which
// happens when the process exits.
func acquire(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	return lockOpenFile(f)
}

// lockOpenFile takes the lock on an ALREADY-OPEN file, so a caller that opened it through a confined
// root gets the same primitive as one that opened it by path. One primitive for both is the point: a
// probe using a different locking family would not contend with the holder and would report a live
// lease as unheld.
func lockOpenFile(f *os.File) (*os.File, bool, error) {
	var ol windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &ol,
	)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

func release(f *os.File) error {
	var ol windows.Overlapped
	unlockErr := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
	return errors.Join(unlockErr, f.Close())
}
