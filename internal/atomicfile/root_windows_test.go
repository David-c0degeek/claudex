//go:build windows

package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// The rooted move retries the transient sharing/access violations another process
// causes while briefly holding the target, then succeeds.
func TestMoveWithRetryRootedRecoversTransient(t *testing.T) {
	calls := 0
	move := func(_, _ string) error {
		calls++
		if calls < 3 {
			return windows.ERROR_SHARING_VIOLATION
		}
		return nil
	}
	if err := moveWithRetry(move, "a", "b"); err != nil {
		t.Fatalf("rooted move did not recover from transient sharing violations: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

// A no-clobber conflict (fs.ErrExist) is never retried.
func TestMoveWithRetryRootedNoClobberNotRetried(t *testing.T) {
	calls := 0
	move := func(_, _ string) error { calls++; return os.ErrExist }
	if err := moveWithRetry(move, "a", "b"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want fs.ErrExist", err)
	}
	if calls != 1 {
		t.Fatalf("no-clobber conflict retried: calls = %d", calls)
	}
}

// SyncInRoot's writable-open retries a transient sharing violation and recovers;
// if it exhausts, the already-visible file yields a committed *PostCommitSyncError.
func TestSyncInRootRecoversAndExhaustsTransientOpen(t *testing.T) {
	r, _ := openRoot(t)
	if err := InstallInRoot(r, "a.json", []byte("x"), 0o600); err != nil {
		t.Fatalf("install: %v", err)
	}
	calls := 0
	recover := rootOps{syncDir: syncRootDir, openRW: func(root *os.Root, name string) (*os.File, error) {
		calls++
		if calls < 3 {
			return nil, windows.ERROR_SHARING_VIOLATION
		}
		return root.OpenFile(name, os.O_RDWR, 0)
	}}
	if err := syncInRoot(r, "a.json", recover); err != nil {
		t.Fatalf("re-confirm did not recover from transient opens: %v", err)
	}
	if calls != 3 {
		t.Fatalf("open calls = %d, want 3", calls)
	}

	exhaust := rootOps{syncDir: syncRootDir, openRW: func(*os.Root, string) (*os.File, error) {
		return nil, windows.ERROR_SHARING_VIOLATION
	}}
	var pce *PostCommitSyncError
	if err := syncInRoot(r, "a.json", exhaust); !errors.As(err, &pce) {
		t.Fatalf("exhausted re-confirm err = %v, want *PostCommitSyncError", err)
	}
}
