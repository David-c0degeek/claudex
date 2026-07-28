package attach

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// maximalPolicyBytes builds a REAL run-policy document that is as large as validation admits.
//
// The size comes from one enormous argv element, which is the shape that broke the earlier bound: a
// policy may spend its whole source budget on a single string, and that string is then carried TWICE —
// base64-expanded as PolicyCanonical, and again inside EffectivePolicy.
func maximalPolicyBytes(t *testing.T, argSize int, envNames []string) []byte {
	t.Helper()
	doc := map[string]any{
		"schema_version": config.RunPolicyVersion,
		"base_branch":    "main",
		"test_gate": map[string]any{
			"argv": []string{"go", "test", strings.Repeat("x", argSize)},
			"env":  map[string]any{"inherit": envNames, "set": []any{}},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	return b
}

// maximalTaskBytes builds a real task contract close to the source ceiling.
func maximalTaskBytes(t *testing.T, fill int) []byte {
	t.Helper()
	doc := map[string]any{
		"schema_version":      config.TaskContractVersion,
		"goal":                strings.Repeat("g", fill),
		"current_behavior":    "none",
		"desired_behavior":    "two terminals converge",
		"scope":               "the mechanical gate",
		"non_goals":           []string{},
		"constraints":         []string{},
		"acceptance_criteria": []string{"it works"},
		"required_tests":      []string{},
		"relevant_files":      []string{},
		"relevant_repo_paths": []string{"README"},
		"open_questions":      []string{},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	return b
}

// TestAnAdmittedIntentAlwaysFitsItsPayloadCeiling.
//
// The previous version of this test proved nothing, and review was right about why: it filled
// PolicyCanonical with arbitrary bytes while carrying an unrelated small EffectivePolicy — a pair
// BootstrapIntent.validate rejects outright, so the "worst case" it measured was a state no admitted
// intent can be in. The missing term was exactly the duplicated policy, and with it a LEGAL
// maximum-sized intent measured 69769 bytes against a 65536 ceiling.
//
// This builds only intents that VALIDATE, and asserts the property that matters: anything validate
// admits also fits the transaction payload.
func TestAnAdmittedIntentAlwaysFitsItsPayloadCeiling(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	relDir := state.RunDirRelFor(runID)

	var envNames []string
	for i := 0; i < 6; i++ {
		envNames = append(envNames, "VAR"+string(rune('A'+i)))
	}

	build := func(t *testing.T, argSize, taskFill, envValueSize int) BootstrapIntent {
		t.Helper()
		policySrc := maximalPolicyBytes(t, argSize, envNames)
		pol, err := config.ParseRunPolicy(policySrc)
		if err != nil {
			t.Fatalf("the policy fixture is not admissible: %v", err)
		}
		vals := map[string]string{}
		for _, n := range envNames {
			vals[n] = strings.Repeat("v", envValueSize)
		}
		re, err := config.ResolveForRun(pol.TestGate, config.HostGOOS(),
			func(n string) (string, bool) { v, ok := vals[n]; return v, ok }, relDir)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		taskSrc := maximalTaskBytes(t, taskFill)
		return BootstrapIntent{
			SchemaVersion: bootstrapIntentVersion,
			RunID:         runID, TxnID: "boot-x1", OperationID: opID("c"),
			SessionID: "sess-" + strings.Repeat("b", 32), Agent: state.AgentClaude, CreatedUnix: 1000,
			PairJoinOperationID: opID("d"),
			RelDir:              relDir,
			TaskRelPath:         "inputs/task.json", TaskDigest: config.Hash(taskSrc), TaskCanonical: taskSrc,
			PolicyRelPath: "inputs/policy.json", PolicyDigest: config.Hash(policySrc), PolicyCanonical: policySrc,
			EffectivePolicy:   pol,
			ResolvedExecution: re,
			Base:              pol.BaseBranch, BaseCommit: strings.Repeat("a", 40),
			WorktreeRelPath: relDir + "/worktree", RunBranch: "claudex/" + runID,
			FSClass: "supported-local", FSReason: "local fixed drive",
		}
	}

	t.Run("a maximal admitted intent fits", func(t *testing.T) {
		in := build(t, 10*1024, 14*1024, 700)
		if err := in.validate(); err != nil {
			t.Fatalf("a maximal intent built from admitted components was refused: %v", err)
		}
		payload, err := in.marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if len(payload) > txn.MaxPayloadBytes {
			t.Fatalf("an ADMITTED intent marshals to %d bytes, over the %d-byte ceiling", len(payload), txn.MaxPayloadBytes)
		}
		t.Logf("maximal admitted payload: %d of %d bytes", len(payload), txn.MaxPayloadBytes)

		// The fixture must be near the bounds, or the headroom is an artefact of an undersized case —
		// which is exactly how the previous version of this test managed to pass.
		polWire, _ := json.Marshal(in.EffectivePolicy)
		reWire, _ := in.ResolvedExecution.MarshalJSON()
		if len(polWire) < config.MaxEffectivePolicyWireBytes*3/4 {
			t.Fatalf("policy fixture is %d bytes, well under its %d ceiling; this is not a worst case",
				len(polWire), config.MaxEffectivePolicyWireBytes)
		}
		if len(reWire) < config.MaxResolvedExecutionWireBytes*3/4 {
			t.Fatalf("resolved-environment fixture is %d bytes, well under its %d ceiling; this is not a worst case",
				len(reWire), config.MaxResolvedExecutionWireBytes)
		}
		if len(in.TaskCanonical) < config.MaxContractBytes*3/4 {
			t.Fatalf("task fixture is %d bytes, well under the %d source ceiling", len(in.TaskCanonical), config.MaxContractBytes)
		}
	})

	// A policy beyond its own wire ceiling is refused at PARSE, which is the earliest and clearest
	// point: the operator learns which component is too large rather than that a transaction could not
	// be written.
	t.Run("an over-large policy is refused at parse", func(t *testing.T) {
		_, err := config.ParseRunPolicy(maximalPolicyBytes(t, 14*1024, envNames))
		if err == nil {
			t.Fatal("a policy over the effective-policy wire ceiling was admitted")
		}
		if !strings.Contains(err.Error(), "encodes to") {
			t.Fatalf("err = %v, want a policy-size refusal naming the component", err)
		}
	})

	// And the assembled-payload guard is what still catches it if the component bounds ever drift apart
	// again — which is the whole reason it exists rather than a size argument in a comment.
	t.Run("the assembled-payload guard fires", func(t *testing.T) {
		in := build(t, 10*1024, 14*1024, 700)
		// Inflated through the FILESYSTEM REASON, which is free text and carries no length bound of its
		// own. That makes it the one component whose growth no earlier check catches — so it is both the
		// honest way to reach this guard and the reason the guard is not merely belt and braces.
		in.FSReason = "local fixed drive " + strings.Repeat("z", 32*1024)
		err := in.validate()
		if err == nil {
			t.Fatal("an oversized intent was admitted")
		}
		if !strings.Contains(err.Error(), "transaction payload ceiling") {
			t.Fatalf("err = %v, want the assembled-payload refusal", err)
		}
	})
}
