package state

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

// RegistryVersion is the on-disk schema version of the registration store.
const RegistryVersion = 1

// ErrUnsupportedRegistrySchema means a persisted registry generation carries a
// schema version this build does not support.
var ErrUnsupportedRegistrySchema = errors.New("state: unsupported on-disk registry schema version")

// Agent is which interactive tool holds a role.
type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

// SlotRole is which side of the pairing a registration fills.
type SlotRole string

const (
	SlotLead SlotRole = "lead"
	SlotPair SlotRole = "pair"
)

// RegStatus classifies a session id against the durable registration history.
type RegStatus string

const (
	// RegCurrent means the id is the live session for its slot.
	RegCurrent RegStatus = "current"
	// RegReplaced means the id was a real session that a later generation
	// superseded — so a superseded TUI is told it was replaced, never mistaken
	// for an unknown/bogus session.
	RegReplaced RegStatus = "replaced"
	// RegUnknown means the id was never registered in either slot.
	RegUnknown RegStatus = "unknown"
)

// SessionRecord is one registration of a role slot. Sessions are append-only and
// generations are consecutive from 1; the last record is the current session.
type SessionRecord struct {
	SessionID              string `json:"session_id"`
	Generation             uint64 `json:"generation"`
	IssuedRegistryRevision uint64 `json:"issued_registry_revision"`
}

// RoleSlot is a filled role: the immutable agent that holds it, its current
// session id, and the append-only history that lets a superseded session be
// distinguished from an unknown one.
type RoleSlot struct {
	Agent            Agent           `json:"agent"`
	CurrentSessionID string          `json:"current_session_id"`
	Sessions         []SessionRecord `json:"sessions"`
}

// Registry is the durable role-registration store for a run. It is persisted as
// immutable generations under the SAME per-run lock as RunState, but as a
// SEPARATE store, so a same-role session replacement can supersede a crashed TUI
// WITHOUT advancing RunState.Revision (which would invalidate the other role's
// live, revision-bound assignment). Absent slots are nil. RunID binds it to the
// run; a cross-store consumer must confirm Registry.RunID == RunState.RunID
// (sharing a lock path alone does not bind the two identities).
type Registry struct {
	SchemaVersion int       `json:"schema_version"`
	RunID         string    `json:"run_id"`
	Revision      uint64    `json:"revision"`
	Lead          *RoleSlot `json:"lead"`
	Pair          *RoleSlot `json:"pair"`
}

// Registration is the value-only resolution of a session id: the single fact
// source the submit authorizer, wait, and status all consult instead of a
// divergent lookup. For a known id it carries the slot's role, agent, and the
// slot's current generation/session; Status says whether the id itself is
// current, replaced, or unknown.
type Registration struct {
	Role              SlotRole
	Agent             Agent
	Status            RegStatus
	CurrentGeneration uint64
	CurrentSessionID  string
}

// Resolve classifies sessionID against the durable history. Session ids are
// globally unique across both slots, so at most one slot matches.
func (r Registry) Resolve(sessionID string) Registration {
	if reg, ok := resolveInSlot(SlotLead, r.Lead, sessionID); ok {
		return reg
	}
	if reg, ok := resolveInSlot(SlotPair, r.Pair, sessionID); ok {
		return reg
	}
	return Registration{Status: RegUnknown}
}

func resolveInSlot(role SlotRole, slot *RoleSlot, sessionID string) (Registration, bool) {
	if slot == nil {
		return Registration{}, false
	}
	cur := slot.currentGeneration()
	if slot.CurrentSessionID == sessionID {
		return Registration{Role: role, Agent: slot.Agent, Status: RegCurrent, CurrentGeneration: cur, CurrentSessionID: slot.CurrentSessionID}, true
	}
	for _, s := range slot.Sessions {
		if s.SessionID == sessionID {
			return Registration{Role: role, Agent: slot.Agent, Status: RegReplaced, CurrentGeneration: cur, CurrentSessionID: slot.CurrentSessionID}, true
		}
	}
	return Registration{}, false
}

func (slot *RoleSlot) currentGeneration() uint64 {
	if len(slot.Sessions) == 0 {
		return 0
	}
	return slot.Sessions[len(slot.Sessions)-1].Generation
}

func (r Registry) slot(role SlotRole) *RoleSlot {
	if role == SlotLead {
		return r.Lead
	}
	return r.Pair
}

// RegistryStore is the registration store over a genstore. It shares the run's
// mutation lock with the run-state store (pass the same lockPath) so a caller can
// mutate both under one guard via MutateLocked.
type RegistryStore struct {
	gs *genstore.Store
}

// OpenRegistry returns a registration store handle (side-effect-free). lockPath
// MUST be the same lock the run-state store uses.
func OpenRegistry(dir, lockPath string) *RegistryStore {
	return &RegistryStore{gs: genstore.Open(dir, lockPath).WithRetention(stateRetentionKeep, stateRetentionTrigger)}
}

// LockPath is the mutation lock guarding this store.
func (s *RegistryStore) LockPath() string { return s.gs.LockPath() }

// Load returns the current registry, strictly decoded and fully validated.
// (_, false, nil) means no registry exists yet.
func (s *RegistryStore) Load() (Registry, bool, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return Registry{}, false, err
	}
	if !ok {
		return Registry{}, false, nil
	}
	reg, err := decodeRegistry(rec)
	if err != nil {
		return Registry{}, false, err
	}
	return reg, true, nil
}

