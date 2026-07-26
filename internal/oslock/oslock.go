// Package oslock provides an exclusive advisory lock backed by an OS primitive
// that the kernel releases automatically when the holding process exits.
//
// That automatic release is the crash-stale reclaim mechanism: a lock left by a
// crashed claudex process is reclaimable by the next one without any PID
// guessing or timeout heuristics. The lock is advisory and only
// meaningful on a local filesystem.
//
// TryAcquire is non-blocking: it either takes the lock or reports that another
// process holds it. The coordinator's mutation path uses this so a long-running
// wait never blocks a status read.
package oslock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Lock is a held advisory lock. Release it exactly once.
type Lock struct {
	f *os.File
}

// TryAcquire attempts to take an exclusive lock on path (creating it if needed).
// It returns (lock, true, nil) when acquired, (nil, false, nil) when another
// process already holds it, and (nil, false, err) on an unexpected error.
func TryAcquire(path string) (*Lock, bool, error) {
	f, ok, err := acquire(path)
	if err != nil || !ok {
		return nil, ok, err
	}
	return &Lock{f: f}, true, nil
}

// Release releases the lock and closes its handle. Closing alone would also
// release the OS lock; Release unlocks explicitly first for clarity.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := release(l.f)
	l.f = nil
	return err
}

// TryAcquireInRoot takes the lock on name inside a confined root.
//
// The attempt lease is opened this way rather than by path so the destination cannot be caller-chosen:
// the supervisor holds a handle to exactly one attempt directory. It does NOT prevent the entry being
// renamed inside that directory — an earlier comment claimed it did, and that was wrong. Detecting a
// displaced lease is the probe's job, below. It uses the same primitive as TryAcquire, which is not a detail — flock and OFD locks do
// not contend with each other, so mixing families would let a probe report a live lease as unheld and
// a second attempt would start over a running one.
//
// On Unix the descriptor is close-on-exec by default, which is what the lease requires: a task that
// inherited it would hold the lock past the supervisor's death, and recovery would read a live owner
// that does not exist.
func TryAcquireInRoot(root *os.Root, name string) (*Lock, bool, error) {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	lf, ok, lerr := lockOpenFile(f)
	if lerr != nil || !ok {
		return nil, ok, lerr
	}
	return &Lock{f: lf}, true, nil
}

var (
	// ErrLeaseMissing means the lease entry does not exist. After arming created it, absence is not a
	// free lease: it means the entry was removed or RENAMED while its lock stayed live under the new
	// name, so treating absence as "nobody is running" would start a second attempt over a running one.
	ErrLeaseMissing = errors.New("oslock: lease entry is missing")
	// ErrLeaseNotRegular means something other than a regular file occupies the lease name.
	ErrLeaseNotRegular = errors.New("oslock: lease entry is not a regular file")
)

// ProbeInRoot reports whether name is currently locked, without holding it and without creating it.
//
// The contract is deliberately asymmetric, because the two answers carry different risk. It opens the
// EXISTING entry — never O_CREATE — takes the lock non-blocking on a distinct open file description,
// treats contention as held, and on success releases immediately, having proven only that nobody held
// it AT THAT INSTANT.
//
// Not creating is the point of the split. Sharing the acquiring open meant a probe of a lease that had
// been renamed away CREATED a fresh empty file at the original name and locked that instead, reporting
// `unheld` while the original lock was still live under its new name. The probe was answering a
// question about a file it had just made.
//
// The Lstat above already rejects an absent entry, so dropping O_CREATE is not separately visible to
// the tests — and it is still load-bearing, because it closes the window Lstat cannot: an entry
// renamed away BETWEEN the Lstat and the open would otherwise be recreated here.
//
// On ANY error it returns held=true alongside the error. That is not a guess dressed as a fact: a
// caller that failed to check the error would otherwise treat "we could not tell" as "nobody is
// running". Failing closed in the return value itself makes the safe reading the default rather than a
// discipline the next caller has to remember.
func ProbeInRoot(root *os.Root, name string) (held bool, err error) {
	// Lstat first, so a symlink or other substitute is rejected by TYPE rather than followed to
	// whatever it points at; O_NOFOLLOW then covers a swap racing this check.
	li, serr := root.Lstat(name)
	if serr != nil {
		if errors.Is(serr, fs.ErrNotExist) {
			return true, fmt.Errorf("%w: %s", ErrLeaseMissing, name)
		}
		return true, serr
	}
	if !li.Mode().IsRegular() {
		return true, fmt.Errorf("%w: %s", ErrLeaseNotRegular, name)
	}
	f, oerr := root.OpenFile(name, os.O_RDWR|probeOpenExtraFlags, 0)
	if oerr != nil {
		if errors.Is(oerr, fs.ErrNotExist) {
			return true, fmt.Errorf("%w: %s", ErrLeaseMissing, name)
		}
		return true, oerr
	}
	lf, ok, lerr := lockOpenFile(f)
	if lerr != nil {
		return true, lerr
	}
	if !ok {
		return true, nil
	}
	if rerr := (&Lock{f: lf}).Release(); rerr != nil {
		return true, rerr
	}
	return false, nil
}
