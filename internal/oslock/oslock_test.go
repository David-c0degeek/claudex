package oslock

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireReleaseReacquire(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")

	l, ok, err := TryAcquire(p)
	if err != nil || !ok {
		t.Fatalf("first TryAcquire: ok=%v err=%v", ok, err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l2, ok, err := TryAcquire(p)
	if err != nil || !ok {
		t.Fatalf("re-acquire after release: ok=%v err=%v", ok, err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("Release 2: %v", err)
	}
}

func TestContentionIsRejected(t *testing.T) {
	// Each TryAcquire opens its own handle / file description, so a second
	// acquire while the first is held fails immediately on both platforms.
	p := filepath.Join(t.TempDir(), "run.lock")

	l1, ok, err := TryAcquire(p)
	if err != nil || !ok {
		t.Fatalf("first TryAcquire: ok=%v err=%v", ok, err)
	}

	l2, ok, err := TryAcquire(p)
	if err != nil {
		t.Fatalf("second TryAcquire errored: %v", err)
	}
	if ok {
		l2.Release()
		l1.Release()
		t.Fatalf("second TryAcquire succeeded while lock held")
	}

	if err := l1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l3, ok, err := TryAcquire(p)
	if err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v err=%v", ok, err)
	}
	l3.Release()
}

func TestReleaseNilSafe(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Fatalf("nil Release: %v", err)
	}
}

// TestCrashStaleReclaim proves the core property: a lock held by a process that
// dies WITHOUT calling Release is reclaimed by the OS, so the next process can
// acquire it. It re-executes this test binary as a child that acquires the lock
// and blocks; the parent confirms contention, kills the child, and reacquires.
// Runs on whatever platform executes `go test` (Windows here, Linux/macOS in CI).
func TestCrashStaleReclaim(t *testing.T) {
	if lockPath := os.Getenv("CLAUDEX_OSLOCK_CHILD"); lockPath != "" {
		// Child mode: acquire the lock, signal readiness, block until killed.
		// Store the lock in a package var so GC never finalizes the underlying
		// file handle (which would release the lock before we are killed).
		var ok bool
		var err error
		childHeldLock, ok, err = TryAcquire(lockPath)
		if err != nil || !ok {
			os.Exit(3)
		}
		_ = os.WriteFile(os.Getenv("CLAUDEX_OSLOCK_READY"), []byte("ready"), 0o600)
		// Hold the lock until the parent kills us. time.Sleep (not select{}) so a
		// runtime timer stays scheduled and the deadlock detector does not fire,
		// which would crash the child and release the lock prematurely.
		time.Sleep(10 * time.Minute)
	}

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "run.lock")
	readyPath := filepath.Join(dir, "ready")

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashStaleReclaim$")
	cmd.Env = append(os.Environ(),
		"CLAUDEX_OSLOCK_CHILD="+lockPath,
		"CLAUDEX_OSLOCK_READY="+readyPath,
	)
	var childOut bytes.Buffer
	cmd.Stdout = &childOut
	cmd.Stderr = &childOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	}()

	if !waitFor(func() bool { _, err := os.Stat(readyPath); return err == nil }, 10*time.Second) {
		t.Fatalf("child never signalled ready")
	}

	// While the child holds the lock, the parent must observe contention.
	if l, ok, err := TryAcquire(lockPath); err != nil {
		t.Fatalf("parent TryAcquire errored: %v", err)
	} else if ok {
		l.Release() // don't leak a live handle out of the failing assertion
		t.Fatalf("parent acquired the lock while the child held it; child output:\n%s", childOut.String())
	}

	// Kill the child WITHOUT giving it a chance to Release.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_, _ = cmd.Process.Wait()
	killed = true

	// The kernel released the lock when the child died; the parent can reacquire.
	if !waitFor(func() bool {
		l, ok, err := TryAcquire(lockPath)
		if err == nil && ok {
			l.Release()
			return true
		}
		return false
	}, 10*time.Second) {
		t.Fatalf("lock was not reclaimed after the holder was killed")
	}
}

// childHeldLock keeps the child process's lock reachable so GC cannot finalize
// its file handle and release the lock before the parent kills the child.
var childHeldLock *Lock

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// --- rooted acquire and probe (the attempt lease) ---

