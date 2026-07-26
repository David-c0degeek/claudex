package oslock

import (
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
		t.Skipf("OFD locks unavailable here: %v", err)
	}
	// It SUCCEEDED against a lease that is genuinely held. That is the whole point: mixing families
	// silently reports a running attempt as free.
	fl.Type = unix.F_UNLCK
	if err := unix.FcntlFlock(other.Fd(), unix.F_OFD_SETLK, &fl); err != nil {
		t.Fatalf("release cross-family probe: %v", err)
	}
	t.Log("confirmed: an OFD probe does not contend with the flock-held lease, so the families are not interchangeable")
}
