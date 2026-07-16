// Package transport defines the client-facing protocol messages over the
// role-addressed file mailbox. This file holds the assignment contract returned
// by pull: a read-only, phase/role-specific projection of run state that never
// mutates and never mints identity.
package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/redact"
	"github.com/David-c0degeek/claudex/internal/state"
)

const assignmentType = "assignment"

var (
	// ErrNoActiveTurn means state has no issued turn to project; pull never
	// creates one.
	ErrNoActiveTurn = errors.New("transport: no active turn to pull")
	// ErrStaleAssignment means the issued turn is not bound to the current state
	// revision. The engine must reissue an assignment on every intervening
	// mutation.
	ErrStaleAssignment = errors.New("transport: assignment is not bound to the current revision")
	// ErrWorkspaceMismatch means the workspace does not match what the role and
	// phase require (a clean worktree only for a lead edit turn, hash-bound
	// evidence otherwise).
	ErrWorkspaceMismatch = errors.New("transport: workspace does not match the role and phase")
	// ErrPhaseNotActionable means the phase issues no agent assignment.
	ErrPhaseNotActionable = errors.New("transport: phase issues no assignment")
	// ErrTurnSpecMismatch means the role or artifact type disagrees with the
	// authoritative per-phase turn spec.
	ErrTurnSpecMismatch = errors.New("transport: role or artifact type disagrees with the phase turn spec")
	// ErrAssignmentInvalid means a built or supplied assignment violates a
	// semantic invariant that the JSON schema alone cannot express.
	ErrAssignmentInvalid = errors.New("transport: assignment is semantically invalid")
)

// Role is which side an assignment addresses.
type Role string

const (
	RoleLead Role = "lead"
	RolePair Role = "pair"
)

// TurnSpecEntry is the authoritative role and artifact contract for a phase.
type TurnSpecEntry struct {
	Role                Role
	ArtifactMessageType string
}

// turnSpecs is the single transport-owned source of which role acts and which
// artifact a submit produces for each actionable phase. The engine (03) consumes
// this rather than duplicating it. INIT, AWAIT_GUIDANCE, DONE, and TESTS issue
// no assignment and are absent here.
var turnSpecs = map[state.Phase]TurnSpecEntry{
	state.PhasePlanDraft:     {RoleLead, "plan"},
	state.PhasePlanCritique:  {RolePair, "plan_critique"},
	state.PhasePlanRevise:    {RoleLead, "plan_revision"},
	state.PhaseImplementStep: {RoleLead, "implementation_report"},
	state.PhaseCheckpoint:    {RolePair, "checkpoint_review"},
	state.PhaseFix:           {RoleLead, "implementation_report"},
	state.PhaseVerify:        {RolePair, "verification"},
}

// TurnSpec returns the authoritative role and artifact message type for a phase,
// and whether the phase issues an assignment at all.
func TurnSpec(p state.Phase) (TurnSpecEntry, bool) {
	e, ok := turnSpecs[p]
	return e, ok
}

// editPhases are the phases in which the lead may mutate the repository. This
// must stay aligned with the repo-edit policy enforced at submit time (03.7).
var editPhases = map[state.Phase]bool{
	state.PhaseImplementStep: true,
	state.PhaseFix:           true,
}

// EditableTurn reports whether an assignment for this role and phase carries a
// mutable worktree: only a lead turn in an edit phase does. 03.7 calls this to
// enforce the same predicate at submit time.
func EditableTurn(role Role, p state.Phase) bool {
	return role == RoleLead && editPhases[p]
}

// EvidenceRef is a hash-bound, immutable evidence locator for a read-only phase:
// a canonical run-relative manifest path plus the lower-hex sha256 that binds its
// content. It never exposes a live worktree path.
type EvidenceRef struct {
	ManifestRelPath string `json:"manifest_rel_path"`
	RootDigest      string `json:"root_digest"`
}

// PullInputs are the assignment fields the engine resolves around run state.
// Every value MUST be read from persisted state/artifacts at the same revision
// as rs — never from ambient config or the current filesystem — so the
// projection stays deterministic (this obligation is the engine's, subjects
// 03/05). Exactly one of Worktree or Evidence is set, per EditableTurn.
type PullInputs struct {
	SessionID       string
	Role            Role
	BindingGuidance []string
	Worktree        *string
	Evidence        *EvidenceRef
}