func testRoot(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func TestRootedAcquireAndRelease(t *testing.T) {
	root := testRoot(t)
	l, ok, err := TryAcquireInRoot(root, "lease")
	if err != nil || !ok {
		t.Fatalf("TryAcquireInRoot: ok=%v err=%v", ok, err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, ok, err := TryAcquireInRoot(root, "lease")
	if err != nil || !ok {
		t.Fatalf("reacquire after release: ok=%v err=%v", ok, err)
	}
	_ = l2.Release()
}

// TestProbeSeesTheSamePrimitiveIsTheWholePoint. flock and OFD locks do not contend with each other, so
// a probe written against a different family would report a live lease as unheld and a second attempt
// would start over a running one. Acquire and probe must therefore share one implementation.
func TestProbeSeesAHeldRootedLease(t *testing.T) {
	root := testRoot(t)
	// Arming creates the entry, exactly as the supervisor does before any probe can be meaningful.
	arm, _, err := TryAcquireInRoot(root, "lease")
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if err := arm.Release(); err != nil {
		t.Fatalf("release arm: %v", err)
	}
	held, err := ProbeInRoot(root, "lease")
	if err != nil {
		t.Fatalf("probe on a free lease: %v", err)
	}
	if held {
		t.Fatal("an unheld lease was reported held")
	}

	l, ok, err := TryAcquireInRoot(root, "lease")
	if err != nil || !ok {
		t.Fatalf("TryAcquireInRoot: ok=%v err=%v", ok, err)
	}
	held, err = ProbeInRoot(root, "lease")
	if err != nil {
		t.Fatalf("probe on a held lease: %v", err)
	}
	if !held {
		t.Fatal("a held lease was reported unheld; acquire and probe are not contending")
	}

	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	held, err = ProbeInRoot(root, "lease")
	if err != nil {
		t.Fatalf("probe after release: %v", err)
	}
	if held {
		t.Fatal("a released lease was still reported held")
	}
}

// TestProbeDoesNotRetainTheLock: a probe that kept the lock would make the next acquire fail and look
// like a live owner.
func TestProbeDoesNotRetainTheLock(t *testing.T) {
	root := testRoot(t)
	arm, _, err := TryAcquireInRoot(root, "lease")
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if err := arm.Release(); err != nil {
		t.Fatalf("release arm: %v", err)
	}
	for i := 0; i < 3; i++ {
		if held, err := ProbeInRoot(root, "lease"); err != nil || held {
			t.Fatalf("probe %d: held=%v err=%v", i, held, err)
		}
	}
	l, ok, aerr := TryAcquireInRoot(root, "lease")
	if aerr != nil || !ok {
		t.Fatalf("acquire after probes: ok=%v err=%v", ok, aerr)
	}
	_ = l.Release()
}

// TestRootedLeaseIsConfined: the lease cannot be steered out of the attempt directory.
func TestRootedLeaseIsConfined(t *testing.T) {
	root := testRoot(t)
	if _, _, err := TryAcquireInRoot(root, "../escaped"); err == nil {
		t.Fatal("the root permitted a lease outside the attempt directory")
	}
}

// TestProbeDoesNotCreateTheLease is the defect the acquire/probe split exists for.
//
// Sharing the acquiring open meant a probe of a lease that had been RENAMED AWAY created a fresh empty
// file at the original name and locked that instead — reporting "unheld" while the original lock was
// still live under its new name. The probe was answering a question about a file it had just made.
func TestProbeDoesNotCreateTheLease(t *testing.T) {
	root := testRoot(t)

	// Absent entry: after arming, absence is not a free lease, and probing must not manufacture one.
	held, err := ProbeInRoot(root, "lease")
	if !errors.Is(err, ErrLeaseMissing) {
		t.Fatalf("err = %v, want ErrLeaseMissing", err)
	}
	if !held {
		t.Fatal("an absent lease was reported unheld; the probe must fail closed")
	}
	if _, serr := os.Lstat(filepath.Join(root.Name(), "lease")); serr == nil {
		t.Fatal("the probe created the lease entry it was asked about")
	}

	// The displaced-lease vector: acquire, rename WITHOUT releasing, then probe the original name.
	l, ok, err := TryAcquireInRoot(root, "lease")
	if err != nil || !ok {
		t.Fatalf("TryAcquireInRoot: ok=%v err=%v", ok, err)
	}
	defer l.Release()
	if rerr := root.Rename("lease", "displaced"); rerr != nil {
		t.Skipf("cannot rename a locked lease here: %v", rerr)
	}
	held, err = ProbeInRoot(root, "lease")
	if !errors.Is(err, ErrLeaseMissing) {
		t.Fatalf("displaced lease: err = %v, want ErrLeaseMissing", err)
	}
	if !held {
		t.Fatal("a displaced lease was reported unheld while its lock was still live")
	}
	// The lock really is still held, under the new name.
	stillHeld, err := ProbeInRoot(root, "displaced")
	if err != nil {
		t.Fatalf("probe displaced: %v", err)
	}
	if !stillHeld {
		t.Fatal("the renamed entry should still carry the live lock")
	}
}

// TestProbeRejectsANonRegularLease: following a substitute would let something other than the lease
// answer for it.
func TestProbeRejectsANonRegularLease(t *testing.T) {
	root := testRoot(t)
	if err := os.Mkdir(filepath.Join(root.Name(), "lease"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	held, err := ProbeInRoot(root, "lease")
	if !errors.Is(err, ErrLeaseNotRegular) {
		t.Fatalf("err = %v, want ErrLeaseNotRegular", err)
	}
	if !held {
		t.Fatal("a non-regular lease entry was reported unheld")
	}
}
