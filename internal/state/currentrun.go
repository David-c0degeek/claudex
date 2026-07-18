package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/redact"
)

// CurrentRunVersion is the on-disk schema version of the active-run pointer.
const CurrentRunVersion = 1

// ErrUnsupportedCurrentRunSchema means the active-run pointer carries a schema
// version this build does not support.
var ErrUnsupportedCurrentRunSchema = errors.New("state: unsupported active-run schema version")

// ErrCurrentRunMismatch means a Clear did not name the actual active run.
var ErrCurrentRunMismatch = errors.New("state: active-run clear does not match the active run")

// CurrentRun is the repository's authoritative active-run pointer: the single run
// currently accepting attaches, plus the caller-stable operation id that
// bootstrapped it. It holds ONLY run/locator/operation activation identity —
// never a session or agent, which are the Registry's sole truth (a copied session
// here would go stale on a same-role replacement). It is a separate
// immutable-generation store under the repo lock.
type CurrentRun struct {
	SchemaVersion int    `json:"schema_version"`
	Revision      uint64 `json:"revision"`
	Active        bool   `json:"active"`
	RunID         string `json:"run_id"`
	RelDir        string `json:"rel_dir"`
	OperationID   string `json:"operation_id"`
}

// CurrentRunStore persists the active-run pointer over a genstore. It shares the
// repo lock so it composes with the catalog and the run stores under one guard.
type CurrentRunStore struct {
	gs *genstore.Store
}

// OpenCurrentRun returns an active-run pointer store handle.
func OpenCurrentRun(dir, lockPath string) *CurrentRunStore {
	return &CurrentRunStore{gs: genstore.Open(dir, lockPath)}
}

// LockPath is the mutation lock guarding this store.
func (s *CurrentRunStore) LockPath() string { return s.gs.LockPath() }

// Load returns the current active-run pointer. (_, false, nil) means none yet.
func (s *CurrentRunStore) Load() (CurrentRun, bool, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil || !ok {
		return CurrentRun{}, false, err
	}
	cr, err := decodeCurrentRun(rec)
	if err != nil {
		return CurrentRun{}, false, err
	}
	return cr, true, nil
}

// Activate sets the active-run pointer to a NEW run under a held guard, CASing
// against the expected pointer revision so a later run activates only after a
// prior one was cleared.
func (s *CurrentRunStore) Activate(g *genstore.Guard, expectedRevision uint64, runID, relDir, operationID string) (CurrentRun, error) {
	return s.mutate(g, expectedRevision, func(next *CurrentRun) error {
		next.Active = true
		next.RunID = runID
		next.RelDir = relDir
		next.OperationID = operationID
		return nil
	})
}

// Clear deactivates the pointer, binding the expected active run id AND revision,
// so only the actual active run can be cleared (the caller must have confirmed
// its RunState is terminal first).
func (s *CurrentRunStore) Clear(g *genstore.Guard, expectedRevision uint64, expectedRunID string) (CurrentRun, error) {
	cur, ok, err := s.Load()
	if err != nil {
		return CurrentRun{}, err
	}
	if !ok || !cur.Active || cur.RunID != expectedRunID {
		return CurrentRun{}, fmt.Errorf("%w: %s", ErrCurrentRunMismatch, expectedRunID)
	}
	return s.mutate(g, expectedRevision, func(next *CurrentRun) error {
		next.Active = false
		return nil
	})
}

