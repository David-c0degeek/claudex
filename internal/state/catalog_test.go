package state

import (
	"errors"
	"path/filepath"
	"testing"
)

func newCatalog(t *testing.T) *CatalogStore {
	t.Helper()
	dir := t.TempDir()
	return OpenCatalog(filepath.Join(dir, "catalog"), filepath.Join(dir, "repo.lock"))
}

func TestCatalogAllocateAndLookup(t *testing.T) {
	c := newCatalog(t)
	if _, ok, err := c.Load(); err != nil || ok {
		t.Fatalf("empty catalog: ok=%v err=%v", ok, err)
	}
	cat1, err := c.Allocate(0, RunRef{RunID: "run-a", RelDir: "runs/run-a", Base: "main", CreatedUnix: 100})
	if err != nil {
		t.Fatalf("allocate a: %v", err)
	}
	cat2, err := c.Allocate(cat1.Revision, RunRef{RunID: "run-b", RelDir: "runs/run-b", Base: "main", CreatedUnix: 200})
	if err != nil {
		t.Fatalf("allocate b: %v", err)
	}
	if cat2.Revision != 2 || len(cat2.Runs) != 2 {
		t.Fatalf("catalog after two allocations = %+v", cat2)
	}
	got, ok, _ := c.Load()
	if !ok {
		t.Fatalf("load")
	}
	ref, found := got.Lookup("run-a")
	if !found || ref.RelDir != "runs/run-a" {
		t.Fatalf("lookup run-a = %+v found=%v", ref, found)
	}
}

func TestCatalogRejectsDuplicateRunID(t *testing.T) {
	c := newCatalog(t)
	cat1, err := c.Allocate(0, RunRef{RunID: "dup", RelDir: "runs/dup"})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if _, err := c.Allocate(cat1.Revision, RunRef{RunID: "dup", RelDir: "runs/dup2"}); !errors.Is(err, ErrRunExists) {
		t.Fatalf("duplicate allocate err = %v, want ErrRunExists", err)
	}
}

func TestCatalogStaleRevisionConflicts(t *testing.T) {
	c := newCatalog(t)
	cat1, _ := c.Allocate(0, RunRef{RunID: "a", RelDir: "runs/a"})
	c.Allocate(cat1.Revision, RunRef{RunID: "b", RelDir: "runs/b"}) // head is now revision 2
	if _, err := c.Allocate(cat1.Revision, RunRef{RunID: "c", RelDir: "runs/c"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale allocate err = %v, want ErrRevisionConflict", err)
	}
}

func TestCatalogRequiresRunIDAndDir(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.Allocate(0, RunRef{RelDir: "x"}); err == nil {
		t.Fatalf("empty run id should be rejected")
	}
	if _, err := c.Allocate(0, RunRef{RunID: "x"}); err == nil {
		t.Fatalf("empty rel dir should be rejected")
	}
}

func TestCatalogRejectsTraversalLocators(t *testing.T) {
	c := newCatalog(t)
	bad := []string{"../escape", "runs/../../../etc", ".", "runs/./x", "runs//x"}
	for _, d := range bad {
		if _, err := c.Allocate(0, RunRef{RunID: "r", RelDir: d}); err == nil {
			t.Fatalf("rel_dir %q should be rejected", d)
		}
	}
}

func TestCatalogRejectsDuplicateRelDir(t *testing.T) {
	c := newCatalog(t)
	cat1, err := c.Allocate(0, RunRef{RunID: "a", RelDir: "runs/shared"})
	if err != nil {
		t.Fatalf("allocate a: %v", err)
	}
	if _, err := c.Allocate(cat1.Revision, RunRef{RunID: "b", RelDir: "runs/shared"}); !errors.Is(err, ErrRunExists) {
		t.Fatalf("duplicate rel_dir err = %v, want ErrRunExists", err)
	}
}
