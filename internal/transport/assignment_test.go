package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

func hex64() string { return strings.Repeat("a", 64) }

// Minted-shape session ids for fixtures ("sess-" + 32 lower-hex).
var (
	sessLead = "sess-" + strings.Repeat("a", 31) + "1"
	sessPair = "sess-" + strings.Repeat("b", 31) + "2"
)

func ptr(s string) *string { return &s }

func absWorktree() string { return filepath.Join(os.TempDir(), "claudex-wt", "run-a") }

func runStateAt(phase state.Phase) state.RunState {
	rs := state.RunState{
		RunID:      "run-a",
		Revision:   5,
		Phase:      phase,
		Assignment: &state.Ref{ID: "turn-1", IssuedRevision: 5},
	}
	// An active VERIFY carries the retained fresh-session threshold.
	if phase == state.PhaseVerify {
		rs.Verify = &state.VerifyRequirement{RequiredGeneration: 3}
	}
	return rs
}

func editInputs() PullInputs {
	return PullInputs{SessionID: sessLead, Role: RoleLead, BindingGuidance: []string{"prefer small steps"}, Worktree: ptr(absWorktree())}
}

// evidenceInputs carries a qualifying pair generation (== the VERIFY fixture's
// threshold); it is ignored for non-VERIFY phases.
func evidenceInputs(role Role) PullInputs {
	return PullInputs{SessionID: sessPair, Role: role, CurrentPairGeneration: 3, Evidence: &EvidenceRef{ManifestRelPath: "evidence/checkpoint-1/manifest.json", RootDigest: hex64()}}
}

// A non-minted (mixed-case / wrong-shape) session id never builds an assignment.
func TestBuildRejectsNonMintedSession(t *testing.T) {
	for _, bad := range []string{"sess-1", "SESS-" + strings.Repeat("a", 32), "sess-" + strings.Repeat("A", 32), "sess-" + strings.Repeat("a", 31)} {
		in := editInputs()
		in.SessionID = bad
		if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in); err == nil {
			t.Fatalf("session id %q should be rejected by BuildAssignment", bad)
		}
	}
}

func TestEditPhaseCarriesWorktree(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseImplementStep, state.PhaseFix} {
		a, err := BuildAssignment(runStateAt(phase), editInputs())
		if err != nil {
			t.Fatalf("build %s: %v", phase, err)
		}
		if a.Worktree == nil || *a.Worktree != absWorktree() || a.Evidence != nil {
			t.Fatalf("%s should carry only a worktree: %+v", phase, a)
		}
		if a.TurnID != "turn-1" || a.ExpectedStateRevision != 5 || a.ArtifactMessageType != "implementation_report" {
			t.Fatalf("%s projection wrong: %+v", phase, a)
		}
		embedded, _ := protocol.Schema("implementation_report", 1)
		if a.ArtifactSchemaJSON != string(embedded) {
			t.Fatalf("%s did not embed the exact artifact schema", phase)
		}
		sum := sha256.Sum256(embedded)
		if a.ArtifactSchemaSHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s artifact digest mismatch", phase)
		}
	}
}

func TestReadOnlyPhasesCarryEvidence(t *testing.T) {
	cases := map[state.Phase]Role{
		state.PhasePlanDraft:    RoleLead, // lead but read-only (planning)
		state.PhasePlanRevise:   RoleLead,
		state.PhasePlanCritique: RolePair,
		state.PhaseCheckpoint:   RolePair,
		state.PhaseVerify:       RolePair,
	}
	for phase, role := range cases {
		a, err := BuildAssignment(runStateAt(phase), evidenceInputs(role))
		if err != nil {
			t.Fatalf("build %s: %v", phase, err)
		}
		if a.Evidence == nil || a.Evidence.RootDigest != hex64() || a.Worktree != nil {
			t.Fatalf("%s should carry only evidence: %+v", phase, a)
		}
		if a.Role != role {
			t.Fatalf("%s role = %s, want %s", phase, a.Role, role)
		}
	}
}

// A role that disagrees with the phase turn spec is rejected — including the
// defensive pair+IMPLEMENT case (a pair may never be handed an edit turn).
func TestRoleMustMatchTurnSpec(t *testing.T) {
	in := evidenceInputs(RolePair)
	if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in); !errors.Is(err, ErrTurnSpecMismatch) {
		t.Fatalf("pair in IMPLEMENT err = %v, want ErrTurnSpecMismatch", err)
	}
	in2 := evidenceInputs(RoleLead)
	if _, err := BuildAssignment(runStateAt(state.PhaseCheckpoint), in2); !errors.Is(err, ErrTurnSpecMismatch) {
		t.Fatalf("lead in CHECKPOINT err = %v, want ErrTurnSpecMismatch", err)
	}
}

