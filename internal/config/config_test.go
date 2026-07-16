package config

import (
	"encoding/json"
	"testing"
)

// fullTask returns a complete task contract with every field present.
func fullTask() map[string]any {
	return map[string]any{
		"schema_version":      1,
		"goal":                "Build the attach protocol",
		"current_behavior":    "headless subprocess driver",
		"desired_behavior":    "two terminals converge by agreement",
		"scope":               "coordinator core",
		"non_goals":           []string{},
		"constraints":         []string{},
		"acceptance_criteria": []string{"pull returns an assignment", "submit advances state"},
		"required_tests":      []string{},
		"relevant_files":      []string{},
		"open_questions":      []string{},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func validPolicy() string {
	return `{"schema_version":1,"test_gate":{"command":"go test ./..."},"base_branch":"main"}`
}

func TestParseTaskContractValid(t *testing.T) {
	tc, err := ParseTaskContract(mustJSON(t, fullTask()))
	if err != nil {
		t.Fatalf("ParseTaskContract: %v", err)
	}
	if tc.Goal == "" || len(tc.AcceptanceCriteria) != 2 {
		t.Fatalf("parsed contract missing fields: %+v", tc)
	}
}

func TestParseTaskContractRequiresFullShape(t *testing.T) {
	for _, missing := range []string{"non_goals", "constraints", "required_tests", "relevant_files", "open_questions", "current_behavior", "scope"} {
		t.Run("missing_"+missing, func(t *testing.T) {
			m := fullTask()
			delete(m, missing)
			if _, err := ParseTaskContract(mustJSON(t, m)); err == nil {
				t.Fatalf("expected rejection for missing %q", missing)
			}
		})
	}
}

func TestParseTaskContractRejections(t *testing.T) {
	full := fullTask()
	badVersion := fullTask()
	badVersion["schema_version"] = 99
	blankAccept := fullTask()
	blankAccept["acceptance_criteria"] = []string{"   "}
	blankReq := fullTask()
	blankReq["required_tests"] = []string{""}
	emptyAccept := fullTask()
	emptyAccept["acceptance_criteria"] = []string{}

	cases := map[string][]byte{
		"bad version":      mustJSON(t, badVersion),
		"blank acceptance": mustJSON(t, blankAccept),
		"blank required":   mustJSON(t, blankReq),
		"no acceptance":    mustJSON(t, emptyAccept),
		"unknown field":    append(mustJSON(t, full)[:len(mustJSON(t, full))-1], []byte(`,"typo":1}`)...),
		"case variant":     []byte(`{"Schema_Version":1}`),
		"explicit null":    []byte(`{"schema_version":1,"goal":null}`),
		"duplicate key":    []byte(`{"schema_version":1,"schema_version":1}`),
		"trailing content": append(mustJSON(t, full), []byte(` garbage`)...),
		"not json":         []byte(`not json`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTaskContract(body); err == nil {
				t.Fatalf("expected rejection for %q", name)
			}
		})
	}
}

func TestParseRunPolicyValidKeepsNestedDefaults(t *testing.T) {
	rp, err := ParseRunPolicy([]byte(`{"schema_version":1,"test_gate":{"command":"make test"},"base_branch":"trunk","limits":{"max_wall_seconds":600}}`))
	if err != nil {
		t.Fatalf("ParseRunPolicy: %v", err)
	}
	if rp.BaseBranch != "trunk" || rp.TestGate.Command != "make test" {
		t.Fatalf("override not applied: %+v", rp)
	}
	if rp.Limits.MaxWallSeconds != 600 {
		t.Fatalf("max_wall_seconds override lost: %d", rp.Limits.MaxWallSeconds)
	}
	if rp.Limits.EvidenceMaxRequests != DefaultRunPolicy().Limits.EvidenceMaxRequests {
		t.Fatalf("nested default lost: %d", rp.Limits.EvidenceMaxRequests)
	}
}

func TestParseRunPolicyRejections(t *testing.T) {
	cases := map[string]string{
		"empty":            ``,
		"missing version":  `{"test_gate":{"command":"x"},"base_branch":"main"}`,
		"bad version":      `{"schema_version":9,"test_gate":{"command":"x"},"base_branch":"main"}`,
		"negative budget":  `{"schema_version":1,"budgets":{"plan_rounds":-1}}`,
		"zero limit":       `{"schema_version":1,"limits":{"max_run_turns":0}}`,
		"bad fs policy":    `{"schema_version":1,"unknown_fs_policy":"maybe"}`,
		"blank base":       `{"schema_version":1,"base_branch":"   "}`,
		"file>total":       `{"schema_version":1,"limits":{"evidence_max_file_bytes":999999,"evidence_max_total_bytes":1000}}`,
		"null value":       `{"schema_version":1,"base_branch":null}`,
		"case variant":     `{"Schema_Version":1}`,
		"duplicate key":    `{"schema_version":1,"schema_version":1}`,
		"trailing content": `{"schema_version":1} x`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRunPolicy([]byte(body)); err == nil {
				t.Fatalf("expected rejection for %q", name)
			}
		})
	}
}

