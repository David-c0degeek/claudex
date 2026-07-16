package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/redact"
)

// CatalogVersion is the on-disk schema version of the repository run catalog.
const CatalogVersion = 1

// ErrRunExists is returned when allocating a run id or directory already present.
var ErrRunExists = errors.New("state: run already allocated")

// RunRef is an immutable run allocation: which relative directory holds run_id.
// It carries NO run status — status is derived from each run's own state, never
// stored here, so the catalog cannot race a separately-committed run store.
type RunRef struct {
	RunID       string `json:"run_id"`
	RelDir      string `json:"rel_dir"`
	Base        string `json:"base"`
	BaseCommit  string `json:"base_commit"`
	CreatedUnix int64  `json:"created_unix"`
}

// Catalog is the repository-level allocation catalog, persisted as immutable
// generations under the repository lock.
type Catalog struct {
	SchemaVersion int      `json:"schema_version"`
	Revision      uint64   `json:"revision"`
	Runs          []RunRef `json:"runs"`
}

// CatalogStore persists the catalog over a genstore under the repository lock.
type CatalogStore struct {
	gs *genstore.Store
}

// OpenCatalog returns a catalog store handle (side-effect-free). lockPath must be
// the repository-level allocation lock, distinct from any per-run lock.
func OpenCatalog(dir, lockPath string) *CatalogStore {
	return &CatalogStore{gs: genstore.Open(dir, lockPath)}
}

// LockPath is the repository lock guarding this catalog.
func (c *CatalogStore) LockPath() string { return c.gs.LockPath() }

// Load returns the current catalog, strictly decoded and validated.
func (c *CatalogStore) Load() (Catalog, bool, error) {
	rec, ok, err := c.gs.Latest()
	if err != nil {
		return Catalog{}, false, err
	}
	if !ok {
		return Catalog{}, false, nil
	}
	cat, err := decodeCatalog(rec)
	if err != nil {
		return Catalog{}, false, err
	}
	return cat, true, nil
}

// Allocate appends a new run allocation as a compare-and-swap against
// expectedRevision. It acquires and releases the repository guard itself.
func (c *CatalogStore) Allocate(expectedRevision uint64, ref RunRef) (Catalog, error) {
	g, ok, err := genstore.Acquire(c.gs.LockPath())
	if err != nil {
		return Catalog{}, err
	}
	if !ok {
		return Catalog{}, genstore.ErrBusy
	}
	cat, aerr := c.AllocateLocked(g, expectedRevision, ref)
	if rerr := g.Release(); aerr == nil && rerr != nil {
		return cat, &genstore.PostCommitError{Generation: cat.Revision, Err: rerr}
	}
	return cat, aerr
}

// AllocateLocked appends a run allocation under an already-held repository guard.
func (c *CatalogStore) AllocateLocked(g *genstore.Guard, expectedRevision uint64, ref RunRef) (Catalog, error) {
	if err := validateRunRef(ref); err != nil {
		return Catalog{}, err
	}

	rec, ok, err := c.gs.Latest()
	if err != nil {
		return Catalog{}, err
	}

	var cur Catalog
	var head genstore.Head
	if ok {
		cur, err = decodeCatalog(rec)
		if err != nil {
			return Catalog{}, err
		}
		if cur.Revision != expectedRevision {
			return Catalog{}, fmt.Errorf("%w: expected %d, have %d", ErrRevisionConflict, expectedRevision, cur.Revision)
		}
		head = rec.Head()
	} else if expectedRevision != 0 {
		return Catalog{}, fmt.Errorf("%w: expected %d on an empty catalog", ErrRevisionConflict, expectedRevision)
	}

	for _, r := range cur.Runs {
		if strings.EqualFold(r.RunID, ref.RunID) {
			return Catalog{}, fmt.Errorf("%w: run_id %s", ErrRunExists, ref.RunID)
		}
		if locatorKey(r.RelDir) == locatorKey(ref.RelDir) {
			return Catalog{}, fmt.Errorf("%w: rel_dir %s", ErrRunExists, ref.RelDir)
		}
	}

	next := cur
	next.Runs = append(append([]RunRef(nil), cur.Runs...), ref)

	built, err := c.gs.AppendLocked(g, head, func(gen uint64, _ string) ([]byte, error) {
		next.Revision = gen
		next.SchemaVersion = CatalogVersion
		if err := validateCatalog(&next); err != nil {
			return nil, err
		}
		raw, merr := json.Marshal(next)
		if merr != nil {
			return nil, merr
		}
		return raw, nil
	})
	if err != nil {
		return Catalog{}, err
	}
	return decodeCatalog(built)
}