func TestWorkspaceMismatchRejected(t *testing.T) {
	// Edit turn with no worktree.
	in := editInputs()
	in.Worktree = nil
	in.Evidence = &EvidenceRef{ManifestRelPath: "e/m.json", RootDigest: hex64()}
	if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("edit turn without worktree err = %v, want ErrWorkspaceMismatch", err)
	}
	// Read-only turn with no evidence.
	in2 := evidenceInputs(RolePair)
	in2.Evidence = nil
	if _, err := BuildAssignment(runStateAt(state.PhaseCheckpoint), in2); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("read-only turn without evidence err = %v, want ErrWorkspaceMismatch", err)
	}
}

func TestPullNeverMints(t *testing.T) {
	rs := runStateAt(state.PhaseImplementStep)
	rs.Assignment = nil
	if _, err := BuildAssignment(rs, editInputs()); !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("err = %v, want ErrNoActiveTurn", err)
	}
}

func TestStaleAssignmentRejected(t *testing.T) {
	rs := runStateAt(state.PhaseImplementStep)
	rs.Assignment.IssuedRevision = 4
	if _, err := BuildAssignment(rs, editInputs()); !errors.Is(err, ErrStaleAssignment) {
		t.Fatalf("err = %v, want ErrStaleAssignment", err)
	}
}

func TestNonActionablePhasesRejected(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseInit, state.PhaseAwaitGuidance, state.PhaseDone, state.PhaseTests} {
		if _, err := BuildAssignment(runStateAt(phase), evidenceInputs(RolePair)); !errors.Is(err, ErrPhaseNotActionable) {
			t.Fatalf("%s err = %v, want ErrPhaseNotActionable", phase, err)
		}
	}
}

func TestTwoPullsAreIdentical(t *testing.T) {
	rs := runStateAt(state.PhaseImplementStep)
	a1, _ := BuildAssignment(rs, editInputs())
	a2, _ := BuildAssignment(rs, editInputs())
	if !reflect.DeepEqual(a1, a2) {
		t.Fatalf("two pulls of the same state differ")
	}
}

func TestBuildDoesNotAliasInputs(t *testing.T) {
	in := evidenceInputs(RolePair)
	in.BindingGuidance = []string{"g1"}
	guidBefore := append([]string{}, in.BindingGuidance...)
	evBefore := *in.Evidence
	rs := runStateAt(state.PhaseCheckpoint)
	refBefore := *rs.Assignment

	a, err := BuildAssignment(rs, in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
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
	a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), editInputs())
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
		t.Fatalf("round-trip differs")
	}
}

func TestReviewMarshalHasNullWorktree(t *testing.T) {
	a, _ := BuildAssignment(runStateAt(state.PhaseVerify), evidenceInputs(RolePair))
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

// Mutating any semantic field after the build must be caught by Validate/Marshal,
// since the JSON schema alone cannot express these constraints.
func TestMutateAfterBuildRejected(t *testing.T) {
	base := func() Assignment {
		a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), editInputs())
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return a
	}
	mutations := map[string]func(a *Assignment){
		"schema bytes":  func(a *Assignment) { a.ArtifactSchemaJSON = strings.Repeat("x", len(a.ArtifactSchemaJSON)) },
		"schema digest": func(a *Assignment) { a.ArtifactSchemaSHA256 = strings.Repeat("0", 64) },
		"both spaces":   func(a *Assignment) { a.Evidence = &EvidenceRef{ManifestRelPath: "e/m.json", RootDigest: hex64()} },
		"wrong role":    func(a *Assignment) { a.Role = RolePair },
		"wrong type":    func(a *Assignment) { a.ArtifactMessageType = "verification" },
		"drop worktree": func(a *Assignment) { a.Worktree = nil },
		// A 37-byte but non-minted (mixed-case) session id is length-valid for the
		// schema yet must be rejected by semantic validation at the wire boundary.
		"mixed-case session": func(a *Assignment) { a.SessionID = "sess-" + strings.Repeat("A", 32) },
	}
	for name, mut := range mutations {
		t.Run(name, func(t *testing.T) {
			a := base()
			mut(&a)
			if err := a.Validate(); err == nil {
				t.Fatalf("%s: Validate should reject the mutation", name)
			}
			if _, err := a.Marshal(); err == nil {
				t.Fatalf("%s: Marshal should reject the mutation", name)
			}
		})
	}
}

