package genstore

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return Open(filepath.Join(dir, "gens"), filepath.Join(dir, "run.lock"))
}

func appendConst(t *testing.T, s *Store, expected Head, payload string) Record {
	t.Helper()
	rec, err := s.Append(expected, func(uint64, string) ([]byte, error) { return []byte(payload), nil })
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return rec
}

func genFile(s *Store, gen uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%012d%s", gen, genFileExt))
}

func TestAppendSequenceAndLatest(t *testing.T) {
	s := newStore(t)
	if _, ok, err := s.Latest(); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	r1 := appendConst(t, s, Head{}, "one")
	r2 := appendConst(t, s, r1.Head(), "two")
	r3 := appendConst(t, s, r2.Head(), "three")
	if r1.Generation != 1 || r2.PrevDigest != r1.Digest || r3.Generation != 3 {
		t.Fatalf("chain wrong: %+v %+v %+v", r1, r2, r3)
	}
	got, ok, err := s.Latest()
	if err != nil || !ok || got.Generation != 3 || string(got.Payload) != "three" {
		t.Fatalf("Latest = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestOpenHasNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "gens")
	s := Open(sub, filepath.Join(dir, "run.lock"))
	if _, ok, err := s.Latest(); err != nil || ok {
		t.Fatalf("Latest on absent store: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatalf("Open/Latest created the store dir")
	}
}

func TestAppendConflict(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	appendConst(t, s, r1.Head(), "two")
	if _, err := s.Append(r1.Head(), func(uint64, string) ([]byte, error) { return []byte("x"), nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale append err = %v, want ErrConflict", err)
	}
}

func TestTwoStoresShareOneGuard(t *testing.T) {
	// The journal+state contract: two distinct stores committed under one guard.
	dir := t.TempDir()
	lock := filepath.Join(dir, "run.lock")
	a := Open(filepath.Join(dir, "a"), lock)
	b := Open(filepath.Join(dir, "b"), lock)

	g, ok, err := Acquire(lock)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer g.Release()

	ra, err := a.AppendLocked(g, Head{}, func(uint64, string) ([]byte, error) { return []byte("a1"), nil })
	if err != nil {
		t.Fatalf("store a append: %v", err)
	}
	rb, err := b.AppendLocked(g, Head{}, func(uint64, string) ([]byte, error) { return []byte("b1"), nil })
	if err != nil {
		t.Fatalf("store b append in the same section: %v", err)
	}
	if ra.Generation != 1 || rb.Generation != 1 {
		t.Fatalf("generations a=%d b=%d, want 1/1", ra.Generation, rb.Generation)
	}
}

func TestWrongLockGuardRejected(t *testing.T) {
	s := newStore(t)
	g, ok, err := Acquire(filepath.Join(t.TempDir(), "other.lock"))
	if err != nil || !ok {
		t.Fatalf("acquire other: %v", err)
	}
	defer g.Release()
	if _, err := s.AppendLocked(g, Head{}, func(uint64, string) ([]byte, error) { return []byte("x"), nil }); !errors.Is(err, ErrWrongLock) {
		t.Fatalf("err = %v, want ErrWrongLock", err)
	}
}

func TestBusyWhenLocked(t *testing.T) {
	s := newStore(t)
	g, ok, err := Acquire(s.lockPath)
	if err != nil || !ok {
		t.Fatalf("hold lock: %v", err)
	}
	defer g.Release()
	if _, err := s.Append(Head{}, func(uint64, string) ([]byte, error) { return []byte("x"), nil }); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

func TestChainMustBeRooted(t *testing.T) {
	// A valid generation 2 with no generation 1 is an orphan, not authoritative.
	s := newStore(t)
	_, file := encode(2, "somedigest", []byte("orphan"))
	_ = os.MkdirAll(s.dir, 0o700)
	if err := os.WriteFile(genFile(s, 2), file, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Latest(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("orphan gen2 err = %v, want ErrCorrupt", err)
	}
}

func TestRootMustHaveEmptyPrev(t *testing.T) {
	s := newStore(t)
	_, file := encode(1, "nonempty-prev", []byte("root"))
	_ = os.MkdirAll(s.dir, 0o700)
	if err := os.WriteFile(genFile(s, 1), file, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Latest(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("nonempty-prev root err = %v, want ErrCorrupt", err)
	}
}

func TestTornRootWithValidSuffixIsCorrupt(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	appendConst(t, s, r1.Head(), "two")
	// Torn root, valid suffix -> the suffix is an orphan.
	if err := os.WriteFile(genFile(s, 1), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("corrupt root: %v", err)
	}
	if _, _, err := s.Latest(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("torn-root err = %v, want ErrCorrupt", err)
	}
}

func TestTornNewestFallsBackAndGap(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	r2 := appendConst(t, s, r1.Head(), "two")
	appendConst(t, s, r2.Head(), "three")
	if err := os.WriteFile(genFile(s, 3), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("corrupt newest: %v", err)
	}
	got, ok, err := s.Latest()
	if err != nil || !ok || got.Generation != 2 {
		t.Fatalf("Latest after torn newest = gen %d ok=%v err=%v, want gen 2", got.Generation, ok, err)
	}
	// Occupied slot 3 is skipped; the next append lands at 4.
	r4 := appendConst(t, s, r2.Head(), "four")
	if r4.Generation != 4 || r4.PrevDigest != r2.Digest {
		t.Fatalf("append after torn slot = %+v, want gen 4 chained to r2", r4)
	}
}

func TestNonCanonicalFilenameIsCorrupt(t *testing.T) {
	s := newStore(t)
	appendConst(t, s, Head{}, "one")
	if err := os.WriteFile(filepath.Join(s.dir, "1"+genFileExt), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Latest(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("non-canonical filename err = %v, want ErrCorrupt", err)
	}
}

func TestDecodeOverflowSafe(t *testing.T) {
	body := make([]byte, 8+10)
	binary.BigEndian.PutUint64(body[:8], ^uint64(0)) // absurd header length
	trailer := sha256.Sum256(body)
	if _, ok := decode(1, append(body, trailer[:]...)); ok {
		t.Fatalf("decode accepted an overflowing header length")
	}
}

// Injected-write reconciliation: the store must interpret an ambiguous write.

func TestWritePrecommitFailurePreservesHead(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	sentinel := errors.New("precommit failure")
	s.write = func(string, []byte, os.FileMode) error { return sentinel } // no file written
	_, err := s.Append(r1.Head(), func(uint64, string) ([]byte, error) { return []byte("two"), nil })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the original precommit error", err)
	}
	got, _, _ := s.Latest()
	if got.Generation != 1 {
		t.Fatalf("head moved to %d after a precommit failure", got.Generation)
	}
}

// A write whose data is visible but whose directory sync failed is reported as a
// typed durability-unconfirmed error over the visible record — NOT as clean success.
// The caller must confirm durability before depending on it.
func TestWriteVisibleButDurabilityUnconfirmed(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	s.write = func(path string, data []byte, perm os.FileMode) error {
		_ = atomicfile.Write(path, data, perm) // the record IS written and valid...
		return &atomicfile.PostCommitSyncError{Path: path, Err: errors.New("dir sync")}
	}
	rec, err := s.Append(r1.Head(), func(uint64, string) ([]byte, error) { return []byte("two"), nil })
	if !IsDurabilityUnconfirmed(err) {
		t.Fatalf("committed-but-unsynced write err = %v, want IsDurabilityUnconfirmed", err)
	}
	var pse *PostCommitSyncError
	if !errors.As(err, &pse) || pse.Generation != 2 {
		t.Fatalf("want *PostCommitSyncError at generation 2, got %v", err)
	}
	// The record is nonetheless the visible head — a caller must not rewrite it.
	if rec.Generation != 2 {
		t.Fatalf("committed rec gen = %d, want 2", rec.Generation)
	}
	if got, _, _ := s.Latest(); got.Generation != 2 {
		t.Fatalf("head = %d after committed write, want 2", got.Generation)
	}
}

// A candidate that is the visible head after ANY non-nil writer error (not only an
// atomicfile sync error — e.g. a WRITE_THROUGH move/flush failure) is durability-
// unconfirmed, never clean success.
func TestReconcilePlainWriteErrorIsDurabilityUnconfirmed(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	s.write = func(path string, data []byte, perm os.FileMode) error {
		_ = atomicfile.Write(path, data, perm) // the record IS written and visible...
		return errors.New("write-through move/flush failed after the destination was visible")
	}
	rec, err := s.Append(r1.Head(), func(uint64, string) ([]byte, error) { return []byte("two"), nil })
	if !IsDurabilityUnconfirmed(err) {
		t.Fatalf("plain post-visibility write error = %v, want IsDurabilityUnconfirmed", err)
	}
	if rec.Generation != 2 {
		t.Fatalf("rec gen = %d, want 2 (the visible head)", rec.Generation)
	}
}

// An ambiguous write that merely wraps an atomicfile durability error must NOT be
// classified as the proven-visible durability-unconfirmed condition.
func TestAmbiguousIsNotDurabilityUnconfirmed(t *testing.T) {
	amb := fmt.Errorf("%w: %w", ErrAmbiguous, &atomicfile.PostCommitSyncError{Path: "x", Err: errors.New("sync")})
	if IsDurabilityUnconfirmed(amb) {
		t.Fatal("an ambiguous outcome must not be reported as visible-but-unconfirmed durability")
	}
}

// A lock-release PostCommitError is durable success (the record is durably committed;
// only the lock release failed), distinct from the visible-but-unconfirmed durability
// condition — so it must not be classified as durability-unconfirmed.
func TestPostCommitErrorIsDurableNotUnconfirmed(t *testing.T) {
	pce := &PostCommitError{Generation: 5, Err: errors.New("release")}
	if IsDurabilityUnconfirmed(pce) {
		t.Fatal("a lock-release PostCommitError is durable success, not durability-unconfirmed")
	}
	if !pce.Committed() {
		t.Fatal("PostCommitError must report Committed")
	}
}

// ConfirmDurable retries a transient directory-sync failure and succeeds; a
// persistent failure is an error; a missing store is a failure, never a create.
func TestConfirmDurableRetriesThenSucceeds(t *testing.T) {
	s := newStore(t)
	appendConst(t, s, Head{}, "one")
	g, ok, err := Acquire(s.lockPath)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	calls := 0
	s.syncDir = func(string) error {
		calls++
		if calls <= 2 {
			return errors.New("transient dir sync")
		}
		return nil
	}
	if err := s.ConfirmDurable(g); err != nil {
		t.Fatalf("confirm should succeed after transient retries: %v", err)
	}
}

func TestConfirmDurablePersistentFailure(t *testing.T) {
	s := newStore(t)
	appendConst(t, s, Head{}, "one")
	g, ok, err := Acquire(s.lockPath)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	s.syncDir = func(string) error { return errors.New("persistent EIO") }
	if err := s.ConfirmDurable(g); err == nil {
		t.Fatal("confirm must fail when the directory sync never succeeds")
	}
}

func TestConfirmDurableMissingStoreIsFailureNotCreate(t *testing.T) {
	s := newStore(t)
	g, ok, err := Acquire(s.lockPath) // no append yet: the store dir does not exist
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	if err := s.ConfirmDurable(g); err == nil {
		t.Fatal("confirm must fail (never create) when the store dir is missing")
	}
	if _, serr := os.Stat(s.dir); !os.IsNotExist(serr) {
		t.Fatalf("confirm created the store dir: stat err = %v", serr)
	}
}

func TestOversizePayloadRejectedFirstGen(t *testing.T) {
	s := newStore(t)
	big := make([]byte, maxPayloadSize+1)
	if _, err := s.Append(Head{}, func(uint64, string) ([]byte, error) { return big, nil }); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, ok, _ := s.Latest(); ok {
		t.Fatalf("store is not empty after an oversize rejection")
	}
}

func TestOversizePayloadRejectedLaterGen(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	big := make([]byte, maxPayloadSize+1)
	if _, err := s.Append(r1.Head(), func(uint64, string) ([]byte, error) { return big, nil }); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got, _, _ := s.Latest(); got.Generation != 1 {
		t.Fatalf("head moved to %d after an oversize rejection", got.Generation)
	}
}

func TestWritePrecommitFailureOnEmptyStore(t *testing.T) {
	// Generation 1 is the bootstrap path for the catalog and run state: a clean
	// precommit failure on an empty store must be the original error, not ambiguous.
	s := newStore(t)
	sentinel := errors.New("precommit failure")
	s.write = func(string, []byte, os.FileMode) error { return sentinel } // no file written
	_, err := s.Append(Head{}, func(uint64, string) ([]byte, error) { return []byte("one"), nil })
	if !errors.Is(err, sentinel) || errors.Is(err, ErrAmbiguous) {
		t.Fatalf("empty-store precommit err = %v, want the original error and not ErrAmbiguous", err)
	}
	if _, ok, _ := s.Latest(); ok {
		t.Fatalf("store is not empty after a clean precommit failure")
	}
}

func TestReconcileCorruptAncestorIsAmbiguous(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	s.write = func(path string, data []byte, perm os.FileMode) error {
		_ = atomicfile.Write(path, data, perm)                    // write the exact candidate gen2...
		_ = os.WriteFile(genFile(s, 1), []byte("garbage"), 0o600) // ...but corrupt the root
		return errors.New("ambiguous")
	}
	if _, err := s.Append(r1.Head(), func(uint64, string) ([]byte, error) { return []byte("two"), nil }); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("corrupt-ancestor + exact-candidate err = %v, want ErrAmbiguous", err)
	}
}

func TestReconcileNoncanonicalNamespaceIsAmbiguous(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	s.write = func(path string, data []byte, perm os.FileMode) error {
		_ = atomicfile.Write(path, data, perm)
		_ = os.WriteFile(filepath.Join(s.dir, "bad"+genFileExt), []byte("x"), 0o600) // breaks the namespace
		return errors.New("ambiguous")
	}
	if _, err := s.Append(r1.Head(), func(uint64, string) ([]byte, error) { return []byte("two"), nil }); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("noncanonical + exact-candidate err = %v, want ErrAmbiguous", err)
	}
}

func TestReleaseFailureIsPostCommitError(t *testing.T) {
	s := newStore(t)
	s.release = func(g *Guard) error { _ = g.Release(); return errors.New("release failed") }
	rec, err := s.Append(Head{}, func(uint64, string) ([]byte, error) { return []byte("one"), nil })
	var pce *PostCommitError
	if !errors.As(err, &pce) {
		t.Fatalf("err = %v, want *PostCommitError", err)
	}
	if !pce.Committed() || rec.Generation != 1 {
		t.Fatalf("committed=%v gen=%d", pce.Committed(), rec.Generation)
	}
	if got, ok, _ := s.Latest(); !ok || got.Generation != 1 {
		t.Fatalf("record was not actually persisted")
	}
}

func TestInitTightensExistingBroadDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	gens := filepath.Join(dir, "gens")
	if err := os.MkdirAll(gens, 0o755); err != nil { // pre-existing broad directory
		t.Fatalf("pre-create: %v", err)
	}
	s := Open(gens, filepath.Join(dir, "run.lock"))
	appendConst(t, s, Head{}, "one")
	fi, _ := os.Stat(gens)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("existing dir perm = %o, want tightened to 700", fi.Mode().Perm())
	}
}

func TestWriteGarbageIsAmbiguous(t *testing.T) {
	s := newStore(t)
	s.write = func(path string, data []byte, perm os.FileMode) error {
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, []byte("garbage"), perm) // leaves an invalid file
		return errors.New("wrote garbage")
	}
	if _, err := s.Append(Head{}, func(uint64, string) ([]byte, error) { return []byte("one"), nil }); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("err = %v, want ErrAmbiguous", err)
	}
}

func TestPermsPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	s := newStore(t)
	appendConst(t, s, Head{}, "one")
	di, err := os.Stat(s.dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm = %o, want 700", di.Mode().Perm())
	}
	fi, err := os.Stat(genFile(s, 1))
	if err != nil {
		t.Fatalf("stat gen: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("record perm = %o, want 600", fi.Mode().Perm())
	}
}

func FuzzDecode(f *testing.F) {
	_, valid := encode(1, "", []byte("hi"))
	f.Add(valid)
	f.Add([]byte("garbage"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decode(1, data) // must never panic
	})
}
