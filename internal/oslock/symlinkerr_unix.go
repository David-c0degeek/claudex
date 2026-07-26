//go:build !windows

package oslock

import (
	"errors"
	"syscall"
)

// isSymlinkOpenErr reports the error O_NOFOLLOW produces when the final component is a symlink.
func isSymlinkOpenErr(err error) bool { return errors.Is(err, syscall.ELOOP) }
