package atomicfile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func openRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

func TestInstallInRootNoClobber(t *testing.T) {
	r, dir := openRoot(t)
	if err := InstallInRoot(r, "a.json", []byte("one"), 0o600); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.json"))
	if err != nil || string(got) != "one" {
		t.Fatalf("read back = %q err=%v", got, err)
	}
	// A second install of the same name fails closed (no clobber).
	if err := InstallInRoot(r, "a.json", []byte("two"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second install err = %v, want fs.ErrExist", err)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "a.json"))
	if string(got) != "one" {
		t.Fatalf("no-clobber install overwrote: %q", got)
	}
	// No temp files linger.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if len(e.Name()) >= len(tmpPrefix) && e.Name()[:len(tmpPrefix)] == tmpPrefix {
			t.Fatalf("temp file lingered: %s", e.Name())
		}
	}
}

func TestReplaceInRootOverwrites(t *testing.T) {
	r, dir := openRoot(t)
	if err := ReplaceInRoot(r, "a.json", []byte("one"), 0o600); err != nil {
		t.Fatalf("replace 1: %v", err)
	}
	if err := ReplaceInRoot(r, "a.json", []byte("two"), 0o600); err != nil {
		t.Fatalf("replace 2: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.json"))
	if string(got) != "two" {
		t.Fatalf("replace did not overwrite: %q", got)
	}
}

func TestReadInRootRegularOnly(t *testing.T) {
	r, _ := openRoot(t)
	if err := InstallInRoot(r, "a.json", []byte("hello"), 0o600); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := ReadInRoot(r, "a.json", 1<<20)
	if err != nil || !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("read = %q err=%v", got, err)
	}
	// Oversize by one byte is a corruption error, not a truncated read.
	if _, err := ReadInRoot(r, "a.json", 4); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("oversize read err = %v, want ErrNotRegular", err)
	}
	// A directory is not a regular file.
	if err := MkdirInRoot(r, "sub", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := ReadInRoot(r, "sub", 1<<20); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("dir read err = %v, want ErrNotRegular", err)
	}
	// A missing file is not-exist.
	if _, err := ReadInRoot(r, "missing.json", 1<<20); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing read err = %v, want fs.ErrNotExist", err)
	}
}

func TestMkdirInRootIdempotent(t *testing.T) {
	r, _ := openRoot(t)
	if err := MkdirInRoot(r, "d", 0o700); err != nil {
		t.Fatalf("mkdir 1: %v", err)
	}
	if err := MkdirInRoot(r, "d", 0o700); err != nil {
		t.Fatalf("mkdir 2 (idempotent): %v", err)
	}
}

