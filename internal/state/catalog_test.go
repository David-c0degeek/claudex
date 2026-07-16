package state

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newCatalog(t *testing.T) *CatalogStore {
	t.Helper()
	dir := t.TempDir()
	return OpenCatalog(filepath.Join(dir, "catalog"), filepath.Join(dir, "repo.lock"))
}

// ref builds a complete, valid run allocation.
func ref(id, dir string) RunRef {
	return RunRef{RunID: id, RelDir: dir, Base: "main", BaseCommit: strings.Repeat("a", 40), CreatedUnix: 100}
}

func TestCatalogAllocateAndLookup(t *testing.T) {
	c := newCatalog(t)
	if _, ok, err := c.Load(); err != nil || ok {
		t.Fatalf("empty catalog: ok=%v err=%v", ok, err)
	}
	cat1, err := c.Allocate(0, ref("run-a", "runs/run-a"))
	if err != nil {
		t.Fatalf("allocate a: %v", err)
	}
	cat2, err := c.Allocate(cat1.Revision, ref("run-b", "runs/run-b"))
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
	cat1, err := c.Allocate(0, ref("dup", "runs/dup"))
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if _, err := c.Allocate(cat1.Revision, ref("dup", "runs/dup2")); !errors.Is(err, ErrRunExists) {
		t.Fatalf("duplicate allocate err = %v, want ErrRunExists", err)
	}
}

func TestCatalogStaleRevisionConflicts(t *testing.T) {
	c := newCatalog(t)
	cat1, _ := c.Allocate(0, ref("a", "runs/a"))
	c.Allocate(cat1.Revision, ref("b", "runs/b")) // head is now revision 2
	if _, err := c.Allocate(cat1.Revision, ref("c", "runs/c")); !errors.Is(err, ErrRevisionConflict) {
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
	bad := []string{"../escape", "runs/../../../etc", ".", "runs/./x", "runs//x", "C:/outside", "C:outside", "//server/share", "runs\\x"}
	for _, d := range bad {
		if _, err := c.Allocate(0, RunRef{RunID: "r", RelDir: d}); err == nil {
			t.Fatalf("rel_dir %q should be rejected", d)
		}
	}
}

func TestCatalogRejectsIncompleteMetadata(t *testing.T) {
	c := newCatalog(t)
	oid := strings.Repeat("a", 40)
	bad := []RunRef{
		{RunID: "r", RelDir: "runs/r", Base: "main", CreatedUnix: 100},                  // no base_commit
		{RunID: "r", RelDir: "runs/r", BaseCommit: oid, CreatedUnix: 100},               // no base
		{RunID: "r", RelDir: "runs/r", Base: "main", BaseCommit: oid},                   // no created
		{RunID: "r", RelDir: "runs/r", Base: "main", BaseCommit: "xyz", CreatedUnix: 1}, // bad oid
	}
	for i, rr := range bad {
		if _, err := c.Allocate(0, rr); err == nil {
			t.Fatalf("incomplete metadata case %d should be rejected", i)
		}
	}
}

func TestCatalogRejectsBackslashLocator(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.Allocate(0, ref("r", `runs\x`)); err == nil {
		t.Fatalf("a backslash locator should be rejected (single forward-slash representation)")
	}
}

func TestCatalogCaseFoldedDuplicateLocator(t *testing.T) {
	c := newCatalog(t)
	cat1, err := c.Allocate(0, ref("a", "runs/shared"))
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if _, err := c.Allocate(cat1.Revision, ref("b", "runs/SHARED")); !errors.Is(err, ErrRunExists) {
		t.Fatalf("case-only locator duplicate err = %v, want ErrRunExists", err)
	}
}

func TestCatalogRejectsDuplicateRelDir(t *testing.T) {
	c := newCatalog(t)
	cat1, err := c.Allocate(0, ref("a", "runs/shared"))
	if err != nil {
		t.Fatalf("allocate a: %v", err)
	}
	if _, err := c.Allocate(cat1.Revision, ref("b", "runs/shared")); !errors.Is(err, ErrRunExists) {
		t.Fatalf("duplicate rel_dir err = %v, want ErrRunExists", err)
	}
}
