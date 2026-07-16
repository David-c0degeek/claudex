package state

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/David-c0degeek/claudex/internal/redact"
)

// strictDecodeRegistry decodes exactly one JSON value with no unknown fields and
// no trailing content.
func strictDecodeRegistry(data []byte) (Registry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Registry
	if err := dec.Decode(&r); err != nil {
		return Registry{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return Registry{}, fmt.Errorf("unexpected trailing content")
	}
	return r, nil
}

// validateRegistry enforces the invariants that hold for every persisted registry.
func validateRegistry(r *Registry) error {
	if r.SchemaVersion != RegistryVersion {
		return fmt.Errorf("registry schema_version %d != %d", r.SchemaVersion, RegistryVersion)
	}
	if !validRunID(r.RunID) {
		return fmt.Errorf("registry run_id %q is not canonical", r.RunID)
	}
	if r.Revision == 0 {
		return fmt.Errorf("registry revision must be > 0")
	}
	// Session ids are globally unique across BOTH slot histories, so a superseded
	// id resolves to exactly one slot.
	seen := make(map[string]SlotRole)
	if err := validateSlot(SlotLead, r.Lead, r.Revision, seen); err != nil {
		return err
	}
	if err := validateSlot(SlotPair, r.Pair, r.Revision, seen); err != nil {
		return err
	}
	if r.Lead != nil && r.Pair != nil && r.Lead.Agent == r.Pair.Agent {
		return fmt.Errorf("lead and pair must be distinct agents")
	}
	return nil
}

func validateSlot(role SlotRole, slot *RoleSlot, rev uint64, seen map[string]SlotRole) error {
	if slot == nil {
		return nil
	}
	if slot.Agent != AgentClaude && slot.Agent != AgentCodex {
		return fmt.Errorf("%s slot has an unknown agent %q", role, slot.Agent)
	}
	if len(slot.Sessions) == 0 {
		return fmt.Errorf("%s slot is present but has no sessions", role)
	}
	var prevIssued uint64
	for i, s := range slot.Sessions {
		if !validID(s.SessionID) {
			return fmt.Errorf("%s slot session %d id is not canonical", role, i)
		}
		if s.Generation != uint64(i+1) {
			return fmt.Errorf("%s slot generations must be consecutive from 1", role)
		}
		if s.IssuedRegistryRevision == 0 || s.IssuedRegistryRevision > rev {
			return fmt.Errorf("%s slot session %d issued_registry_revision %d out of range (1..%d)", role, i, s.IssuedRegistryRevision, rev)
		}
		if s.IssuedRegistryRevision < prevIssued {
			return fmt.Errorf("%s slot session issued revisions must not decrease", role)
		}
		prevIssued = s.IssuedRegistryRevision
		if prior, dup := seen[s.SessionID]; dup {
			return fmt.Errorf("%s slot session id collides with the %s history", role, prior)
		}
		seen[s.SessionID] = role
	}
	if slot.CurrentSessionID != slot.Sessions[len(slot.Sessions)-1].SessionID {
		return fmt.Errorf("%s slot current_session_id must be the last session", role)
	}
	return nil
}

// validateRegistryTransition enforces run-id immutability and append-only,
// agent-immutable slot history.
func validateRegistryTransition(old, next *Registry) error {
	if old.RunID != next.RunID {
		return fmt.Errorf("registry run_id is immutable")
	}
	if err := slotAppendOnly(SlotLead, old.Lead, next.Lead, next.Revision); err != nil {
		return err
	}
	if err := slotAppendOnly(SlotPair, old.Pair, next.Pair, next.Revision); err != nil {
		return err
	}
	return nil
}

func slotAppendOnly(role SlotRole, old, next *RoleSlot, rev uint64) error {
	if old == nil {
		// A first registration is fine; every record it adds binds to this revision.
		if next != nil {
			for i := range next.Sessions {
				if next.Sessions[i].IssuedRegistryRevision != rev {
					return fmt.Errorf("%s slot: a newly registered session must bind to registry revision %d", role, rev)
				}
			}
		}
		return nil
	}
	if next == nil {
		return fmt.Errorf("%s slot cannot be cleared once filled", role)
	}
	if old.Agent != next.Agent {
		return fmt.Errorf("%s slot agent is immutable", role)
	}
	if len(next.Sessions) < len(old.Sessions) {
		return fmt.Errorf("%s slot history must not shrink", role)
	}
	// Existing records are frozen; only appended ones are new.
	for i := range old.Sessions {
		if next.Sessions[i] != old.Sessions[i] {
			return fmt.Errorf("%s slot history is append-only", role)
		}
	}
	for i := len(old.Sessions); i < len(next.Sessions); i++ {
		if next.Sessions[i].IssuedRegistryRevision != rev {
			return fmt.Errorf("%s slot: a newly registered session must bind to registry revision %d", role, rev)
		}
	}
	return nil
}

// registryGuard rejects a secret in any registry control field (a session id,
// agent, or run id must never carry a credential).
func registryGuard(r *Registry) error {
	control := map[string]string{"run_id": r.RunID}
	add := func(role SlotRole, slot *RoleSlot) {
		if slot == nil {
			return
		}
		control[string(role)+".agent"] = string(slot.Agent)
		control[string(role)+".current_session_id"] = slot.CurrentSessionID
		for i, s := range slot.Sessions {
			control[fmt.Sprintf("%s.sessions.%d", role, i)] = s.SessionID
		}
	}
	add(SlotLead, r.Lead)
	add(SlotPair, r.Pair)
	for field, v := range control {
		if redact.Text(v) != v {
			return fmt.Errorf("state: a secret was detected in registry control field %s", field)
		}
	}
	return nil
}

// ErrSessionIDExhausted means fresh session-id minting could not find an unused id.
var ErrSessionIDExhausted = errors.New("state: session id minting exhausted attempts")

// MintSessionID generates a fresh, canonical, secret-free session id from rng,
// retrying on a collision reported by taken, and failing closed on an RNG error.
// taken should report whether an id already exists in either slot history.
func MintSessionID(rng io.Reader, taken func(string) bool) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var b [16]byte
		if _, err := io.ReadFull(rng, b[:]); err != nil {
			return "", fmt.Errorf("state: mint session id: %w", err)
		}
		id := "sess-" + hex.EncodeToString(b[:])
		if !validID(id) || redact.Text(id) != id {
			continue // grammar/secret guard (asserted; unreachable for hex)
		}
		if taken != nil && taken(id) {
			continue
		}
		return id, nil
	}
	return "", ErrSessionIDExhausted
}
