// Package transport defines the client-facing protocol messages over the
// role-addressed file mailbox. This file holds the assignment contract returned
// by pull: a read-only, phase/role-specific projection of run state that never
// mutates and never mints identity.
package transport

import (
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
	// ErrWorkspaceMismatch means the supplied workspace does not match what the
	// phase requires (worktree for an edit phase, evidence otherwise).
	ErrWorkspaceMismatch = errors.New("transport: workspace does not match the phase")
	// ErrPhaseNotActionable means the phase issues no agent assignment.
	ErrPhaseNotActionable = errors.New("transport: phase issues no assignment")
)

// Role is which side an assignment addresses.
type Role string

const (
	RoleLead Role = "lead"
	RolePair Role = "pair"
)

// actionablePhases are the phases that issue an agent assignment.
var actionablePhases = map[state.Phase]bool{
	state.PhasePlanDraft:     true,
	state.PhasePlanCritique:  true,
	state.PhasePlanRevise:    true,
	state.PhaseImplementStep: true,
	state.PhaseCheckpoint:    true,
	state.PhaseFix:           true,
	state.PhaseVerify:        true,
}

// editPhases are the phases in which the lead mutates the repository; their
// assignments carry a mutable worktree. Every other actionable phase carries an
// immutable evidence root. This matches the repo-edit policy (lead edits only
// during IMPLEMENT_STEP/FIX).
var editPhases = map[state.Phase]bool{
	state.PhaseImplementStep: true,
	state.PhaseFix:           true,
}

// PhaseEdits reports whether an assignment for phase carries a mutable worktree.
func PhaseEdits(p state.Phase) bool { return editPhases[p] }

// Workspace is exactly one of a mutable worktree path (edit phases) or an
// immutable evidence-root digest (all other phases). BuildAssignment enforces
// which one is required for the phase.
type Workspace struct {
	Worktree     string
	EvidenceRoot string
}

// PullInputs are the assignment fields the engine resolves around run state:
// the addressed session, the role that owns this turn (the phase->role table is
// the engine's), any binding guidance, the message type a submit must be, and
// the workspace.
type PullInputs struct {
	SessionID           string
	Role                Role
	BindingGuidance     []string
	ArtifactMessageType string
	Workspace           Workspace
}

// Assignment is the read-only, phase/role-specific contract pull returns. The
// worktree and evidence_root fields are mutually exclusive per phase; exactly
// one is non-nil.
type Assignment struct {
	ProtocolVersion       int         `json:"protocol_version"`
	MessageType           string      `json:"message_type"`
	RunID                 string      `json:"run_id"`
	SessionID             string      `json:"session_id"`
	TurnID                string      `json:"turn_id"`
	Role                  Role        `json:"role"`
	Phase                 state.Phase `json:"phase"`
	ExpectedStateRevision uint64      `json:"expected_state_revision"`
	BindingGuidance       []string    `json:"binding_guidance"`
	ArtifactMessageType   string      `json:"artifact_message_type"`
	Worktree              *string     `json:"worktree"`
	EvidenceRoot          *string     `json:"evidence_root"`
}

// BuildAssignment projects a read-only assignment from run state. It never
// mutates rs and never mints identity: the turn_id is the one the preceding
// transition durably issued (rs.Assignment). It is deterministic, so two pulls
// of the same state produce equal assignments.
func BuildAssignment(rs state.RunState, in PullInputs) (Assignment, error) {
	if rs.Assignment == nil || rs.Assignment.ID == "" {
		return Assignment{}, ErrNoActiveTurn
	}
	if !actionablePhases[rs.Phase] {
		return Assignment{}, fmt.Errorf("%w: %s", ErrPhaseNotActionable, rs.Phase)
	}
	if in.Role != RoleLead && in.Role != RolePair {
		return Assignment{}, fmt.Errorf("transport: invalid role %q", in.Role)
	}
	if in.SessionID == "" {
		return Assignment{}, fmt.Errorf("transport: session_id is required")
	}
	if in.ArtifactMessageType == "" {
		return Assignment{}, fmt.Errorf("transport: artifact_message_type is required")
	}

	edits := PhaseEdits(rs.Phase)
	hasWorktree := in.Workspace.Worktree != ""
	hasEvidence := in.Workspace.EvidenceRoot != ""
	if edits && (!hasWorktree || hasEvidence) {
		return Assignment{}, fmt.Errorf("%w: %s needs a worktree only", ErrWorkspaceMismatch, rs.Phase)
	}
	if !edits && (hasWorktree || !hasEvidence) {
		return Assignment{}, fmt.Errorf("%w: %s needs evidence only", ErrWorkspaceMismatch, rs.Phase)
	}

	a := Assignment{
		ProtocolVersion:       protocol.SupportedVersion,
		MessageType:           assignmentType,
		RunID:                 rs.RunID,
		SessionID:             in.SessionID,
		TurnID:                rs.Assignment.ID,
		Role:                  in.Role,
		Phase:                 rs.Phase,
		ExpectedStateRevision: rs.Revision,
		BindingGuidance:       append([]string{}, in.BindingGuidance...), // never nil
		ArtifactMessageType:   in.ArtifactMessageType,
	}
	if edits {
		wt := in.Workspace.Worktree
		a.Worktree = &wt
	} else {
		ev := in.Workspace.EvidenceRoot
		a.EvidenceRoot = &ev
	}
	return a, nil
}

// Marshal serializes the assignment and validates it against the embedded
// assignment schema (the same bytes handed to a provider), returning the
// canonical wire bytes.
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