// Lookup returns the run ref for run_id, if allocated.
func (cat Catalog) Lookup(runID string) (RunRef, bool) {
	for _, r := range cat.Runs {
		if r.RunID == runID {
			return r, true
		}
	}
	return RunRef{}, false
}

func validateRunRef(ref RunRef) error {
	if !validRunID(ref.RunID) {
		return fmt.Errorf("catalog: invalid run_id %q", ref.RunID)
	}
	// RelDir is an authoritative locator: reject traversal/absolute/non-canonical.
	if !isLocalRelPath(ref.RelDir) {
		return fmt.Errorf("catalog: rel_dir %q is not a canonical local path", ref.RelDir)
	}
	// The allocation is authoritative immutable metadata, not a partial cache:
	// the base was already resolved by the first attach.
	if strings.TrimSpace(ref.Base) == "" {
		return fmt.Errorf("catalog: base is required")
	}
	if !isGitOID(ref.BaseCommit) {
		return fmt.Errorf("catalog: base_commit is not a git object id (40 or 64 lower-hex)")
	}
	if ref.CreatedUnix <= 0 {
		return fmt.Errorf("catalog: created_unix must be positive")
	}
	// A secret must never be laundered through an allocation field.
	if redact.Text(ref.RunID) != ref.RunID || redact.Text(ref.RelDir) != ref.RelDir || redact.Text(ref.Base) != ref.Base {
		return fmt.Errorf("catalog: a secret was detected in a run allocation field")
	}
	return nil
}

func validateCatalog(cat *Catalog) error {
	if cat.SchemaVersion != CatalogVersion {
		return fmt.Errorf("catalog: schema_version %d != %d", cat.SchemaVersion, CatalogVersion)
	}
	if cat.Revision == 0 {
		return fmt.Errorf("catalog: revision must be > 0")
	}
	ids := map[string]bool{}
	dirs := map[string]bool{}
	for _, r := range cat.Runs {
		if err := validateRunRef(r); err != nil {
			return err
		}
		idKey := strings.ToLower(r.RunID)
		dirKey := locatorKey(r.RelDir)
		if ids[idKey] {
			return fmt.Errorf("catalog: duplicate run_id %s", r.RunID)
		}
		if dirs[dirKey] {
			return fmt.Errorf("catalog: duplicate rel_dir %s", r.RelDir)
		}
		ids[idKey] = true
		dirs[dirKey] = true
	}
	return nil
}

func decodeCatalog(rec genstore.Record) (Catalog, error) {
	dec := json.NewDecoder(bytes.NewReader(rec.Payload))
	dec.DisallowUnknownFields()
	var cat Catalog
	if err := dec.Decode(&cat); err != nil {
		return Catalog{}, fmt.Errorf("catalog: decode generation %d: %w", rec.Generation, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Catalog{}, fmt.Errorf("catalog: unexpected trailing content in generation %d", rec.Generation)
	}
	if cat.Runs == nil {
		cat.Runs = []RunRef{}
	}
	if cat.Revision != rec.Generation {
		return Catalog{}, fmt.Errorf("catalog: revision %d disagrees with generation %d", cat.Revision, rec.Generation)
	}
	if err := validateCatalog(&cat); err != nil {
		return Catalog{}, err
	}
	return cat, nil
}
