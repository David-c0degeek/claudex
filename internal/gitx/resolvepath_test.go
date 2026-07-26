package gitx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveThroughMissingLeafFailsClosedOnAnExistingUnresolvable is the guard against reinterpreting
// "cannot resolve" as "not there yet".
//
// This helper feeds a registration ownership proof, so an existing entry that cannot be resolved — a
// symlink loop, a dangling link — must fail closed rather than be treated as a recoverable missing
// worktree. Note that Lstat, not Stat, is what distinguishes the cases: a looping or dangling symlink
// EXISTS as a directory entry while EvalSymlinks reports not-exist, so an errors.Is check on the
// resolution error alone would still admit it.
func TestResolveThroughMissingLeafFailsClosedOnAnExistingUnresolvable(t *testing.T) {
	dir := t.TempDir()

	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if _, err := resolveThroughMissingLeaf(loop); err == nil {
		t.Fatal("a symlink loop was converted into a missing-tail success")
	}

	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "no-such-target"), dangling); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
	if _, err := resolveThroughMissingLeaf(dangling); err == nil {
		t.Fatal("a dangling symlink was converted into a missing-tail success")
	}

	// A component BENEATH an unresolvable entry must fail too: stripping it would walk straight past
	// the entry that cannot be trusted.
	if _, err := resolveThroughMissingLeaf(filepath.Join(loop, "child")); err == nil {
		t.Fatal("a path beneath a symlink loop was resolved")
	}
}

// TestResolveThroughMissingLeafHandlesGenuineAbsence is the case the helper exists for: our worktree
// directory has been deleted and the registration must still be matchable.
func TestResolveThroughMissingLeafHandlesGenuineAbsence(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "gone", "deeper")
	got, err := resolveThroughMissingLeaf(missing)
	if err != nil {
		t.Fatalf("genuine absence must resolve: %v", err)
	}
	// The existing ancestor is resolved and the absent tail re-appended, so the answer is comparable
	// with a path the OS reported through its own symlinks.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if want := filepath.Join(realDir, "gone", "deeper"); got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fixture is not genuinely absent: %v", err)
	}
}

// TestResolveThroughMissingLeafPassesThroughExisting keeps the ordinary path honest.
func TestResolveThroughMissingLeafPassesThroughExisting(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveThroughMissingLeaf(dir)
	if err != nil {
		t.Fatalf("existing path: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}
