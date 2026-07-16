package transport

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

func runStateAt(phase state.Phase) state.RunState {
	return state.RunState{
		RunID:      "run-a",
		Revision:   5,
		Phase:      phase,
		Assignment: &state.Ref{ID: "turn-1", IssuedRevision: 5},
	}
}

func editInputs() PullInputs {
	return PullInputs{
		SessionID:           "sess-1",
		Role:                RoleLead,
		ArtifactMessageType: "result",
		Workspace:           Workspace{Worktree: "/wt/run-a"},
	}
}

func reviewInputs(role Role) PullInputs {
	return PullInputs{
		SessionID:           "sess-2",
		Role:                role,
		ArtifactMessageType: "result",
		Workspace:           Workspace{EvidenceRoot: "deadbeef"},
	}
}

func TestEditPhaseCarriesWorktree(t *testing.T) {
	a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), editInputs())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a.Worktree == nil || *a.Worktree != "/wt/run-a" {
		t.Fatalf("edit assignment should carry a worktree: %+v", a)
	}
	if a.EvidenceRoot != nil {
		t.Fatalf("edit assignment must not carry evidence: %+v", a)
	}
	if a.TurnID != "turn-1" || a.ExpectedStateRevision != 5 {
		t.Fatalf("assignment did not project the issued turn/revision: %+v", a)
	}
}

func TestReviewAndVerifyCarryEvidence(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseCheckpoint, state.PhaseVerify} {
		role := RolePair
		a, err := BuildAssignment(runStateAt(phase), reviewInputs(role))
		if err != nil {
			t.Fatalf("build %s: %v", phase, err)
		}
		if a.EvidenceRoot == nil || *a.EvidenceRoot != "deadbeef" {
			t.Fatalf("%s assignment should carry evidence: %+v", phase, a)
		}
		if a.Worktree != nil {
			t.Fatalf("%s assignment must not carry a worktree: %+v", phase, a)
		}
	}
}

func TestWorkspaceMismatchRejected(t *testing.T) {
	// Edit phase given evidence.
	in := editInputs()
	in.Workspace = Workspace{EvidenceRoot: "deadbeef"}
	if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("edit phase with evidence err = %v, want ErrWorkspaceMismatch", err)
	}
	// Review phase given a worktree.
	in2 := reviewInputs(RolePair)
	in2.Workspace = Workspace{Worktree: "/wt"}
	if _, err := BuildAssignment(runStateAt(state.PhaseCheckpoint), in2); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("review phase with worktree err = %v, want ErrWorkspaceMismatch", err)
	}
}

func TestPullNeverMints(t *testing.T) {
	rs := runStateAt(state.PhaseImplementStep)
	rs.Assignment = nil
	if _, err := BuildAssignment(rs, editInputs()); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("err = %v, want ErrNoActiveTurn", err)
	}
}

func TestNonActionablePhaseRejected(t *testing.T) {
	if _, err := BuildAssignment(runStateAt(state.PhaseInit), editInputs()); !errors.Is(err, ErrPhaseNotActionable) {
		t.Fatalf("err = %v, want ErrPhaseNotActionable", err)
	}
}

func TestTwoPullsAreIdentical(t *testing.T) {
	rs := runStateAt(state.PhaseImplementStep)
	a1, _ := BuildAssignment(rs, editInputs())
	a2, _ := BuildAssignment(rs, editInputs())
	if !reflect.DeepEqual(a1, a2) {
		t.Fatalf("two pulls of the same state differ:\n%+v\n%+v", a1, a2)
	}
}

func TestMarshalValidatesAndRoundTrips(t *testing.T) {
	a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), editInputs())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	canon, err := a.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The wire bytes validate against the embedded assignment schema.
	if _, err := protocol.Validate("assignment", canon); err != nil {
		t.Fatalf("wire bytes fail the schema: %v", err)
	}
	// And decode back to an equal assignment.
	var back Assignment
	if err := json.Unmarshal(canon, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(a, back) {
		t.Fatalf("round-trip differs:\n%+v\n%+v", a, back)
	}
}

// A review assignment marshals with a null worktree and a present evidence_root.
func TestReviewMarshalHasNullWorktree(t *testing.T) {
	a, _ := BuildAssignment(runStateAt(state.PhaseVerify), reviewInputs(RolePair))
	canon, err := a.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(canon, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(m["worktree"]) != "null" {
		t.Fatalf("worktree should be null, got %s", m["worktree"])
	}
	if string(m["evidence_root"]) == "null" {
		t.Fatalf("evidence_root should be set")
	}
}