func TestParseRunPolicyBudgetZeroAllowed(t *testing.T) {
	rp, err := ParseRunPolicy([]byte(`{"schema_version":1,"test_gate":{"command":"x"},"budgets":{"plan_rounds":0,"checkpoint_rounds":0,"test_rounds":0,"verify_rounds":0}}`))
	if err != nil {
		t.Fatalf("zero budgets should be allowed: %v", err)
	}
	if rp.Budgets.PlanRounds != 0 {
		t.Fatalf("plan_rounds = %d, want 0", rp.Budgets.PlanRounds)
	}
}

func TestValidateEffectiveRunsStructural(t *testing.T) {
	tc, err := ParseTaskContract(mustJSON(t, fullTask()))
	if err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	// An override that zeroes a limit must be caught by the final gate even
	// though it never went through ParseRunPolicy.
	rp := DefaultRunPolicy()
	rp.TestGate = TestGate{Command: "go test ./..."}
	rp.Limits.MaxWallSeconds = 0
	if err := ValidateEffective(tc, rp); err == nil {
		t.Fatalf("ValidateEffective should reject a zero limit from an override")
	}
}

func TestValidateEffectiveTestGate(t *testing.T) {
	tc, err := ParseTaskContract(mustJSON(t, fullTask()))
	if err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	tcReq := tc
	tcReq.RequiredTests = []string{"unit tests"}

	base := DefaultRunPolicy()
	cmd := base
	cmd.TestGate = TestGate{Command: "go test ./..."}
	disabled := base
	disabled.TestGate = TestGate{Disabled: true}
	both := base
	both.TestGate = TestGate{Command: "x", Disabled: true}
	neither := base
	neither.TestGate = TestGate{}

	if err := ValidateEffective(tc, cmd); err != nil {
		t.Fatalf("command gate should be valid: %v", err)
	}
	if err := ValidateEffective(tc, disabled); err != nil {
		t.Fatalf("disabled gate with no required tests should be valid: %v", err)
	}
	if err := ValidateEffective(tc, both); err == nil {
		t.Fatalf("command+disabled should be rejected")
	}
	if err := ValidateEffective(tc, neither); err == nil {
		t.Fatalf("neither command nor disabled should be rejected")
	}
	if err := ValidateEffective(tcReq, disabled); err == nil {
		t.Fatalf("disabling the gate with required tests should be rejected")
	}
}

func TestHashStableAndSensitive(t *testing.T) {
	if Hash([]byte("one")) != Hash([]byte("one")) {
		t.Fatalf("Hash not stable")
	}
	if Hash([]byte("one")) == Hash([]byte("two")) {
		t.Fatalf("Hash collided")
	}
	if len(Hash([]byte("x"))) != 64 {
		t.Fatalf("Hash length wrong")
	}
}

var _ = validPolicy

func TestParseTaskContractRejectsDuplicateCriteria(t *testing.T) {
	task := fullTask()
	task["acceptance_criteria"] = []string{"same", "same"}
	if _, err := ParseTaskContract(mustJSON(t, task)); err == nil {
		t.Fatalf("duplicate acceptance_criteria should be rejected")
	}
}

func TestRunPolicyBudgetCeiling(t *testing.T) {
	base := DefaultRunPolicy()
	base.TestGate = TestGate{Command: "go test ./..."}

	atCeiling := base
	atCeiling.Budgets.PlanRounds = MaxBudget
	atCeiling.Limits.MaxRunTurns = MaxBudget
	if err := atCeiling.Validate(); err != nil {
		t.Fatalf("budgets at the ceiling should be accepted: %v", err)
	}

	overBudget := base
	overBudget.Budgets.PlanRounds = MaxBudget + 1
	if err := overBudget.Validate(); err == nil {
		t.Fatalf("a budget above the ceiling should be rejected")
	}

	overTurns := base
	overTurns.Limits.MaxRunTurns = MaxBudget + 1
	if err := overTurns.Validate(); err == nil {
		t.Fatalf("max_run_turns above the ceiling should be rejected")
	}
}