// Assignment is the read-only, phase/role-specific contract pull returns. It
// carries the exact schema a submit must satisfy, so it is a complete provider
// instruction. Worktree and Evidence are mutually exclusive; exactly one is set.
type Assignment struct {
	ProtocolVersion       int          `json:"protocol_version"`
	MessageType           string       `json:"message_type"`
	RunID                 string       `json:"run_id"`
	SessionID             string       `json:"session_id"`
	TurnID                string       `json:"turn_id"`
	Role                  Role         `json:"role"`
	Phase                 state.Phase  `json:"phase"`
	ExpectedStateRevision uint64       `json:"expected_state_revision"`
	BindingGuidance       []string     `json:"binding_guidance"`
	ArtifactMessageType   string       `json:"artifact_message_type"`
	ArtifactSchemaJSON    string       `json:"artifact_schema_json"`
	ArtifactSchemaSHA256  string       `json:"artifact_schema_sha256"`
	Worktree              *string      `json:"worktree"`
	Evidence              *EvidenceRef `json:"evidence"`
}

// BuildAssignment projects a read-only assignment from run state. It never
// mutates rs or in, never mints identity (the turn_id is the one the preceding
// transition issued, bound to the current revision), redacts binding guidance,
// and fully validates the result. Two pulls of the same state produce equal
// assignments.
func BuildAssignment(rs state.RunState, in PullInputs) (Assignment, error) {
	if rs.Assignment == nil || rs.Assignment.ID == "" {
		return Assignment{}, ErrNoActiveTurn
	}
	spec, ok := TurnSpec(rs.Phase)
	if !ok {
		return Assignment{}, fmt.Errorf("%w: %s", ErrPhaseNotActionable, rs.Phase)
	}
	if rs.Assignment.IssuedRevision != rs.Revision {
		return Assignment{}, fmt.Errorf("%w: turn issued at %d, state at %d", ErrStaleAssignment, rs.Assignment.IssuedRevision, rs.Revision)
	}
	if in.SessionID == "" {
		return Assignment{}, fmt.Errorf("transport: session_id is required")
	}

	schemaBytes, err := protocol.Schema(spec.ArtifactMessageType, protocol.SupportedVersion)
	if err != nil {
		return Assignment{}, fmt.Errorf("transport: artifact schema: %w", err)
	}
	sum := sha256.Sum256(schemaBytes)

	a := Assignment{
		ProtocolVersion:       protocol.SupportedVersion,
		MessageType:           assignmentType,
		RunID:                 rs.RunID,
		SessionID:             in.SessionID,
		TurnID:                rs.Assignment.ID,
		Role:                  in.Role,
		Phase:                 rs.Phase,
		ExpectedStateRevision: rs.Assignment.IssuedRevision,
		BindingGuidance:       redactGuidance(in.BindingGuidance),
		ArtifactMessageType:   spec.ArtifactMessageType,
		ArtifactSchemaJSON:    string(schemaBytes),
		ArtifactSchemaSHA256:  hex.EncodeToString(sum[:]),
	}
	if EditableTurn(in.Role, rs.Phase) {
		if in.Worktree != nil {
			wt := *in.Worktree
			a.Worktree = &wt
		}
	} else if in.Evidence != nil {
		ev := *in.Evidence // copy so caller mutation cannot alter the assignment
		a.Evidence = &ev
	}

	if err := a.Validate(); err != nil {
		return Assignment{}, err
	}
	return a, nil
}

// redactGuidance clones and redacts each guidance string, so a raw secret in an
// input never reaches the assignment and the caller's slice is untouched.
func redactGuidance(g []string) []string {
	out := make([]string, 0, len(g))
	for _, s := range g {
		out = append(out, redact.Text(s))
	}
	return out
}

