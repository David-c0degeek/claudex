package atomicfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestWriteCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := Write(p, []byte("v1"), 0o644); err != nil {
		t.Fatalf("Write v1: %v", err)
	}
	if err := Write(p, []byte("v2-is-longer"), 0o644); err != nil {
		t.Fatalf("Write v2: %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "v2-is-longer" {
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
		t.Fatalf("directory has %d entries, want exactly the target", len(entries))
	}
	if strings.HasPrefix(entries[0].Name(), tmpPrefix) {
		t.Fatalf("leftover temp file: %s", entries[0].Name())
	}
}

// faultFile wraps *os.File and forces one operation to fail.
type faultFile struct {
	f      *os.File
	failOn string // "write" | "sync" | "chmod" | "close"
}

func (x *faultFile) Write(p []byte) (int, error) {
	if x.failOn == "write" {
		return 0, errors.New("injected write failure")
	}
	return x.f.Write(p)
}
func (x *faultFile) Sync() error {
	if x.failOn == "sync" {
		return errors.New("injected sync failure")
	}
	return x.f.Sync()
}
func (x *faultFile) Chmod(m os.FileMode) error {
	if x.failOn == "chmod" {
		return errors.New("injected chmod failure")
	}
	return x.f.Chmod(m)
}
func (x *faultFile) Close() error {
	err := x.f.Close()
	if x.failOn == "close" {
		return errors.New("injected close failure")
	}
	return err
}
func (x *faultFile) Name() string { return x.f.Name() }

// injectedOps returns ops that create faultFiles failing at step, and lets the
// caller force rename/syncDir failures too.
func injectedOps(step string, renameErr, syncErr error) ops {
	o := defaultOps
	o.createTemp = func(dir, pattern string) (tmpFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return &faultFile{f: f, failOn: step}, nil
	}
	if renameErr != nil {
		o.rename = func(_, _ string) error { return renameErr }
	}
	if syncErr != nil {
		o.syncDir = func(string) error { return syncErr }
	}
	return o
}

func TestPreCommitFailuresPreservePreviousFile(t *testing.T) {
	for _, step := range []string{"write", "sync", "chmod", "close", "rename"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "state.json")
			if err := Write(p, []byte("original"), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}

			var o ops
			if step == "rename" {
				o = injectedOps("", errors.New("injected rename failure"), nil)
			} else {
				o = injectedOps(step, nil, nil)
			}
			err := write(p, []byte("replacement"), 0o644, o)
			if err == nil {
				t.Fatalf("write with injected %s failure unexpectedly succeeded", step)
			}
			var pce *PostCommitSyncError
			if errors.As(err, &pce) {
				t.Fatalf("%s failure wrongly reported as post-commit", step)
			}

			// The original file must be byte-identical.
			if got, _ := os.ReadFile(p); string(got) != "original" {
				t.Fatalf("original disturbed after %s failure: %q", step, got)
			}
			// No leaked temp files.
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), tmpPrefix) {
					t.Fatalf("leaked temp after %s failure: %s", step, e.Name())
				}
			}
		})
	}
}

func TestPostCommitSyncFailureReportsCommitted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := Write(p, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	o := injectedOps("", nil, errors.New("injected dir sync failure"))
	err := write(p, []byte("new-bytes"), 0o644, o)

	var pce *PostCommitSyncError
	if !errors.As(err, &pce) {
		t.Fatalf("want *PostCommitSyncError, got %v", err)
	}
	if !pce.Committed() {
		t.Fatalf("PostCommitSyncError.Committed() = false, want true")
	}
	// Despite the error, the new bytes ARE committed and visible.
	if got, _ := os.ReadFile(p); string(got) != "new-bytes" {
		t.Fatalf("committed content = %q, want %q", got, "new-bytes")
	}
}

func TestConcurrentReaderSeesCompleteValue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Rename is not guaranteed atomic on Windows (see package doc); strict atomicity is asserted only where the OS guarantees it")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	a := bytes.Repeat([]byte("A"), 64*1024)
	b := bytes.Repeat([]byte("B"), 48*1024)
	if err := Write(p, a, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Writer alternates two distinct complete payloads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		cur := b
		for {
			select {
			case <-stop:
				return
			default:
				if err := Write(p, cur, 0o644); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
				if cur[0] == 'A' {
					cur = b
				} else {
					cur = a
				}
			}
		}
	}()
	// Readers must only ever observe a complete A* or B* payload.
	var rwg sync.WaitGroup
	for r := 0; r < 4; r++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for i := 0; i < 300; i++ {
				got, err := os.ReadFile(p)
				if err != nil {
					t.Errorf("ReadFile: %v", err)
					return
				}
				if !(bytes.Equal(got, a) || bytes.Equal(got, b)) {
					t.Errorf("reader saw a torn value of length %d", len(got))
					return
				}
			}
		}()
	}
	rwg.Wait()
	close(stop)
	wg.Wait()
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

// MkdirAllDurable must be re-runnable: if a directory is created but its durability
// confirmation fails, a retry RE-CONFIRMS the existing directory rather than blessing a
// visible-but-unconfirmed one (Blocking: the exists branch must re-confirm). Seam-driven
// so it is platform-independent.
func TestMkdirAllDurableReconfirmsOnRetryAfterConfirmFailure(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sub", "store")

	calls := map[string]int{}
	failOnce := map[string]bool{dir: true}
	ensure := func(d string, perm os.FileMode) error {
		_ = os.MkdirAll(d, perm) // the directory IS created (create succeeds)...
		calls[d]++
		if failOnce[d] && calls[d] == 1 {
			return errors.New("parent sync failed") // ...but its confirmation fails once
		}
		return nil
	}

	if err := mkdirAllDurable(dir, 0o700, ensure); err == nil {
		t.Fatal("first attempt must fail when the durability confirmation fails")
	}
	// The directory now exists but was left unconfirmed. A retry must RE-CONFIRM it.
	if err := mkdirAllDurable(dir, 0o700, ensure); err != nil {
		t.Fatalf("retry must re-confirm the existing directory and succeed: %v", err)
	}
	if calls[dir] != 2 {
		t.Fatalf("ensure(%s) called %d times, want 2 (retry re-confirmed, did not bless)", dir, calls[dir])
	}
}

// A mid-chain confirmation failure is re-confirmed on retry too: the recursion for the
// still-missing leaf descends into the ancestor it created and re-confirms it.
func TestMkdirAllDurableReconfirmsAncestorOnRetry(t *testing.T) {
	root := t.TempDir()
	mid := filepath.Join(root, "a")
	leaf := filepath.Join(mid, "b")

	calls := map[string]int{}
	failOnce := map[string]bool{mid: true}
	ensure := func(d string, perm os.FileMode) error {
		calls[d]++
		if failOnce[d] && calls[d] == 1 {
			_ = os.MkdirAll(d, perm) // created, but confirmation fails once
			return errors.New("mid-chain sync failed")
		}
		_ = os.MkdirAll(d, perm)
		return nil
	}

	if err := mkdirAllDurable(leaf, 0o700, ensure); err == nil {
		t.Fatal("first attempt must fail at the mid-chain confirmation")
	}
	if err := mkdirAllDurable(leaf, 0o700, ensure); err != nil {
		t.Fatalf("retry must re-confirm the ancestor and the leaf: %v", err)
	}
	if calls[mid] != 2 {
		t.Fatalf("ensure(%s) called %d times, want 2 (ancestor re-confirmed on retry)", mid, calls[mid])
	}
}
