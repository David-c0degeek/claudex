package oslock

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestLockFamiliesDoNotContend documents the hazard that made "one primitive for acquire and probe" a
// requirement rather than a preference.
//
// flock and OFD (F_OFD_SETLK) locks are independent: a holder of one does not block a taker of the
// other. So a probe written against the wrong family would report a live attempt lease as unheld, and
// the gate would start a second attempt over a running one. This test exists to fail loudly if anyone
// substitutes one for the other — it asserts the NON-contention directly, so the reason the codebase
// insists on a single family is recorded as executable evidence rather than a comment.
func TestLockFamiliesDoNotContend(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	held, ok, err := TryAcquireInRoot(root, "lease")
	if err != nil || !ok {
		t.Fatalf("TryAcquireInRoot: ok=%v err=%v", ok, err)
	}
	defer held.Release()

	// The shipped probe, same family: contends, reports held.
	isHeld, err := ProbeInRoot(root, "lease")
	if err != nil {
		t.Fatalf("same-family probe: %v", err)
	}
	if !isHeld {
		t.Fatal("the shipped probe did not see the shipped lock")
	}

	// A probe using the OTHER family on a distinct open file description.
	other, err := root.OpenFile("lease", os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for the cross-family probe: %v", err)
	}
	defer other.Close()
	fl := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 1}
	if err := unix.FcntlFlock(other.Fd(), unix.F_OFD_SETLK, &fl); err != nil {
		// Only a genuinely unsupported kernel or filesystem is a skip. CONTENTION is the opposite of
		// a reason to skip: it means the shipped family has become OFD, which is exactly the silent
		// substitution this guard exists to catch — and converting every error into a skip would have
		// turned this test green-by-omission at the moment it mattered.
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
			t.Fatal("an OFD probe CONTENDED with the shipped lease lock: the shipped family appears to " +
				"have been changed to OFD, so acquire and probe are no longer the single family the " +
				"design requires")
		}
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("OFD locks unsupported here: %v", err)
		}
		t.Fatalf("unexpected error from the cross-family probe: %v", err)
	}
	// It SUCCEEDED against a lease that is genuinely held. That is the whole point: mixing families
	// silently reports a running attempt as free.
	fl.Type = unix.F_UNLCK
	if err := unix.FcntlFlock(other.Fd(), unix.F_OFD_SETLK, &fl); err != nil {
		t.Fatalf("release cross-family probe: %v", err)
	}
	t.Log("confirmed: an OFD probe does not contend with the flock-held lease, so the families are not interchangeable")
}
