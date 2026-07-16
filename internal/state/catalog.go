package state

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/redact"
)

// CatalogVersion is the on-disk schema version of the repository run catalog.
const CatalogVersion = 1

// ErrRunExists is returned when allocating a run id that is already present.
var ErrRunExists = errors.New("state: run id already allocated")

// RunRef is an immutable run allocation: which relative directory holds run_id.
// It deliberately carries NO run status — status is derived from each run's own
// state, never stored here, so the catalog cannot race a separately-committed run
// store.
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

// Load returns the current catalog. (_, false, nil) means no catalog exists yet.
func (c *CatalogStore) Load() (Catalog, bool, error) {
	rec, ok, err := c.gs.Latest()
	if err != nil {
		return Catalog{}, false, err
	}
	if !ok {
		return Catalog{}, false, nil
	}
	cat, err := unmarshalCatalog(rec)
	if err != nil {
		return Catalog{}, false, err
	}
	return cat, true, nil
}

// Allocate appends a new run allocation as a compare-and-swap against
// expectedRevision. It rejects a duplicate run id. It acquires and releases the
// repository guard itself; use AllocateLocked to compose under a held guard.
func (c *CatalogStore) Allocate(expectedRevision uint64, ref RunRef) (Catalog, error) {
	g, ok, err := genstore.Acquire(c.gs.LockPath())
	if err != nil {
		return Catalog{}, err
	}
	if !ok {
		return Catalog{}, genstore.ErrBusy
	}
	cat, aerr := c.AllocateLocked(g, expectedRevision, ref)
	rerr := g.Release()
	if aerr == nil && rerr != nil {
		return cat, fmt.Errorf("catalog generation %d committed but lock release failed: %w", cat.Revision, rerr)
	}
	return cat, aerr
}

// AllocateLocked appends a run allocation under an already-held repository guard.
func (c *CatalogStore) AllocateLocked(g *genstore.Guard, expectedRevision uint64, ref RunRef) (Catalog, error) {
	if ref.RunID == "" || ref.RelDir == "" {
		return Catalog{}, fmt.Errorf("catalog: run_id and rel_dir are required")
	}

	rec, ok, err := c.gs.Latest()
	if err != nil {
		return Catalog{}, err
	}

	var cur Catalog
	var head genstore.Head
	if ok {
		cur, err = unmarshalCatalog(rec)
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
		if r.RunID == ref.RunID {
			return Catalog{}, fmt.Errorf("%w: %s", ErrRunExists, ref.RunID)
		}
	}

	next := cur
	next.Runs = append(append([]RunRef(nil), cur.Runs...), ref)

	built, err := c.gs.AppendLocked(g, head, func(gen uint64, _ string) ([]byte, error) {
		next.Revision = gen
		next.SchemaVersion = CatalogVersion
		raw, merr := json.Marshal(next)
		if merr != nil {
			return nil, merr
		}
		return redact.Bytes(raw), nil
	})
	if err != nil {
		return Catalog{}, err
	}
	next.Revision = built.Generation
	return next, nil
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

func unmarshalCatalog(rec genstore.Record) (Catalog, error) {
	var cat Catalog
	if err := json.Unmarshal(rec.Payload, &cat); err != nil {
		return Catalog{}, fmt.Errorf("catalog: decode generation %d: %w", rec.Generation, err)
	}
	if cat.SchemaVersion != CatalogVersion {
		return Catalog{}, fmt.Errorf("catalog: unsupported schema_version %d (want %d)", cat.SchemaVersion, CatalogVersion)
	}
	if cat.Revision != rec.Generation {
		return Catalog{}, fmt.Errorf("catalog: revision %d disagrees with generation %d", cat.Revision, rec.Generation)
	}
	return cat, nil
}
