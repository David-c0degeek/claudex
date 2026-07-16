package genstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/oslock"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "gens"), filepath.Join(dir, "run.lock"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// appendConst appends a fixed payload.
func appendConst(t *testing.T, s *Store, expected Head, payload string) Record {
	t.Helper()
	rec, err := s.Append(expected, func(next uint64, prev string) ([]byte, error) {
		return []byte(payload), nil
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return rec
}

func TestAppendSequenceAndLatest(t *testing.T) {
	s := newStore(t)

	if _, ok, err := s.Latest(); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v, want false/nil", ok, err)
	}

	r1 := appendConst(t, s, Head{}, "one")
	if r1.Generation != 1 || r1.PrevDigest != "" {
		t.Fatalf("r1 = %+v, want gen 1 empty prev", r1)
	}
	r2 := appendConst(t, s, r1.Head(), "two")
	if r2.Generation != 2 || r2.PrevDigest != r1.Digest {
		t.Fatalf("r2 chain broken: %+v (prev want %s)", r2, r1.Digest)
	}
	r3 := appendConst(t, s, r2.Head(), "three")

	got, ok, err := s.Latest()
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if got.Generation != 3 || string(got.Payload) != "three" || got.Digest != r3.Digest {
		t.Fatalf("Latest = %+v, want gen 3 'three'", got)
	}
}

func TestBuilderReceivesGenerationAndPrev(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")

	var gotNext uint64
	var gotPrev string
	_, err := s.Append(r1.Head(), func(next uint64, prev string) ([]byte, error) {
		gotNext, gotPrev = next, prev
		return fmt.Appendf(nil, "gen-%d", next), nil
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if gotNext != 2 || gotPrev != r1.Digest {
		t.Fatalf("builder got next=%d prev=%s, want 2/%s", gotNext, gotPrev, r1.Digest)
	}
}

func TestAppendConflict(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	appendConst(t, s, r1.Head(), "two") // head is now gen 2

	// Appending against the stale gen-1 head must conflict.
	_, err := s.Append(r1.Head(), func(uint64, string) ([]byte, error) { return []byte("x"), nil })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale append err = %v, want ErrConflict", err)
	}
}

func TestBusyWhenLocked(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "run.lock")
	s, err := Open(filepath.Join(dir, "gens"), lockPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	held, ok, err := oslock.TryAcquire(lockPath)
	if err != nil || !ok {
		t.Fatalf("hold lock: ok=%v err=%v", ok, err)
	}
	defer held.Release()

	_, err = s.Append(Head{}, func(uint64, string) ([]byte, error) { return []byte("x"), nil })
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("append while locked err = %v, want ErrBusy", err)
	}
}

func genFile(s *Store, gen uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%012d%s", gen, genFileExt))
}

func TestTornHighestFallsBackToOlderHead(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	r2 := appendConst(t, s, r1.Head(), "two")
	appendConst(t, s, r2.Head(), "three")

	// Corrupt the newest generation (simulate a torn write).
	if err := os.WriteFile(genFile(s, 3), []byte("garbage-not-a-valid-record"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	got, ok, err := s.Latest()
	if err != nil || !ok {
		t.Fatalf("Latest after torn head: ok=%v err=%v", ok, err)
	}
	if got.Generation != 2 {
		t.Fatalf("Latest = gen %d, want the older valid head gen 2", got.Generation)
	}

	// Appending against the older head must skip the occupied slot 3 and land at 4.
	r4 := appendConst(t, s, r2.Head(), "four")
	if r4.Generation != 4 {
		t.Fatalf("append after torn slot 3 landed at gen %d, want 4 (gap allowed)", r4.Generation)
	}
	if r4.PrevDigest != r2.Digest {
		t.Fatalf("r4 prev = %s, want r2 %s", r4.PrevDigest, r2.Digest)
	}
}

func TestAllInvalidIsCorrupt(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	// Corrupt the only record.
	if err := os.WriteFile(genFile(s, r1.Generation), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, _, err := s.Latest(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("all-invalid Latest err = %v, want ErrCorrupt", err)
	}
}

func TestBrokenChainIsCorrupt(t *testing.T) {
	s := newStore(t)
	r1 := appendConst(t, s, Head{}, "one")
	appendConst(t, s, r1.Head(), "two")

	// Replace generation 1 with a DIFFERENT valid record so gen 2's prev no
	// longer matches — a self-intact but chain-inconsistent state.
	_, forged := encode(1, "", []byte("forged-one"))
	if err := os.WriteFile(genFile(s, 1), forged, 0o644); err != nil {
		t.Fatalf("forge: %v", err)
	}
	if _, _, err := s.Latest(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("broken-chain Latest err = %v, want ErrCorrupt", err)
	}
}

func TestDecodeRejectsTornRecord(t *testing.T) {
	_, file := encode(5, "prev", []byte("payload"))
	// Flip a byte in the payload region; the trailer checksum must catch it.
	file[len(file)-trailerLen-1] ^= 0xff
	if _, ok := decode(5, file); ok {
		t.Fatalf("decode accepted a tampered record")
	}
	// Wrong expected generation is also rejected.
	_, clean := encode(5, "prev", []byte("payload"))
	if _, ok := decode(6, clean); ok {
		t.Fatalf("decode accepted a generation mismatch")
	}
}
