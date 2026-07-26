//go:build !windows

package oslock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// acquire takes a non-blocking exclusive flock. The lock is tied to the open
// file description and is released by the kernel when the process exits.
func acquire(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	return lockOpenFile(f)
}

// lockOpenFile takes the lock on an ALREADY-OPEN file, so a caller that opened it through a confined
// root gets the same primitive as one that opened it by path. Using one primitive for both is the
// point: flock and OFD locks do not contend with each other, so a probe written against one would
// report a lease held by the other as unheld.
func lockOpenFile(f *os.File) (*os.File, bool, error) {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		// EWOULDBLOCK and EAGAIN both signal "already locked" portably.
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

func release(f *os.File) error {
	unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return errors.Join(unlockErr, f.Close())
}
