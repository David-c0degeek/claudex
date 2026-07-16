package oslock

import (
	"path/filepath"
	"testing"
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
	// acquire while the first is held must fail immediately on both platforms —
	// this is the "second run-lock acquire is rejected promptly" vector.
	p := filepath.Join(t.TempDir(), "run.lock")

	l1, ok, err := TryAcquire(p)
	if err != nil || !ok {
		t.Fatalf("first TryAcquire: ok=%v err=%v", ok, err)
	}
	defer l1.Release()

	l2, ok, err := TryAcquire(p)
	if err != nil {
		t.Fatalf("second TryAcquire errored: %v", err)
	}
	if ok {
		l2.Release()
		t.Fatalf("second TryAcquire unexpectedly succeeded while lock held")
	}

	// After releasing the first, the lock is free again.
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
