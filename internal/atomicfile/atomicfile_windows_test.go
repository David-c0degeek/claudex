//go:build windows

package atomicfile

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReplaceWithPermanentErrorDoesNotRetry(t *testing.T) {
	permanent := errors.New("permanent: invalid path")
	calls, sleeps := 0, 0
	err := replaceWith(
		func() error { calls++; return permanent },
		func() { sleeps++ },
		isTransientRename,
	)
	if !errors.Is(err, permanent) {
		t.Fatalf("err = %v, want the permanent error", err)
	}
	if calls != 1 || sleeps != 0 {
		t.Fatalf("permanent error: calls=%d sleeps=%d, want 1/0", calls, sleeps)
	}
}

func TestReplaceWithTransientThenSuccess(t *testing.T) {
	calls, sleeps := 0, 0
	err := replaceWith(
		func() error {
			calls++
			if calls < 3 {
				return windows.ERROR_SHARING_VIOLATION
			}
			return nil
		},
		func() { sleeps++ },
		isTransientRename,
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 3 || sleeps != 2 {
		t.Fatalf("transient-then-success: calls=%d sleeps=%d, want 3/2", calls, sleeps)
	}
}

func TestReplaceWithExhaustsTransient(t *testing.T) {
	calls, sleeps := 0, 0
	err := replaceWith(
		func() error { calls++; return windows.ERROR_ACCESS_DENIED },
		func() { sleeps++ },
		isTransientRename,
	)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("err = %v, want ACCESS_DENIED", err)
	}
	if calls != maxRenameAttempts || sleeps != maxRenameAttempts-1 {
		t.Fatalf("exhaustion: calls=%d sleeps=%d, want %d/%d", calls, sleeps, maxRenameAttempts, maxRenameAttempts-1)
	}
}

func TestIsTransientRenameSelectivity(t *testing.T) {
	if !isTransientRename(windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("SHARING_VIOLATION should be transient")
	}
	if !isTransientRename(windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("ACCESS_DENIED should be transient")
	}
	if isTransientRename(windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("FILE_NOT_FOUND must not be treated as transient")
	}
	if isTransientRename(errors.New("some other error")) {
		t.Fatalf("generic error must not be transient")
	}
}