func TestBindingGuidanceRedacted(t *testing.T) {
	secret := "sk-ant-abcdefghijklmnopqrstuvwx"
	in := editInputs()
	in.BindingGuidance = []string{"use token=" + secret + " sparingly"}
	a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(a.BindingGuidance[0], secret) {
		t.Fatalf("guidance not redacted: %q", a.BindingGuidance[0])
	}
	canon, err := a.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(canon), "sk-ant-") {
		t.Fatalf("secret leaked into wire bytes")
	}
}

func TestEvidenceGrammarEnforced(t *testing.T) {
	bad := []EvidenceRef{
		{ManifestRelPath: "../escape.json", RootDigest: hex64()},
		{ManifestRelPath: "/abs/manifest.json", RootDigest: hex64()},
		{ManifestRelPath: "a\\b.json", RootDigest: hex64()},
		{ManifestRelPath: "evidence/m.json", RootDigest: strings.Repeat("A", 64)}, // uppercase
		{ManifestRelPath: "evidence/m.json", RootDigest: "short"},
	}
	for _, ev := range bad {
		in := evidenceInputs(RolePair)
		e := ev
		in.Evidence = &e
		if _, err := BuildAssignment(runStateAt(state.PhaseCheckpoint), in); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("evidence %+v err = %v, want ErrWorkspaceMismatch", ev, err)
		}
	}
}

// BuildAssignment must fully validate, so structural bound violations fail at
// build time, not only later at Marshal.
func TestOverBoundInputsRejectedAtBuild(t *testing.T) {
	// Session id beyond the schema's 128-char bound.
	in := editInputs()
	in.SessionID = strings.Repeat("s", 129)
	if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in); err == nil {
		t.Fatalf("an over-long session id should be rejected at build")
	}
	// Guidance beyond the 4096-char bound.
	in2 := editInputs()
	in2.BindingGuidance = []string{strings.Repeat("g", 5000)}
	if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in2); err == nil {
		t.Fatalf("over-long binding guidance should be rejected at build")
	}
	// Evidence manifest path valid grammar but beyond the 4096-char bound.
	in3 := evidenceInputs(RolePair)
	in3.Evidence = &EvidenceRef{ManifestRelPath: "evidence/" + strings.Repeat("a", 5000) + ".json", RootDigest: hex64()}
	if _, err := BuildAssignment(runStateAt(state.PhaseCheckpoint), in3); err == nil {
		t.Fatalf("an over-long manifest path should be rejected at build")
	}
}

// The TurnSpec table is the contract the phase engine consumes: exact role,
// exact artifact type, an embedded schema for it, and the workspace mode.
func TestTurnSpecTableIsTheContract(t *testing.T) {
	type want struct {
		role     Role
		artifact string
		editable bool
	}
	table := map[state.Phase]want{
		state.PhasePlanDraft:     {RoleLead, "plan", false},
		state.PhasePlanCritique:  {RolePair, "plan_critique", false},
		state.PhasePlanRevise:    {RoleLead, "plan_revision", false},
		state.PhaseImplementStep: {RoleLead, "implementation_report", true},
		state.PhaseCheckpoint:    {RolePair, "checkpoint_review", false},
		state.PhaseFix:           {RoleLead, "implementation_report", true},
		state.PhaseVerify:        {RolePair, "verification", false},
	}
	if len(table) != len(turnSpecs) {
		t.Fatalf("turn spec table drift: %d expected vs %d in turnSpecs", len(table), len(turnSpecs))
	}
	for phase, w := range table {
		spec, ok := TurnSpec(phase)
		if !ok {
			t.Fatalf("%s has no turn spec", phase)
		}
		if spec.Role != w.role || spec.ArtifactMessageType != w.artifact {
			t.Fatalf("%s spec = %+v, want role %s artifact %s", phase, spec, w.role, w.artifact)
		}
		if _, err := protocol.Schema(spec.ArtifactMessageType, protocol.SupportedVersion); err != nil {
			t.Fatalf("%s references a missing schema %q: %v", phase, spec.ArtifactMessageType, err)
		}
		if EditableTurn(spec.Role, phase) != w.editable {
			t.Fatalf("%s editable = %v, want %v", phase, EditableTurn(spec.Role, phase), w.editable)
		}
	}
}

func TestWorktreeGrammarEnforced(t *testing.T) {
	bad := []string{
		"relative/path",
		absWorktree() + "/../escape",
		absWorktree() + "\x01ctrl",
	}
	for _, wt := range bad {
		in := editInputs()
		in.Worktree = ptr(wt)
		if _, err := BuildAssignment(runStateAt(state.PhaseImplementStep), in); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("worktree %q err = %v, want ErrWorkspaceMismatch", wt, err)
		}
	}
}