func (s *CurrentRunStore) mutate(g *genstore.Guard, expectedRevision uint64, fn func(next *CurrentRun) error) (CurrentRun, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return CurrentRun{}, err
	}
	var head genstore.Head
	var prev *CurrentRun
	if ok {
		p, derr := decodeCurrentRun(rec)
		if derr != nil {
			return CurrentRun{}, derr
		}
		if p.Revision != expectedRevision {
			return CurrentRun{}, fmt.Errorf("%w: expected %d, have %d", ErrRevisionConflict, expectedRevision, p.Revision)
		}
		head = rec.Head()
		prev = &p
	} else if expectedRevision != 0 {
		return CurrentRun{}, fmt.Errorf("%w: expected %d on an empty active-run store", ErrRevisionConflict, expectedRevision)
	}

	built, err := s.gs.AppendLocked(g, head, func(gen uint64, _ string) ([]byte, error) {
		next := &CurrentRun{}
		if err := fn(next); err != nil {
			return nil, err
		}
		next.Revision = gen
		next.SchemaVersion = CurrentRunVersion
		if err := currentRunGuard(next); err != nil {
			return nil, err
		}
		if err := validateCurrentRun(next); err != nil {
			return nil, err
		}
		if prev != nil {
			if err := validateCurrentRunTransition(prev, next); err != nil {
				return nil, err
			}
		}
		return json.Marshal(next)
	})
	if err != nil {
		if genstore.IsDurabilityUnconfirmed(err) {
			cr, derr := decodeCurrentRun(built)
			if derr != nil {
				return CurrentRun{}, derr
			}
			return cr, err
		}
		return CurrentRun{}, err
	}
	return decodeCurrentRun(built)
}

// ConfirmDurable re-confirms this store's directory is power-safe under the held
// guard (see genstore.Store.ConfirmDurable).
func (s *CurrentRunStore) ConfirmDurable(g *genstore.Guard) error { return s.gs.ConfirmDurable(g) }

func decodeCurrentRun(rec genstore.Record) (CurrentRun, error) {
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(rec.Payload, &probe); err != nil {
		return CurrentRun{}, fmt.Errorf("%w: unreadable", ErrUnsupportedCurrentRunSchema)
	}
	if probe.SchemaVersion != CurrentRunVersion {
		return CurrentRun{}, fmt.Errorf("%w: on-disk %d, expected %d", ErrUnsupportedCurrentRunSchema, probe.SchemaVersion, CurrentRunVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(rec.Payload))
	dec.DisallowUnknownFields()
	var cr CurrentRun
	if err := dec.Decode(&cr); err != nil {
		return CurrentRun{}, fmt.Errorf("state: decode active-run generation %d: %w", rec.Generation, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return CurrentRun{}, fmt.Errorf("state: trailing content in active-run generation %d", rec.Generation)
	}
	if cr.Revision != rec.Generation {
		return CurrentRun{}, fmt.Errorf("state: active-run revision %d disagrees with generation %d", cr.Revision, rec.Generation)
	}
	if err := validateCurrentRun(&cr); err != nil {
		return CurrentRun{}, err
	}
	return cr, nil
}

func validateCurrentRun(cr *CurrentRun) error {
	if cr.SchemaVersion != CurrentRunVersion {
		return fmt.Errorf("active-run schema_version %d != %d", cr.SchemaVersion, CurrentRunVersion)
	}
	if cr.Revision == 0 {
		return fmt.Errorf("active-run revision must be > 0")
	}
	if !cr.Active {
		if cr.RunID != "" || cr.RelDir != "" || cr.OperationID != "" {
			return fmt.Errorf("an inactive active-run pointer must carry no run identity")
		}
		return nil
	}
	if !validRunID(cr.RunID) {
		return fmt.Errorf("active-run run_id is not canonical")
	}
	if cr.RelDir != RunDirRelFor(cr.RunID) {
		return fmt.Errorf("active-run rel_dir must be the derived %q", RunDirRelFor(cr.RunID))
	}
	if !isOperationID(cr.OperationID) {
		return fmt.Errorf("active-run operation_id is not a minted operation id")
	}
	return nil
}

// validateCurrentRunTransition enforces that a mutation flips Active: an active
// run may be cleared (its RunState reached terminal), and an inactive/absent
// pointer may activate a NEW run. A double-set or double-clear is a no-op
// generation and rejected.
func validateCurrentRunTransition(old, next *CurrentRun) error {
	if old.Active == next.Active {
		if old.Active {
			return fmt.Errorf("an active run must be cleared before another is activated")
		}
		return fmt.Errorf("the active-run pointer is already inactive")
	}
	return nil
}

// currentRunGuard rejects a secret in any active-run control field.
func currentRunGuard(cr *CurrentRun) error {
	for field, v := range map[string]string{
		"run_id":       cr.RunID,
		"rel_dir":      cr.RelDir,
		"operation_id": cr.OperationID,
	} {
		if redact.Text(v) != v {
			return fmt.Errorf("state: a secret was detected in active-run field %s", field)
		}
	}
	return nil
}
