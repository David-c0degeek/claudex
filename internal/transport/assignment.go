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

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

const assignmentType = "assignment"

var (
	// ErrNoActiveTurn means state has no issued turn to project; pull never
	// creates one.
	ErrNoActiveTurn = errors.New("transport: no active turn to pull")
	// ErrStaleAssignment means the issued turn is not bound to the current state
	// revision, so projecting it would produce a turn that drifts across
	// mutations. The engine must reissue an assignment on every intervening
	// mutation.
	ErrStaleAssignment = errors.New("transport: assignment is not bound to the current revision")
	// ErrWorkspaceMismatch means the supplied workspace does not match what the
	// role and phase require (a worktree only for a lead edit phase, immutable
	// evidence otherwise).
	ErrWorkspaceMismatch = errors.New("transport: workspace does not match the role and phase")
	// ErrPhaseNotActionable means the phase issues no agent assignment.
	ErrPhaseNotActionable = errors.New("transport: phase issues no assignment")
)

// Role is which side an assignment addresses.
type Role string

const (
	RoleLead Role = "lead"
	RolePair Role = "pair"
)

// actionablePhases are the phases that issue an agent assignment. INIT,
// AWAIT_GUIDANCE, DONE, and TESTS deliberately issue none.
var actionablePhases = map[state.Phase]bool{
	state.PhasePlanDraft:     true,
	state.PhasePlanCritique:  true,
	state.PhasePlanRevise:    true,
	state.PhaseImplementStep: true,
	state.PhaseCheckpoint:    true,
	state.PhaseFix:           true,
	state.PhaseVerify:        true,
}

// editPhases are the phases in which the lead may mutate the repository; a lead
// assignment for one of these carries a mutable worktree. This must stay aligned
// with the repo-edit policy (the lead edits only during IMPLEMENT_STEP/FIX);
// every other phase, and every pair turn, is read-only.
var editPhases = map[state.Phase]bool{
	state.PhaseImplementStep: true,
	state.PhaseFix:           true,
}

// EditableTurn reports whether an assignment for this role and phase carries a
// mutable worktree: only a lead turn in an edit phase does.
func EditableTurn(role Role, p state.Phase) bool {
	return role == RoleLead && editPhases[p]
}

// EvidenceRef is a hash-bound, immutable evidence locator for a read-only phase:
// a canonical run-relative manifest path plus the digest that binds its content.
// It never exposes a live worktree path.
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
	SessionID           string
	Role                Role
	BindingGuidance     []string
	ArtifactMessageType string
	Worktree            *string
	Evidence            *EvidenceRef
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
// mutates rs or in, and never mints identity: the turn_id is the one the
// preceding transition durably issued, bound to the current revision. Two pulls
// of the same state produce equal assignments.
func BuildAssignment(rs state.RunState, in PullInputs) (Assignment, error) {
	if rs.Assignment == nil || rs.Assignment.ID == "" {
		return Assignment{}, ErrNoActiveTurn
	}
	if !actionablePhases[rs.Phase] {
		return Assignment{}, fmt.Errorf("%w: %s", ErrPhaseNotActionable, rs.Phase)
	}
	// Bind the assignment to exactly one state snapshot: the turn must have been
	// issued at the current revision, else the same turn id could yield a
	// different assignment after an unrelated mutation.
	if rs.Assignment.IssuedRevision != rs.Revision {
		return Assignment{}, fmt.Errorf("%w: turn issued at %d, state at %d", ErrStaleAssignment, rs.Assignment.IssuedRevision, rs.Revision)
	}
	if in.Role != RoleLead && in.Role != RolePair {
		return Assignment{}, fmt.Errorf("transport: invalid role %q", in.Role)
	}
	if in.SessionID == "" {
		return Assignment{}, fmt.Errorf("transport: session_id is required")
	}

	// The artifact schema must already exist in the registry; embed its exact
	// bytes + digest so the assignment is a complete instruction.
	schemaBytes, err := protocol.Schema(in.ArtifactMessageType, protocol.SupportedVersion)
	if err != nil {
		return Assignment{}, fmt.Errorf("transport: artifact schema: %w", err)
	}

	editable := EditableTurn(in.Role, rs.Phase)
	if err := validateWorkspace(editable, in); err != nil {
		return Assignment{}, err
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
		BindingGuidance:       append([]string{}, in.BindingGuidance...), // clone; never nil
		ArtifactMessageType:   in.ArtifactMessageType,
		ArtifactSchemaJSON:    string(schemaBytes),
		ArtifactSchemaSHA256:  hex.EncodeToString(sum[:]),
	}
	if editable {
		wt := *in.Worktree
		a.Worktree = &wt
	} else {
		ev := *in.Evidence // copy so caller mutation cannot alter the assignment
		a.Evidence = &ev
	}
	return a, nil
}

func validateWorkspace(editable bool, in PullInputs) error {
	if editable {
		if in.Worktree == nil || *in.Worktree == "" || in.Evidence != nil {
			return fmt.Errorf("%w: a lead edit phase needs a worktree only", ErrWorkspaceMismatch)
		}
		return nil
	}
	if in.Worktree != nil || in.Evidence == nil {
		return fmt.Errorf("%w: a read-only phase needs immutable evidence only", ErrWorkspaceMismatch)
	}
	if in.Evidence.ManifestRelPath == "" || len(in.Evidence.RootDigest) != 64 {
		return fmt.Errorf("%w: evidence needs a manifest path and a 64-hex digest", ErrWorkspaceMismatch)
	}
	return nil
}

// Marshal serializes the assignment and validates it against the embedded
// assignment schema (the same bytes handed to a provider), returning the
// canonical wire bytes so two pulls are byte-identical.
func (a Assignment) Marshal() ([]byte, error) {
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
