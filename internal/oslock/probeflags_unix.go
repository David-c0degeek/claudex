//go:build !windows

package oslock

import "syscall"

// probeOpenExtraFlags hardens the probe's open on POSIX. O_NOFOLLOW refuses a symlink that was swapped
// in after the Lstat check, and O_NONBLOCK keeps a FIFO from blocking the open itself.
const probeOpenExtraFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
