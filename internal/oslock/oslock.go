// Package oslock provides an exclusive advisory lock backed by an OS primitive
// that the kernel releases automatically when the holding process exits.
//
// That automatic release is the crash-stale reclaim mechanism: a lock left by a
// crashed claudex process is reclaimable by the next one without any PID
// guessing or timeout heuristics (D015). The lock is advisory and only
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
