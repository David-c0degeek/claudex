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

// afterOpenHook is a test seam, nil in production.
//
// The post-open identity check guards a race that is otherwise impossible to schedule from a test.
// This lets a fixture perform the swap at exactly the moment the guard exists for, so the guard is
// proven rather than asserted — the alternative is a timing loop that passes for the wrong reason.
var afterOpenHook func(name string)

// afterLockHook is a second test seam, nil in production.
//
// It exists because the open-time seam cannot reach the window the post-lock re-confirmation guards:
// a swap performed during the open is caught by the in-open identity check, so a test using only that
// seam leaves the later check entirely uncovered — which a mutation run duly proved.
var afterLockHook func(name string)

// openLeaseInRoot opens the lease entry with the full rooted-identity discipline.
//
// The sequence matters, and each step covers something the others cannot:
//
//   - Lstat first (when not creating), so a directory, FIFO, device or symlink is rejected by TYPE
//     before anything is opened — opening a device can itself have side effects.
//   - O_NOFOLLOW and O_NONBLOCK harden the open, but note what O_NOFOLLOW does NOT do here: os.Root
//     resolves in-root symlinks itself, component by component, so the flag applies to the object it
//     already resolved to rather than to the link. A symlinked lease is caught by the identity check
//     below, not by the flag — this was measured, not assumed, and it is why the check is mandatory
//     rather than defence in depth.
//   - A post-open Stat, because neither flag covers a different REGULAR inode swapped in after the
//     Lstat: the open would succeed on the replacement while the original stayed locked.
//   - os.SameFile between the name as it stands NOW and the object actually opened, which is the only
//     check that ties the descriptor back to the authoritative name.
func openLeaseInRoot(root *os.Root, name string, create bool) (*os.File, error) {
	if !create {
		li, err := root.Lstat(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s", ErrLeaseMissing, name)
			}
			return nil, err
		}
		if !li.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: %s", ErrLeaseNotRegular, name)
		}
	}

	flag := os.O_RDWR | probeOpenExtraFlags
	if create {
		flag |= os.O_CREATE
	}
	f, err := root.OpenFile(name, flag, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrLeaseMissing, name)
		}
		// O_NOFOLLOW reports ELOOP for a symlink; surface it as the type error it is.
		if isSymlinkOpenErr(err) {
			return nil, fmt.Errorf("%w: %s is a symlink", ErrLeaseNotRegular, name)
		}
		return nil, err
	}
	if afterOpenHook != nil {
		afterOpenHook(name)
	}
	if err := confirmLeaseIdentity(root, name, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// confirmLeaseIdentity proves the opened descriptor is still what the authoritative name refers to.
func confirmLeaseIdentity(root *os.Root, name string, f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	// Subsumed by the Lstat regularity check below once SameFile ties the two together, so this is
	// defence in depth rather than an independent guard, and is recorded as such rather than counted.
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrLeaseNotRegular, name)
	}
	li, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s was displaced after it was opened", ErrLeaseMissing, name)
		}
		return err
	}
	if !li.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrLeaseNotRegular, name)
	}
	if !os.SameFile(li, fi) {
		return fmt.Errorf("%w: %s no longer refers to the object that was opened", ErrLeaseDisplaced, name)
	}
	return nil
}

// TryAcquireInRoot takes the lock on name inside a confined root, creating it if needed.
//
// It uses the SAME opening discipline as the probe. An earlier version used a bare rooted open, so
// arming happily followed a symlink and locked the wrong object — reporting success and proceeding
// toward the active CAS while holding a lock on a substitute, after which the strict probe rejected
// the very state arming had created.
//
// One primitive for acquire and probe is also the point: flock and OFD locks do not contend, so a
// probe against the wrong family would report a live lease as unheld and a second attempt would start
// over a running one.
//
// On Unix the descriptor is close-on-exec by default, which is what the lease requires: a task that
// inherited it would hold the lock past the supervisor's death, and recovery would read a live owner
// that does not exist.
func TryAcquireInRoot(root *os.Root, name string) (*Lock, bool, error) {
	f, err := openLeaseInRoot(root, name, true)
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
	// ErrLeaseDisplaced means the name stopped referring to the object that was opened. It is distinct
	// from Missing because the failure is an identity mismatch, not an absence: the original lease can
	// still be locked under another name while a replacement occupies this one.
	ErrLeaseDisplaced = errors.New("oslock: lease entry was displaced after it was opened")
)

// ProbeInRoot reports whether name is currently locked, without holding it and without creating it.
//
// The contract is deliberately asymmetric, because the two answers carry different risk. It opens the
// EXISTING entry — never O_CREATE — takes the lock non-blocking on a distinct open file description,
// treats contention as held, and on success releases immediately, having proven only that nobody held
// it AT THAT INSTANT.
//
// Not creating is the point of the acquire/probe split. Sharing the acquiring open meant a probe of a
// lease that had been renamed away CREATED a fresh empty file at the original name and locked that
// instead, reporting `unheld` while the original lock was still live under its new name. The probe was
// answering a question about a file it had just made.
//
// Before reporting `unheld` it re-confirms identity a second time. Everything up to that point proves
// only that the object it locked was the right one WHEN IT OPENED IT; a rename in the interim would
// otherwise reintroduce the displaced-lease answer through a narrower window.
//
// On ANY error it returns held=true alongside the error. That is not a guess dressed as a fact: a
// caller that failed to check the error would otherwise treat "we could not tell" as "nobody is
// running". Failing closed in the return value itself makes the safe reading the default rather than a
// discipline the next caller has to remember.
func ProbeInRoot(root *os.Root, name string) (held bool, err error) {
	f, oerr := openLeaseInRoot(root, name, false)
	if oerr != nil {
		return true, oerr
	}
	lf, ok, lerr := lockOpenFile(f)
	if lerr != nil {
		return true, lerr
	}
	if !ok {
		return true, nil
	}
	l := &Lock{f: lf}
	if afterLockHook != nil {
		afterLockHook(name)
	}
	// Re-confirmed while still HOLDING the lock: releasing first would leave a window in which the
	// answer is decided by an object nobody holds.
	if cerr := confirmLeaseIdentity(root, name, lf); cerr != nil {
		_ = l.Release()
		return true, cerr
	}
	if rerr := l.Release(); rerr != nil {
		return true, rerr
	}
	return false, nil
}