// Validate enforces the semantic invariants the JSON schema cannot express, so a
// mutated or hand-built assignment cannot slip past Marshal. It re-fetches the
// registry schema and requires exact bytes + digest, enforces the phase turn
// spec (role and artifact type), enforces the workspace XOR by EditableTurn, and
// validates the evidence/worktree grammar.
func (a Assignment) Validate() error {
	if a.ProtocolVersion != protocol.SupportedVersion {
		return fmt.Errorf("%w: wrong protocol_version", ErrAssignmentInvalid)
	}
	if a.MessageType != assignmentType {
		return fmt.Errorf("%w: wrong message_type", ErrAssignmentInvalid)
	}
	if a.RunID == "" || a.SessionID == "" || a.TurnID == "" || a.ExpectedStateRevision == 0 {
		return fmt.Errorf("%w: missing identity fields", ErrAssignmentInvalid)
	}

	spec, ok := TurnSpec(a.Phase)
	if !ok {
		return fmt.Errorf("%w: %s", ErrPhaseNotActionable, a.Phase)
	}
	if a.Role != spec.Role {
		return fmt.Errorf("%w: %s is a %s turn, got %s", ErrTurnSpecMismatch, a.Phase, spec.Role, a.Role)
	}
	if a.ArtifactMessageType != spec.ArtifactMessageType {
		return fmt.Errorf("%w: %s expects artifact %q, got %q", ErrTurnSpecMismatch, a.Phase, spec.ArtifactMessageType, a.ArtifactMessageType)
	}

	schemaBytes, err := protocol.Schema(a.ArtifactMessageType, protocol.SupportedVersion)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAssignmentInvalid, err)
	}
	if a.ArtifactSchemaJSON != string(schemaBytes) {
		return fmt.Errorf("%w: artifact_schema_json is not the exact embedded schema", ErrAssignmentInvalid)
	}
	sum := sha256.Sum256([]byte(a.ArtifactSchemaJSON))
	if a.ArtifactSchemaSHA256 != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%w: artifact_schema_sha256 does not match the schema bytes", ErrAssignmentInvalid)
	}

	for _, g := range a.BindingGuidance {
		if g == "" {
			return fmt.Errorf("%w: empty binding guidance", ErrAssignmentInvalid)
		}
		if redact.Text(g) != g {
			return fmt.Errorf("%w: binding guidance is not redacted", ErrAssignmentInvalid)
		}
	}

	if EditableTurn(a.Role, a.Phase) {
		if a.Worktree == nil || a.Evidence != nil {
			return fmt.Errorf("%w: an edit turn needs a worktree only", ErrWorkspaceMismatch)
		}
		return validateWorktree(*a.Worktree)
	}
	if a.Evidence == nil || a.Worktree != nil {
		return fmt.Errorf("%w: a read-only turn needs evidence only", ErrWorkspaceMismatch)
	}
	return validateEvidence(*a.Evidence)
}

// validateEvidence reuses the state store's canonical run-relative locator and
// lower-hex digest grammar rather than a divergent copy.
func validateEvidence(e EvidenceRef) error {
	if !state.IsLocalRelPath(e.ManifestRelPath) {
		return fmt.Errorf("%w: evidence manifest path is not a canonical run-relative path", ErrWorkspaceMismatch)
	}
	if !state.IsHex64(e.RootDigest) {
		return fmt.Errorf("%w: evidence root digest is not a lower-hex sha256", ErrWorkspaceMismatch)
	}
	return nil
}

// validateWorktree requires a clean absolute platform path free of NUL, control
// characters, and secret-bearing values. Subject 03 later verifies it belongs to
// the allocated run.
func validateWorktree(p string) error {
	if p == "" || !filepath.IsAbs(p) {
		return fmt.Errorf("%w: worktree must be a clean absolute path", ErrWorkspaceMismatch)
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("%w: worktree path is not clean", ErrWorkspaceMismatch)
	}
	for _, r := range p {
		if r == 0 || r < 0x20 {
			return fmt.Errorf("%w: worktree path has a NUL or control character", ErrWorkspaceMismatch)
		}
	}
	if redact.Text(p) != p {
		return fmt.Errorf("%w: worktree path looks secret-bearing", ErrWorkspaceMismatch)
	}
	return nil
}

// Marshal validates the assignment (semantic invariants) and its schema, and
// returns the canonical wire bytes so two pulls are byte-identical.
func (a Assignment) Marshal() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("transport: marshal assignment: %w", err)
	}
	canon, err := protocol.Validate(assignmentType, raw)
	if err != nil {
		return nil, fmt.Errorf("transport: assignment fails its schema: %w", err)
	}
	return canon, nil
}
