package state

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/David-c0degeek/claudex/internal/redact"
)

const (
	sessionIDPrefix   = "sess-"
	operationIDPrefix = "op-"
)

// isMintedID enforces an EXACT coordinator-minted grammar: a fixed prefix + 32
// lowercase hex. These ids become directory names and gate authority, so the
// general mixed-case id grammar is too loose — on a case-insensitive filesystem
// `Sess-X`/`sess-x` would alias, and a guessable id could release an incumbent
// session. Lowercase-only makes equality and uniqueness case-safe.
func isMintedID(prefix, s string) bool {
	if len(s) != len(prefix)+32 {
		return false
	}
	if s[:len(prefix)] != prefix {
		return false
	}
	for _, c := range s[len(prefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isSessionID(s string) bool   { return isMintedID(sessionIDPrefix, s) }
func isOperationID(s string) bool { return isMintedID(operationIDPrefix, s) }

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
		if !isSessionID(s.SessionID) {
			return fmt.Errorf("%s slot session %d id is not a canonical minted session id", role, i)
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

// validateRegistryInit enforces the only legal first generation: the lead slot
// filled with exactly one generation-1 session bound to this revision, pair empty.
func validateRegistryInit(r *Registry) error {
	if r.Lead == nil || r.Pair != nil {
		return fmt.Errorf("initial registry must have the lead slot filled and the pair empty")
	}
	if len(r.Lead.Sessions) != 1 || r.Lead.Sessions[0].Generation != 1 {
		return fmt.Errorf("initial registry lead must have exactly one generation-1 session")
	}
	if r.Lead.Sessions[0].IssuedRegistryRevision != r.Revision {
		return fmt.Errorf("initial registry lead session must bind to revision %d", r.Revision)
	}
	return nil
}

// validateRegistrytransition enforces run-id immutability and that every registry
// revision records EXACTLY ONE attach or replacement: a slot either gains one new
// generation (bound to this revision) or is byte-identical to its old value, and
// the total new sessions across both slots is exactly one — so a bulk append, a
// two-slot change, and a no-op generation are all rejected, and issued revisions
// strictly increase per slot.
func validateRegistryTransition(old, next *Registry) error {
	if old.RunID != next.RunID {
		return fmt.Errorf("registry run_id is immutable")
	}
	leadAdded, err := slotTransition(SlotLead, old.Lead, next.Lead, next.Revision)
	if err != nil {
		return err
	}
	pairAdded, err := slotTransition(SlotPair, old.Pair, next.Pair, next.Revision)
	if err != nil {
		return err
	}
	if leadAdded+pairAdded != 1 {
		return fmt.Errorf("a registry mutation must record exactly one attach/replacement, got %d", leadAdded+pairAdded)
	}
	return nil
}

// slotTransition returns the number of newly appended sessions (0 or 1) and
// enforces the per-slot rules: agent immutable, no clear, existing records
// frozen, an unchanged slot byte-identical, and at most one new record bound to
// this revision.
func slotTransition(role SlotRole, old, next *RoleSlot, rev uint64) (int, error) {
	if old == nil {
		if next == nil {
			return 0, nil
		}
		if len(next.Sessions) != 1 {
			return 0, fmt.Errorf("%s slot must be filled with exactly one session", role)
		}
		if next.Sessions[0].IssuedRegistryRevision != rev {
			return 0, fmt.Errorf("%s slot: a newly registered session must bind to registry revision %d", role, rev)
		}
		return 1, nil
	}
	if next == nil {
		return 0, fmt.Errorf("%s slot cannot be cleared once filled", role)
	}
	if old.Agent != next.Agent {
		return 0, fmt.Errorf("%s slot agent is immutable", role)
	}
	if len(next.Sessions) < len(old.Sessions) {
		return 0, fmt.Errorf("%s slot history must not shrink", role)
	}
	for i := range old.Sessions {
		if next.Sessions[i] != old.Sessions[i] {
			return 0, fmt.Errorf("%s slot history is append-only", role)
		}
	}
	switch added := len(next.Sessions) - len(old.Sessions); added {
	case 0:
		if !reflect.DeepEqual(old, next) {
			return 0, fmt.Errorf("%s slot changed without adding a session", role)
		}
		return 0, nil
	case 1:
		if next.Sessions[len(next.Sessions)-1].IssuedRegistryRevision != rev {
			return 0, fmt.Errorf("%s slot: a newly registered session must bind to registry revision %d", role, rev)
		}
		return 1, nil
	default:
		return 0, fmt.Errorf("%s slot appended %d sessions; exactly one attach/replacement per revision", role, added)
	}
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

// ErrMintExhausted means id minting could not find an unused id after its bounded
// attempts (neutral across session and operation ids).
var ErrMintExhausted = errors.New("state: id minting exhausted attempts")

// MintSessionID generates a fresh, canonical, secret-free session id from rng,
// retrying on a collision reported by taken, and failing closed on an RNG error.
// taken should report whether an id already exists in either slot history.
func MintSessionID(rng io.Reader, taken func(string) bool) (string, error) {
	return mintID(sessionIDPrefix, rng, taken)
}

// MintOperationID mints a caller-stable idempotency key ("op-" + 32 lower-hex).
// The CLI mints one per attach invocation and reuses it across retries; equality
// releases the incumbent lead session, so it must be unguessable.
func MintOperationID(rng io.Reader) (string, error) {
	return mintID(operationIDPrefix, rng, nil)
}

func mintID(prefix string, rng io.Reader, taken func(string) bool) (string, error) {
	if rng == nil {
		return "", fmt.Errorf("state: mint %sid: nil RNG", prefix)
	}
	for attempt := 0; attempt < 8; attempt++ {
		var b [16]byte
		if _, err := io.ReadFull(rng, b[:]); err != nil {
			return "", fmt.Errorf("state: mint %sid: %w", prefix, err)
		}
		id := prefix + hex.EncodeToString(b[:])
		if !isMintedID(prefix, id) || redact.Text(id) != id {
			continue // grammar/secret guard (asserted; unreachable for lower-hex)
		}
		if taken != nil && taken(id) {
			continue
		}
		return id, nil
	}
	return "", ErrMintExhausted
}
