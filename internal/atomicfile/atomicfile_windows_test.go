//go:build windows

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
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

// The durable Windows move (MOVEFILE_WRITE_THROUGH) replaces the target and makes the
// new contents visible. This pins the write-through move path — the flag that makes the
// move power-safe, which plain os.Rename does not request.
func TestMoveFileWriteThroughReplaces(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatalf("write dst: %v", err)
	}
	if err := moveFileWriteThrough(src, dst); err != nil {
		t.Fatalf("moveFileWriteThrough: %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "new" {
		t.Fatalf("dst = %q err=%v, want %q", got, err, "new")
	}
	if _, serr := os.Stat(src); !os.IsNotExist(serr) {
		t.Fatalf("src still present after move: %v", serr)
	}
}

// The directory-creation publication primitive succeeds on a real directory: Windows
// refuses FlushFileBuffers on a directory handle (ERROR_ACCESS_DENIED), which
// swallowDirFlush treats as satisfied, so the durability barrier does not spuriously fail.
func TestSyncDirSucceedsOnDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := syncDir(dir); err != nil {
		t.Fatalf("syncDir(%q) = %v, want nil (the directory-flush refusal is satisfied)", dir, err)
	}
}

// MkdirAllDurable creates a durable multi-level tree, is idempotent on an existing tree,
// and rejects a path that exists as a file.
func TestMkdirAllDurableCreatesTree(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := MkdirAllDurable(deep, 0o700); err != nil {
		t.Fatalf("MkdirAllDurable: %v", err)
	}
	if fi, err := os.Stat(deep); err != nil || !fi.IsDir() {
		t.Fatalf("stat %q: fi=%v err=%v", deep, fi, err)
	}
	if err := MkdirAllDurable(deep, 0o700); err != nil {
		t.Fatalf("MkdirAllDurable idempotent: %v", err)
	}
	f := filepath.Join(root, "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := MkdirAllDurable(f, 0o700); err == nil {
		t.Fatal("MkdirAllDurable over a file must fail")
	}
}

// swallowDirFlush treats only the Windows directory-flush refusal as satisfied; a real
// error propagates.
func TestSwallowDirFlush(t *testing.T) {
	if err := swallowDirFlush(nil); err != nil {
		t.Fatalf("nil should be satisfied: %v", err)
	}
	if err := swallowDirFlush(windows.ERROR_ACCESS_DENIED); err != nil {
		t.Fatalf("the directory-flush refusal should be satisfied: %v", err)
	}
	real := errors.New("EIO")
	if err := swallowDirFlush(real); err != real {
		t.Fatalf("a real error must propagate, got %v", err)
	}
}
