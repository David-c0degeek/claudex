package oslock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestAcquireRefusesASymlinkedLease is the case that let arming lock the wrong object.
//
// A bare rooted open follows an in-root symlink, so arming would report success while holding a lock
// on a substitute, proceed toward the active CAS, and only then be rejected by the strict probe —
// which would be refusing the very state arming had created.
func TestAcquireRefusesASymlinkedLease(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target"), nil, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "lease")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	l, ok, err := TryAcquireInRoot(root, "lease")
	if err == nil {
		if ok {
			_ = l.Release()
		}
		t.Fatal("TryAcquireInRoot followed and locked a symlinked substitute for the lease")
	}
	if !errors.Is(err, ErrLeaseNotRegular) {
		t.Fatalf("err = %v, want ErrLeaseNotRegular", err)
	}
}

// TestPostOpenSwapIsRejected pins the window that Lstat and O_NOFOLLOW cannot cover.
//
// Neither flag says anything about a different REGULAR inode being swapped in after the type check:
// the open succeeds on the replacement while the original lease stays locked, and the probe would
// report `unheld` about an object that was never the lease. The swap is performed through a test seam
// rather than by racing, because a timing loop that happens to pass proves nothing about the guard.
func TestPostOpenSwapIsRejected(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	// Arm the lease, then hold it so the "original stays locked" half is real.
	held, ok, err := TryAcquireInRoot(root, "lease")
	if err != nil || !ok {
		t.Fatalf("arm: ok=%v err=%v", ok, err)
	}
	defer held.Release()

	// A different regular file that will take the lease's place mid-probe.
	if err := os.WriteFile(filepath.Join(dir, "replacement"), nil, 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	swapped := false
	afterOpenHook = func(name string) {
		if swapped {
			return
		}
		swapped = true
		// Replace the entry AFTER it was opened: the descriptor still refers to the real lease, but
		// the name no longer does.
		if rerr := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, name)); rerr != nil {
			t.Errorf("swap: %v", rerr)
		}
	}
	t.Cleanup(func() { afterOpenHook = nil })

	isHeld, err := ProbeInRoot(root, "lease")
	if !swapped {
		t.Fatal("the seam never fired; the test proved nothing")
	}
	if !errors.Is(err, ErrLeaseDisplaced) {
		t.Fatalf("err = %v, want ErrLeaseDisplaced", err)
	}
	if !isHeld {
		t.Fatal("a displaced lease was reported unheld; the original lock is still live")
	}
}

// TestAcquireRejectsAPostOpenSwap: arming must not proceed to the CAS holding a lock on an object the
// authoritative name no longer refers to.
func TestAcquireRejectsAPostOpenSwap(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(dir, "replacement"), nil, 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	swapped := false
	afterOpenHook = func(name string) {
		if swapped {
			return
		}
		swapped = true
		if rerr := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, name)); rerr != nil {
			t.Errorf("swap: %v", rerr)
		}
	}
	t.Cleanup(func() { afterOpenHook = nil })

	l, ok, err := TryAcquireInRoot(root, "lease")
	if !swapped {
		t.Fatal("the seam never fired; the test proved nothing")
	}
	if err == nil {
		if ok {
			_ = l.Release()
		}
		t.Fatal("arming succeeded while holding a lock on a displaced object")
	}
	if !errors.Is(err, ErrLeaseDisplaced) {
		t.Fatalf("err = %v, want ErrLeaseDisplaced", err)
	}
}

// TestProbeReconfirmsAfterLocking covers the window the open-time seam cannot reach.
//
// A swap during the open is caught by the in-open identity check, so a test using only that seam
// leaves the post-lock re-confirmation completely uncovered — a mutation run proved exactly that by
// deleting the re-confirmation without any test noticing. Here the swap happens AFTER the lock is
// taken, which is the case the second check exists for: everything earlier proved only that the object
// was right WHEN IT WAS OPENED.
func TestProbeReconfirmsAfterLocking(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	arm, ok, err := TryAcquireInRoot(root, "lease")
	if err != nil || !ok {
		t.Fatalf("arm: ok=%v err=%v", ok, err)
	}
	if err := arm.Release(); err != nil {
		t.Fatalf("release arm: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "replacement"), nil, 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	swapped := false
	afterLockHook = func(name string) {
		if swapped {
			return
		}
		swapped = true
		if rerr := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, name)); rerr != nil {
			t.Errorf("swap: %v", rerr)
		}
	}
	t.Cleanup(func() { afterLockHook = nil })

	held, err := ProbeInRoot(root, "lease")
	if !swapped {
		t.Fatal("the post-lock seam never fired; the test proved nothing")
	}
	if !errors.Is(err, ErrLeaseDisplaced) {
		t.Fatalf("err = %v, want ErrLeaseDisplaced", err)
	}
	if !held {
		t.Fatal("a lease displaced after locking was reported unheld")
	}
}

// TestAcquireReconfirmsAfterLocking is the acquire half of the post-lock window.
//
// The previous revision added the re-confirmation to the probe only, so arming could end up HOLDING
// inode A while the lease name referred to inode B: it would report success, proceed to READY and the
// active CAS, and a later probe of B would answer `unheld` while the supervisor was still live. The
// same seam that pins the probe window pins this one, because the two now share one implementation.
func TestAcquireReconfirmsAfterLocking(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(dir, "replacement"), nil, 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	swapped := false
	afterLockHook = func(name string) {
		if swapped {
			return
		}
		swapped = true
		// The lock is already held on the original object here; the NAME is what moves.
		if rerr := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, name)); rerr != nil {
			t.Errorf("swap: %v", rerr)
		}
	}
	t.Cleanup(func() { afterLockHook = nil })

	l, ok, err := TryAcquireInRoot(root, "lease")
	if !swapped {
		t.Fatal("the post-lock seam never fired; the test proved nothing")
	}
	if err == nil {
		if ok {
			_ = l.Release()
		}
		t.Fatal("arming reported success while holding a lock on a displaced object")
	}
	if !errors.Is(err, ErrLeaseDisplaced) {
		t.Fatalf("err = %v, want ErrLeaseDisplaced", err)
	}
	if ok {
		t.Fatal("arming reported acquired on a displacement")
	}
}
