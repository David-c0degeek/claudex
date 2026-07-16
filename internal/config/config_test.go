package config

import "testing"

const validTask = `{
	"schema_version": 1,
	"goal": "Build the attach protocol",
	"current_behavior": "headless subprocess driver",
	"desired_behavior": "two terminals converge by agreement",
	"scope": "coordinator core",
	"acceptance_criteria": ["pull returns an assignment", "submit advances state"]
}`

func validPolicy() string {
	return `{"schema_version":1,"test_gate":{"command":"go test ./..."},"base_branch":"main"}`
}

func TestParseTaskContractValid(t *testing.T) {
	tc, err := ParseTaskContract([]byte(validTask))
	if err != nil {
		t.Fatalf("ParseTaskContract: %v", err)
	}
	if tc.Goal == "" || len(tc.AcceptanceCriteria) != 2 {
		t.Fatalf("parsed contract missing fields: %+v", tc)
	}
}

func TestParseTaskContractRejections(t *testing.T) {
	cases := map[string]string{
		"missing version":  `{"goal":"g","current_behavior":"c","desired_behavior":"d","scope":"s","acceptance_criteria":["a"]}`,
		"bad version":      `{"schema_version":99,"goal":"g","current_behavior":"c","desired_behavior":"d","scope":"s","acceptance_criteria":["a"]}`,
		"missing goal":     `{"schema_version":1,"current_behavior":"c","desired_behavior":"d","scope":"s","acceptance_criteria":["a"]}`,
		"missing scope":    `{"schema_version":1,"goal":"g","current_behavior":"c","desired_behavior":"d","acceptance_criteria":["a"]}`,
		"no acceptance":    `{"schema_version":1,"goal":"g","current_behavior":"c","desired_behavior":"d","scope":"s","acceptance_criteria":[]}`,
		"blank acceptance": `{"schema_version":1,"goal":"g","current_behavior":"c","desired_behavior":"d","scope":"s","acceptance_criteria":["  "]}`,
		"blank req test":   `{"schema_version":1,"goal":"g","current_behavior":"c","desired_behavior":"d","scope":"s","acceptance_criteria":["a"],"required_tests":[""]}`,
		"unknown field":    `{"schema_version":1,"goal":"g","current_behavior":"c","desired_behavior":"d","scope":"s","acceptance_criteria":["a"],"typo":1}`,
		"duplicate key":    `{"schema_version":1,"schema_version":1,"goal":"g","current_behavior":"c","desired_behavior":"d","scope":"s","acceptance_criteria":["a"]}`,
		"explicit null":    `{"schema_version":1,"goal":null,"current_behavior":"c","desired_behavior":"d","scope":"s","acceptance_criteria":["a"]}`,
		"trailing content": validTask + ` garbage`,
		"stray delimiter":  validTask + `}`,
		"not json":         `not json`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTaskContract([]byte(body)); err == nil {
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
	// A sibling limit not in the doc keeps its default.
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

func TestValidateEffective(t *testing.T) {
	tc, err := ParseTaskContract([]byte(validTask))
	if err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	tcReq := tc
	tcReq.RequiredTests = []string{"unit tests"}

	cmd := DefaultRunPolicy()
	cmd.TestGate = TestGate{Command: "go test ./..."}

	disabled := DefaultRunPolicy()
	disabled.TestGate = TestGate{Disabled: true}

	both := DefaultRunPolicy()
	both.TestGate = TestGate{Command: "x", Disabled: true}

	neither := DefaultRunPolicy()
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
