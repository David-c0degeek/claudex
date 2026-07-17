package txn

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

// A journal's CheckGuard proves the supplied guard is the live lock for this journal:
// nil/released guards and a guard for a different lock are rejected, so a reader
// cannot read the head under a guard it does not actually hold.
func TestJournalCheckGuard(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "run.lock")
	j := Open(filepath.Join(dir, "journal"), lock)

	if err := j.CheckGuard(nil); err == nil {
		t.Fatal("nil guard should be rejected")
	}

	g, ok, err := genstore.Acquire(lock)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := j.CheckGuard(g); err != nil {
		t.Fatalf("live guard rejected: %v", err)
	}

	other := filepath.Join(dir, "other.lock")
	g2, ok, err := genstore.Acquire(other)
	if err != nil || !ok {
		t.Fatalf("acquire other: ok=%v err=%v", ok, err)
	}
	if err := j.CheckGuard(g2); !errors.Is(err, genstore.ErrWrongLock) {
		t.Fatalf("wrong-lock guard err = %v, want ErrWrongLock", err)
	}
	if err := g2.Release(); err != nil {
		t.Fatalf("release other: %v", err)
	}

	if err := g.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := j.CheckGuard(g); err == nil {
		t.Fatal("released guard should be rejected")
	}
}
