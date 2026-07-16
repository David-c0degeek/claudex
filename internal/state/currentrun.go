package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

// CurrentRunVersion is the on-disk schema version of the active-run pointer.
const CurrentRunVersion = 1

// ErrUnsupportedCurrentRunSchema means the active-run pointer carries a schema
// version this build does not support.
var ErrUnsupportedCurrentRunSchema = errors.New("state: unsupported active-run schema version")

// CurrentRun is the repository's authoritative active-run pointer. The catalog is
// the full allocation history; this names the single run currently accepting
// attaches, plus the caller-stable operation id that bootstrapped it (so a lost
// response after a completed bootstrap returns the same run, never a forced
// replacement). It is a separate immutable-generation store under the repo lock.
type CurrentRun struct {
	SchemaVersion int    `json:"schema_version"`
	Revision      uint64 `json:"revision"`
	Active        bool   `json:"active"`
	RunID         string `json:"run_id"`
	RelDir        string `json:"rel_dir"`
	OperationID   string `json:"operation_id"`
	LeadSessionID string `json:"lead_session_id"`
	LeadAgent     Agent  `json:"lead_agent"`
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

// MutateLocked appends the next generation under an already-held guard.
func (s *CurrentRunStore) MutateLocked(g *genstore.Guard, expectedRevision uint64, fn func(next *CurrentRun) error) (CurrentRun, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return CurrentRun{}, err
	}
	var head genstore.Head
	if ok {
		p, derr := decodeCurrentRun(rec)
		if derr != nil {
			return CurrentRun{}, derr
		}
		if p.Revision != expectedRevision {
			return CurrentRun{}, fmt.Errorf("%w: expected %d, have %d", ErrRevisionConflict, expectedRevision, p.Revision)
		}
		head = rec.Head()
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
		if err := validateCurrentRun(next); err != nil {
			return nil, err
		}
		return json.Marshal(next)
	})
	if err != nil {
		return CurrentRun{}, err
	}
	return decodeCurrentRun(built)
}

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
		// A cleared pointer carries no run identity.
		if cr.RunID != "" || cr.RelDir != "" || cr.OperationID != "" || cr.LeadSessionID != "" || cr.LeadAgent != "" {
			return fmt.Errorf("an inactive active-run pointer must carry no run identity")
		}
		return nil
	}
	if !validRunID(cr.RunID) {
		return fmt.Errorf("active-run run_id is not canonical")
	}
	if !isLocalRelPath(cr.RelDir) {
		return fmt.Errorf("active-run rel_dir is not a canonical local path")
	}
	if !validRunID(cr.OperationID) {
		return fmt.Errorf("active-run operation_id is not canonical")
	}
	if !isSessionID(cr.LeadSessionID) {
		return fmt.Errorf("active-run lead_session_id is not a minted session id")
	}
	if cr.LeadAgent != AgentClaude && cr.LeadAgent != AgentCodex {
		return fmt.Errorf("active-run lead_agent is unknown")
	}
	return nil
}