func TestSyncInRootReconfirms(t *testing.T) {
	r, _ := openRoot(t)
	if err := InstallInRoot(r, "a.json", []byte("x"), 0o600); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Re-confirming an existing file is durable and idempotent.
	if err := SyncInRoot(r, "a.json"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// A missing file cannot be confirmed.
	if err := SyncInRoot(r, "missing.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("sync missing err = %v, want fs.ErrNotExist", err)
	}
}

// A failed durability sync after the bytes are already visible is a committed
// *PostCommitSyncError for install, replace, mkdir, and re-confirm; a later
// healthy call re-confirms durability and succeeds.
func TestRootedSyncFaultsAreCommitted(t *testing.T) {
	errInject := errors.New("injected dir sync failure")
	bad := rootOps{syncDir: func(*os.Root, string) error { return errInject }, openRW: defaultRootOps.openRW}
	asCommitted := func(t *testing.T, err error) {
		t.Helper()
		var pce *PostCommitSyncError
		if !errors.As(err, &pce) {
			t.Fatalf("err = %v, want *PostCommitSyncError", err)
		}
		if !pce.Committed() {
			t.Fatalf("PostCommitSyncError must report committed")
		}
	}

	t.Run("install", func(t *testing.T) {
		r, dir := openRoot(t)
		err := publishInRoot(r, "a.json", []byte("x"), 0o600, r.Link, false, bad)
		asCommitted(t, err)
		if got, _ := os.ReadFile(filepath.Join(dir, "a.json")); string(got) != "x" {
			t.Fatalf("install bytes not visible after committed sync failure: %q", got)
		}
		if err := SyncInRoot(r, "a.json"); err != nil { // healthy re-confirm
			t.Fatalf("re-confirm: %v", err)
		}
	})

	t.Run("replace", func(t *testing.T) {
		r, dir := openRoot(t)
		err := publishInRoot(r, "a.json", []byte("y"), 0o600, r.Rename, true, bad)
		asCommitted(t, err)
		if got, _ := os.ReadFile(filepath.Join(dir, "a.json")); string(got) != "y" {
			t.Fatalf("replace bytes not visible after committed sync failure: %q", got)
		}
	})

	t.Run("mkdir", func(t *testing.T) {
		r, dir := openRoot(t)
		err := mkdirInRoot(r, "d", 0o700, bad)
		asCommitted(t, err)
		if info, serr := os.Stat(filepath.Join(dir, "d")); serr != nil || !info.IsDir() {
			t.Fatalf("dir not visible after committed sync failure: %v", serr)
		}
		if err := MkdirInRoot(r, "d", 0o700); err != nil { // healthy re-confirm
			t.Fatalf("re-confirm mkdir: %v", err)
		}
	})

	t.Run("reconfirm", func(t *testing.T) {
		r, _ := openRoot(t)
		if err := InstallInRoot(r, "a.json", []byte("z"), 0o600); err != nil {
			t.Fatalf("install: %v", err)
		}
		err := syncInRoot(r, "a.json", bad)
		asCommitted(t, err)
		if err := SyncInRoot(r, "a.json"); err != nil { // healthy re-confirm
			t.Fatalf("healthy re-confirm: %v", err)
		}
	})
}

// Re-confirming an already-visible file whose writable open fails for any reason
// other than genuine absence is a committed *PostCommitSyncError; a genuinely
// absent file is a raw not-exist error.
func TestSyncInRootOpenFailuresAreCommitted(t *testing.T) {
	r, _ := openRoot(t)
	if err := InstallInRoot(r, "a.json", []byte("x"), 0o600); err != nil {
		t.Fatalf("install: %v", err)
	}
	errOpen := errors.New("injected non-transient open failure")
	bad := rootOps{syncDir: syncRootDir, openRW: func(*os.Root, string) (*os.File, error) { return nil, errOpen }}
	err := syncInRoot(r, "a.json", bad)
	var pce *PostCommitSyncError
	if !errors.As(err, &pce) || !errors.Is(err, errOpen) {
		t.Fatalf("open-failure re-confirm err = %v, want a committed error wrapping the open failure", err)
	}

	absent := rootOps{syncDir: syncRootDir, openRW: func(*os.Root, string) (*os.File, error) { return nil, fs.ErrNotExist }}
	err = syncInRoot(r, "a.json", absent)
	if errors.As(err, &pce) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent re-confirm err = %v, want a raw not-exist error", err)
	}
}

// MkdirInRoot re-confirming a path that exists as a non-directory is refused.
func TestMkdirInRootRejectsNonDir(t *testing.T) {
	r, _ := openRoot(t)
	if err := InstallInRoot(r, "a", []byte("file"), 0o600); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := MkdirInRoot(r, "a", 0o700); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("mkdir over a file err = %v, want ErrNotDirectory", err)
	}
}

func TestInstallInRootNested(t *testing.T) {
	r, dir := openRoot(t)
	if err := MkdirInRoot(r, "turn-1", 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := InstallInRoot(r, "turn-1/x.json", []byte("nested"), 0o600); err != nil {
		t.Fatalf("nested install: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "turn-1", "x.json"))
	if err != nil || string(got) != "nested" {
		t.Fatalf("nested read = %q err=%v", got, err)
	}
}