// Mutate applies fn as a compare-and-swap against expectedRevision, appending the
// next generation. It acquires and releases the guard itself; use MutateLocked to
// compose with the run-state store under one guard.
func (s *RegistryStore) Mutate(expectedRevision uint64, fn func(nextRevision uint64, next *Registry) error) (Registry, error) {
	g, ok, err := genstore.Acquire(s.gs.LockPath())
	if err != nil {
		return Registry{}, err
	}
	if !ok {
		return Registry{}, genstore.ErrBusy
	}
	reg, merr := s.MutateLocked(g, expectedRevision, fn)
	if rerr := g.Release(); merr == nil && rerr != nil {
		return reg, &genstore.PostCommitError{Generation: reg.Revision, Err: rerr}
	}
	return reg, merr
}

// MutateLocked appends the next generation under an already-held guard, so a
// caller can mutate the registry and the run state serialized under one lock.
// This composes the two writers; it does NOT make them crash-atomic (a crash
// between the two appends leaves one committed) — cross-store atomicity is the
// prepared-transaction journal's job at the attach layer.
func (s *RegistryStore) MutateLocked(g *genstore.Guard, expectedRevision uint64, fn func(nextRevision uint64, next *Registry) error) (Registry, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return Registry{}, err
	}

	var prev *Registry
	var head genstore.Head
	if ok {
		p, derr := decodeRegistry(rec)
		if derr != nil {
			return Registry{}, derr
		}
		if p.Revision != expectedRevision {
			return Registry{}, fmt.Errorf("%w: expected %d, have %d", ErrRevisionConflict, expectedRevision, p.Revision)
		}
		prev = &p
		head = rec.Head()
	} else if expectedRevision != 0 {
		return Registry{}, fmt.Errorf("%w: expected %d on an empty registry", ErrRevisionConflict, expectedRevision)
	}

	built, err := s.gs.AppendLocked(g, head, func(gen uint64, _ string) ([]byte, error) {
		next := cloneRegistryForNext(prev)
		next.Revision = gen
		next.SchemaVersion = RegistryVersion

		if err := fn(gen, next); err != nil {
			return nil, err
		}
		// The callback must not poison the generation identity: a changed revision
		// or schema version would be committed here and only caught post-commit by
		// decodeRegistry (bricking the head), so reject it before serialization.
		if next.Revision != gen {
			return nil, fmt.Errorf("registry mutation must not change the revision (want %d)", gen)
		}
		if next.SchemaVersion != RegistryVersion {
			return nil, fmt.Errorf("registry mutation must not change the schema version")
		}
		if err := registryGuard(next); err != nil {
			return nil, err
		}
		if err := validateRegistry(next); err != nil {
			return nil, err
		}
		if prev == nil {
			if err := validateRegistryInit(next); err != nil {
				return nil, err
			}
		} else if err := validateRegistryTransition(prev, next); err != nil {
			return nil, err
		}
		return json.Marshal(next)
	})
	if err != nil {
		if genstore.IsDurabilityUnconfirmed(err) {
			reg, derr := decodeRegistry(built)
			if derr != nil {
				return Registry{}, derr
			}
			return reg, err
		}
		return Registry{}, err
	}
	return decodeRegistry(built)
}

// ConfirmDurable re-confirms this store's directory is power-safe under the held
// guard (see genstore.Store.ConfirmDurable).
func (s *RegistryStore) ConfirmDurable(g *genstore.Guard) error { return s.gs.ConfirmDurable(g) }

// cloneRegistryForNext deep-copies prev (or a fresh registry) so the mutator sees
// usable slices and transition validation can compare without aliasing.
func cloneRegistryForNext(prev *Registry) *Registry {
	if prev == nil {
		return &Registry{}
	}
	n := *prev
	n.Lead = cloneSlot(prev.Lead)
	n.Pair = cloneSlot(prev.Pair)
	return &n
}

func cloneSlot(s *RoleSlot) *RoleSlot {
	if s == nil {
		return nil
	}
	c := *s
	c.Sessions = append([]SessionRecord(nil), s.Sessions...)
	return &c
}

// decodeRegistry strictly decodes and fully validates a persisted record.
func decodeRegistry(rec genstore.Record) (Registry, error) {
	if err := checkRegistrySchemaVersion(rec.Payload); err != nil {
		return Registry{}, fmt.Errorf("state: registry generation %d: %w", rec.Generation, err)
	}
	reg, err := strictDecodeRegistry(rec.Payload)
	if err != nil {
		return Registry{}, fmt.Errorf("state: decode registry generation %d: %w", rec.Generation, err)
	}
	if reg.Revision != rec.Generation {
		return Registry{}, fmt.Errorf("state: registry revision %d disagrees with generation %d", reg.Revision, rec.Generation)
	}
	if err := validateRegistry(&reg); err != nil {
		return Registry{}, fmt.Errorf("state: registry generation %d invalid: %w", rec.Generation, err)
	}
	return reg, nil
}

func checkRegistrySchemaVersion(payload []byte) error {
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return fmt.Errorf("%w: schema_version is unreadable", ErrUnsupportedRegistrySchema)
	}
	if probe.SchemaVersion != RegistryVersion {
		return fmt.Errorf("%w: on-disk registry schema_version %d, this build expects %d", ErrUnsupportedRegistrySchema, probe.SchemaVersion, RegistryVersion)
	}
	return nil
}
