package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

func hex64() string { return strings.Repeat("a", 64) }

func ptr(s string) *string { return &s }

func runStateAt(phase state.Phase) state.RunState {
	return state.RunState{
		RunID:      "run-a",
		Revision:   5,
		Phase:      phase,
		Assignment: &state.Ref{ID: "turn-1", IssuedRevision: 5},
	}
}

func leadEditInputs() PullInputs {
	return PullInputs{
		SessionID:           "sess-1",
		Role:                RoleLead,
		BindingGuidance:     []string{"prefer small steps"},
		ArtifactMessageType: "plan",
		Worktree:            ptr("/wt/run-a"),
	}
}

func reviewInputs(role Role) PullInputs {
	return PullInputs{
		SessionID:           "sess-2",
		Role:                role,
		ArtifactMessageType: "plan",
		Evidence:            &EvidenceRef{ManifestRelPath: "evidence/checkpoint-1/manifest.json", RootDigest: hex64()},
	}
}

func TestEditPhaseCarriesWorktree(t *testing.T) {
	a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), leadEditInputs())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a.Worktree == nil || *a.Worktree != "/wt/run-a" || a.Evidence != nil {
		t.Fatalf("edit assignment should carry only a worktree: %+v", a)
	}
	if a.TurnID != "turn-1" || a.ExpectedStateRevision != 5 {
		t.Fatalf("assignment did not project the issued turn/revision: %+v", a)
	}
	// The exact embedded artifact schema and its digest are present.
	embedded, _ := protocol.Schema("plan", 1)
	if a.ArtifactSchemaJSON != string(embedded) {
		t.Fatalf("assignment did not embed the exact artifact schema")
	}
	sum := sha256.Sum256(embedded)
	if a.ArtifactSchemaSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("artifact schema digest mismatch")
	}
}

func TestReviewAndVerifyCarryEvidence(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseCheckpoint, state.PhaseVerify, state.PhasePlanCritique} {
		a, err := BuildAssignment(runStateAt(phase), reviewInputs(RolePair))
		if err != nil {
			t.Fatalf("build %s: %v", phase, err)
		}
		if a.Evidence == nil || a.Evidence.RootDigest != hex64() || a.Worktree != nil {
			t.Fatalf("%s assignment should carry only evidence: %+v", phase, a)
		}
	}
}

// A pair turn is always read-only, even in an edit phase: a defensive engine
// input pairing a pair with IMPLEMENT_STEP must not yield a worktree.
func TestPairIsAlwaysReadOnly(t *testing.T) {
	// pair + IMPLEMENT + worktree is a mismatch (pair may not edit).
	in := PullInputs{SessionID: "s", Role: RolePair, ArtifactMessageType: "plan", Worktree: ptr("/wt")}
	if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("pair+IMPLEMENT+worktree err = %v, want ErrWorkspaceMismatch", err)
	}
	// pair + IMPLEMENT + evidence is read-only and accepted.
	a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), reviewInputs(RolePair))
	if err != nil {
		t.Fatalf("pair+IMPLEMENT+evidence build: %v", err)
	}
	if a.Worktree != nil || a.Evidence == nil {
		t.Fatalf("a pair edit-phase turn must be read-only: %+v", a)
	}
}

func TestWorkspaceMismatchRejected(t *testing.T) {
	// Lead edit phase given evidence.
	in := leadEditInputs()
	in.Worktree = nil
	in.Evidence = &EvidenceRef{ManifestRelPath: "e", RootDigest: hex64()}
	if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("lead edit with evidence err = %v, want ErrWorkspaceMismatch", err)
	}
	// Read-only phase given a worktree.
	in2 := reviewInputs(RolePair)
	in2.Evidence = nil
	in2.Worktree = ptr("/wt")
	if _, err := BuildAssignment(runStateAt(state.PhaseCheckpoint), in2); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("review with worktree err = %v, want ErrWorkspaceMismatch", err)
	}
	// Evidence missing its digest.
	in3 := reviewInputs(RolePair)
	in3.Evidence = &EvidenceRef{ManifestRelPath: "e", RootDigest: "short"}
	if _, err := BuildAssignment(runStateAt(state.PhaseCheckpoint), in3); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("evidence with a bad digest err = %v, want ErrWorkspaceMismatch", err)
	}
}

func TestPullNeverMints(t *testing.T) {
	rs := runStateAt(state.PhaseImplementStep)
	rs.Assignment = nil
	if _, err := BuildAssignment(rs, leadEditInputs()); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("err = %v, want ErrNoActiveTurn", err)
	}
}

func TestStaleAssignmentRejected(t *testing.T) {
	rs := runStateAt(state.PhaseImplementStep)
	rs.Assignment.IssuedRevision = 4 // state advanced to 5 without reissuing
	if _, err := BuildAssignment(rs, leadEditInputs()); !errors.Is(err, ErrStaleAssignment) {
		t.Fatalf("err = %v, want ErrStaleAssignment", err)
	}
}

func TestNonActionablePhasesRejected(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseInit, state.PhaseAwaitGuidance, state.PhaseDone, state.PhaseTests} {
		if _, err := BuildAssignment(runStateAt(phase), reviewInputs(RolePair)); !errors.Is(err, ErrPhaseNotActionable) {
			t.Fatalf("%s err = %v, want ErrPhaseNotActionable", phase, err)
		}
	}
}

func TestUnknownArtifactSchemaRejected(t *testing.T) {
	in := leadEditInputs()
	in.ArtifactMessageType = "does-not-exist"
	if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in); err == nil {
		t.Fatalf("an unknown artifact schema should be rejected")
	}
}

func TestTwoPullsAreIdentical(t *testing.T) {
	rs := runStateAt(state.PhaseImplementStep)
	a1, _ := BuildAssignment(rs, leadEditInputs())
	a2, _ := BuildAssignment(rs, leadEditInputs())
	if !reflect.DeepEqual(a1, a2) {
		t.Fatalf("two pulls of the same state differ:\n%+v\n%+v", a1, a2)
	}
}

// The build must not mutate its inputs, and mutating the returned assignment
// must not reach back into the caller's inputs.
func TestBuildDoesNotAliasInputs(t *testing.T) {
	in := reviewInputs(RolePair)
	in.BindingGuidance = []string{"g1"}
	guidBefore := append([]string{}, in.BindingGuidance...)
	evBefore := *in.Evidence
	rs := runStateAt(state.PhaseCheckpoint)
	refBefore := *rs.Assignment

	a, err := BuildAssignment(rs, in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Mutate the returned assignment.
	a.BindingGuidance = append(a.BindingGuidance, "x")
	a.Evidence.RootDigest = strings.Repeat("b", 64)

	if !reflect.DeepEqual(in.BindingGuidance, guidBefore) {
		t.Fatalf("build mutated the input binding guidance")
	}
	if *in.Evidence != evBefore {
		t.Fatalf("mutating the returned evidence reached the input")
	}
	if *rs.Assignment != refBefore {
		t.Fatalf("build mutated the run state assignment ref")
	}
}

func TestMarshalValidatesAndRoundTrips(t *testing.T) {
	a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), leadEditInputs())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	canon, err := a.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := protocol.Validate("assignment", canon); err != nil {
		t.Fatalf("wire bytes fail the schema: %v", err)
	}
	var back Assignment
	if err := json.Unmarshal(canon, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(a, back) {
		t.Fatalf("round-trip differs:\n%+v\n%+v", a, back)
	}
}

// A read-only assignment marshals with a null worktree and a present evidence.
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
	if string(m["evidence"]) == "null" {
		t.Fatalf("evidence should be present")
	}
}
