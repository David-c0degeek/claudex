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
