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

import "os"

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
// The attempt lease is opened this way rather than by path so the destination cannot be caller-chosen
// and cannot be redirected by a rename: the supervisor holds a handle to exactly one attempt
// directory. It uses the same primitive as TryAcquire, which is not a detail — flock and OFD locks do
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

// ProbeInRoot reports whether name is currently locked, without holding it.
//
// The contract is deliberately asymmetric, because the two answers carry different risk. It takes the
// lock non-blocking on a DISTINCT open file description; contention means held; success proves only
// that nobody held it AT THAT INSTANT, so the lock is released immediately.
//
// On ANY error it returns held=true alongside the error. That is not a guess dressed as a fact: a
// caller that failed to check the error would otherwise treat "we could not tell" as "nobody is
// running" and start a second attempt over a live one. Failing closed in the return value itself makes
// the safe reading the default rather than a discipline.
func ProbeInRoot(root *os.Root, name string) (held bool, err error) {
	l, ok, perr := TryAcquireInRoot(root, name)
	if perr != nil {
		return true, perr
	}
	if !ok {
		return true, nil
	}
	if rerr := l.Release(); rerr != nil {
		return true, rerr
	}
	return false, nil
}
