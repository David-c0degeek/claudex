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
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

func release(f *os.File) error {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return f.Close()
}
