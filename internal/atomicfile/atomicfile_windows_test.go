//go:build windows

package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The rooted immutable FILE publish (InstallInRoot) goes through the confined write-through
// no-clobber rename with EXACTLY MOVEFILE_WRITE_THROUGH (no REPLACE_EXISTING) — pinned via
// the seam — and a second install of the same name is a no-clobber fs.ErrExist conflict.
func TestInstallInRootPublishesViaWriteThroughNoClobber(t *testing.T) {
	var gotFlags []uint32
	orig := moveFileEx
	moveFileEx = func(from, to *uint16, flags uint32) error {
		gotFlags = append(gotFlags, flags)
		return orig(from, to, flags)
	}
	defer func() { moveFileEx = orig }()

	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()
	if err := InstallInRoot(root, "f.txt", []byte("data"), 0o600); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(gotFlags) != 1 || gotFlags[0] != uint32(windows.MOVEFILE_WRITE_THROUGH) {
		t.Fatalf("install flags = %v, want [%#x] (write-through, no REPLACE_EXISTING)", gotFlags, uint32(windows.MOVEFILE_WRITE_THROUGH))
	}
	if got, _ := os.ReadFile(filepath.Join(base, "f.txt")); string(got) != "data" {
		t.Fatalf("installed content = %q", got)
	}
	if err := InstallInRoot(root, "f.txt", []byte("other"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second install err = %v, want fs.ErrExist (no-clobber)", err)
	}
}

// The own-move-visible cut: a moveFileEx that renames the directory into place THEN returns
// a (non-already-exists) error is our own visible-but-unconfirmed move — MkdirInRoot must
// return *PostCommitSyncError (durability unconfirmed), NEVER clean success.
func TestMkdirInRootOwnMoveVisibleFlushErrorIsUnconfirmed(t *testing.T) {
	orig := moveFileEx
	moveFileEx = func(from, to *uint16, flags uint32) error {
		_ = orig(from, to, flags) // perform the real rename (target becomes visible)...
		return errors.New("simulated write-through flush failure")
	}
	defer func() { moveFileEx = orig }()

	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()
	err = MkdirInRoot(root, "d", 0o700)
	var pce *PostCommitSyncError
	if !errors.As(err, &pce) {
		t.Fatalf("own-move-visible flush error = %v, want *PostCommitSyncError (not clean success)", err)
	}
	if fi, serr := os.Stat(filepath.Join(base, "d")); serr != nil || !fi.IsDir() {
		t.Fatalf("directory should be visible after the own move: %v", serr)
	}
}

// finalPathByHandle grows its buffer when GetFinalPathNameByHandle reports insufficient
// space (n >= len(buf), which INCLUDES the NUL) rather than blessing the boundary as
// success. A one-element initial buffer forces the grow path.
func TestFinalPathByHandleGrowsBuffer(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer f.Close()
	got, err := finalPathByHandleBuf(windows.Handle(f.Fd()), make([]uint16, 1))
	if err != nil {
		t.Fatalf("finalPathByHandleBuf (grow path): %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(got), strings.ToLower(filepath.Base(dir))) {
		t.Fatalf("resolved path %q does not end with %q", got, filepath.Base(dir))
	}
}

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

// The file replace path invokes MoveFileEx with EXACTLY
// MOVEFILE_REPLACE_EXISTING|MOVEFILE_WRITE_THROUGH — pinned via the seam so a regression
// to os.Rename or a dropped flag is caught (the integration test above cannot see flags).
func TestReplaceInvokesMoveFileExWithWriteThroughFlags(t *testing.T) {
	var gotFlags uint32
	called := false
	orig := moveFileEx
	moveFileEx = func(from, to *uint16, flags uint32) error {
		called, gotFlags = true, flags
		return orig(from, to, flags)
	}
	defer func() { moveFileEx = orig }()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("y"), 0o600); err != nil {
		t.Fatalf("write dst: %v", err)
	}
	if err := moveFileWriteThrough(src, dst); err != nil {
		t.Fatalf("moveFileWriteThrough: %v", err)
	}
	want := uint32(windows.MOVEFILE_REPLACE_EXISTING | windows.MOVEFILE_WRITE_THROUGH)
	if !called || gotFlags != want {
		t.Fatalf("file replace flags = %#x (called=%v), want %#x", gotFlags, called, want)
	}
}

// The directory publish path invokes MoveFileEx with EXACTLY MOVEFILE_WRITE_THROUGH
// (REPLACE_EXISTING is invalid for directories and the target does not exist).
func TestDirPublishInvokesMoveFileExWithWriteThroughFlag(t *testing.T) {
	var gotFlags uint32
	called := false
	orig := moveFileEx
	moveFileEx = func(from, to *uint16, flags uint32) error {
		called, gotFlags = true, flags
		return orig(from, to, flags)
	}
	defer func() { moveFileEx = orig }()

	dir := filepath.Join(t.TempDir(), "newdir")
	if err := ensureDirDurableImpl(dir, 0o700); err != nil {
		t.Fatalf("ensureDirDurableImpl: %v", err)
	}
	if !called || gotFlags != uint32(windows.MOVEFILE_WRITE_THROUGH) {
		t.Fatalf("dir publish flags = %#x (called=%v), want %#x", gotFlags, called, uint32(windows.MOVEFILE_WRITE_THROUGH))
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not published: fi=%v err=%v", fi, err)
	}
}

// The ROOTED directory publish (MkdirInRoot, as snapshotStep uses per ancestor) goes
// through the confined write-through rename with EXACTLY MOVEFILE_WRITE_THROUGH — pinned
// via the seam. This is the rooted path, not the path-based MkdirAllDurable helper.
func TestMkdirInRootPublishesViaWriteThrough(t *testing.T) {
	var gotFlags []uint32
	orig := moveFileEx
	moveFileEx = func(from, to *uint16, flags uint32) error {
		gotFlags = append(gotFlags, flags)
		return orig(from, to, flags)
	}
	defer func() { moveFileEx = orig }()

	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer root.Close()
	for _, d := range []string{"a", "a/b", "a/b/c"} { // nested tree, level by level
		if err := MkdirInRoot(root, d, 0o700); err != nil {
			t.Fatalf("MkdirInRoot(%s): %v", d, err)
		}
	}
	if len(gotFlags) != 3 {
		t.Fatalf("moveFileEx invoked %d times, want 3 (one write-through publish per level)", len(gotFlags))
	}
	for i, f := range gotFlags {
		if f != uint32(windows.MOVEFILE_WRITE_THROUGH) {
			t.Fatalf("rooted publish %d flags = %#x, want %#x", i, f, uint32(windows.MOVEFILE_WRITE_THROUGH))
		}
	}
	if fi, err := os.Stat(filepath.Join(base, "a", "b", "c")); err != nil || !fi.IsDir() {
		t.Fatalf("rooted directory tree not published: fi=%v err=%v", fi, err)
	}
}

// Site 1: the PATH-based directory publish own-move-visible cut — a moveFileEx that renames
// the dir into place THEN errors is our visible-but-unconfirmed move: *PostCommitSyncError,
// never clean success.
func TestPublishDirWriteThroughOwnMoveVisibleIsUnconfirmed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	orig := moveFileEx
	moveFileEx = func(from, to *uint16, flags uint32) error {
		_ = orig(from, to, flags) // rename into place (visible)...
		return errors.New("simulated write-through flush failure")
	}
	defer func() { moveFileEx = orig }()
	err := publishDirWriteThrough(dir, 0o700)
	var pce *PostCommitSyncError
	if !errors.As(err, &pce) {
		t.Fatalf("own-move-visible flush error = %v, want *PostCommitSyncError (not clean success)", err)
	}
	if fi, serr := os.Stat(dir); serr != nil || !fi.IsDir() {
		t.Fatalf("directory should be visible after the own move: %v", serr)
	}
}

// Site 2: the PATH-based exists/recovery re-confirm uses the REAL barrier (moveFileEx),
// never a swallowed no-op, and is re-runnable after a barrier failure.
func TestEnsureDirDurableExistsUsesRealBarrier(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	calls := 0
	orig := moveFileEx
	moveFileEx = func(from, to *uint16, flags uint32) error {
		calls++
		if calls == 1 {
			return errors.New("barrier failed once")
		}
		return orig(from, to, flags)
	}
	defer func() { moveFileEx = orig }()
	if err := ensureDirDurableImpl(dir, 0o700); err == nil {
		t.Fatal("exists-branch re-confirm must surface the barrier failure, not swallow it")
	}
	if err := ensureDirDurableImpl(dir, 0o700); err != nil {
		t.Fatalf("retry must re-confirm via the real barrier: %v", err)
	}
	if calls < 2 {
		t.Fatalf("moveFileEx invoked %d times, want >= 2 (exists-branch used the real barrier)", calls)
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
