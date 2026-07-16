package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteCreatesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := Write(p, []byte("hello"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}
}

func TestWriteReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := Write(p, []byte("v1"), 0o644); err != nil {
		t.Fatalf("Write v1: %v", err)
	}
	if err := Write(p, []byte("v2-is-longer"), 0o644); err != nil {
		t.Fatalf("Write v2: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "v2-is-longer" {
		t.Fatalf("content = %q, want %q", got, "v2-is-longer")
	}
}

func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	for i := 0; i < 3; i++ {
		if err := Write(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries, want exactly the target file", len(entries))
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWritePreservesPreviousOnError(t *testing.T) {
	// A write into a directory that does not exist must fail without touching an
	// existing good file elsewhere.
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := Write(good, []byte("keep-me"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	missing := filepath.Join(dir, "no-such-subdir", "state.json")
	if err := Write(missing, []byte("nope"), 0o644); err == nil {
		t.Fatalf("Write into missing dir unexpectedly succeeded")
	}
	got, _ := os.ReadFile(good)
	if string(got) != "keep-me" {
		t.Fatalf("existing file was disturbed: %q", got)
	}
}

func TestWritePermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := Write(p, []byte("x"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, _ := os.Stat(p)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
}
