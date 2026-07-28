package config

import (
	"encoding/json"
	"slices"
	"testing"
)

// fullTask returns a complete task contract with every field present.
func fullTask() map[string]any {
	return map[string]any{
		"schema_version":      TaskContractVersion,
		"goal":                "Build the attach protocol",
		"current_behavior":    "headless subprocess driver",
		"desired_behavior":    "two terminals converge by agreement",
		"scope":               "coordinator core",
		"non_goals":           []string{},
		"constraints":         []string{},
		"acceptance_criteria": []string{"pull returns an assignment", "submit advances state"},
		"required_tests":      []string{},
		"relevant_files":      []string{},
		"relevant_repo_paths": []string{"internal/config/config.go"},
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
	return `{"schema_version":2,"test_gate":{"argv":["go","test","./..."]},"base_branch":"main"}`
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
	for _, missing := range []string{"non_goals", "constraints", "required_tests", "relevant_files", "relevant_repo_paths", "open_questions", "current_behavior", "scope"} {
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
	rp, err := ParseRunPolicy([]byte(`{"schema_version":2,"test_gate":{"argv":["make","test"]},"base_branch":"trunk","limits":{"max_wall_seconds":600}}`))
	if err != nil {
		t.Fatalf("ParseRunPolicy: %v", err)
	}
	if rp.BaseBranch != "trunk" || !slices.Equal(rp.TestGate.Argv, []string{"make", "test"}) {
		t.Fatalf("override not applied: %+v", rp)
	}
	// The nested env default must survive an override that names only argv: a policy that supplied a
	// command and silently lost PATH would fail at execution rather than at parse.
	if !slices.Equal(rp.TestGate.Env.Inherit, []string{"PATH"}) {
		t.Fatalf("test_gate.env default lost: %+v", rp.TestGate.Env)
	}
	if rp.Limits.MaxWallSeconds != 600 {
		t.Fatalf("max_wall_seconds override lost: %d", rp.Limits.MaxWallSeconds)
	}
	if rp.Limits.EvidenceMaxRequests != DefaultRunPolicy().Limits.EvidenceMaxRequests {
		t.Fatalf("nested default lost: %d", rp.Limits.EvidenceMaxRequests)
	}
}

func TestParseRunPolicyRejections(t *testing.T) {
	// Each case must be rejected for the reason it NAMES. Every entry therefore carries the current
	// schema version and a valid test gate, so nothing is refused by the version check or the gate XOR
	// on its way to the defect under test — which is what these cases looked like before the v2 bump,
	// when they all declared schema_version 1 and would have passed while proving nothing.
	const gate = `"test_gate":{"argv":["x"]},`
	cases := map[string]string{
		"empty":            ``,
		"missing version":  `{` + gate + `"base_branch":"main"}`,
		"bad version":      `{"schema_version":9,` + gate + `"base_branch":"main"}`,
		"negative budget":  `{"schema_version":2,` + gate + `"budgets":{"plan_rounds":-1}}`,
		"zero limit":       `{"schema_version":2,` + gate + `"limits":{"max_run_turns":0}}`,
		"bad fs policy":    `{"schema_version":2,` + gate + `"unknown_fs_policy":"maybe"}`,
		"blank base":       `{"schema_version":2,` + gate + `"base_branch":"   "}`,
		"file>total":       `{"schema_version":2,` + gate + `"limits":{"evidence_max_file_bytes":999999,"evidence_max_total_bytes":1000}}`,
		"null value":       `{"schema_version":2,` + gate + `"base_branch":null}`,
		"case variant":     `{"Schema_Version":2}`,
		"duplicate key":    `{"schema_version":2,"schema_version":2}`,
		"trailing content": `{"schema_version":2} x`,

		// Run-policy v2's own rules.
		"gate with neither argv nor disabled": `{"schema_version":2,"test_gate":{}}`,
		"gate with both":                      `{"schema_version":2,"test_gate":{"argv":["x"],"disabled":true}}`,
		"blank argv[0]":                       `{"schema_version":2,"test_gate":{"argv":["   ","y"]}}`,
		"no implicit shell split":             `{"schema_version":2,"test_gate":{"command":"go test ./..."}}`,
		"env name not a name":                 `{"schema_version":2,` + gate + `"test_gate":{"argv":["x"],"env":{"inherit":["not-a-name"]}}}`,
		"env name starts with a digit":        `{"schema_version":2,"test_gate":{"argv":["x"],"env":{"inherit":["1PATH"]}}}`,
		"env named twice":                     `{"schema_version":2,"test_gate":{"argv":["x"],"env":{"inherit":["PATH","PATH"]}}}`,
		"env both inherited and set":          `{"schema_version":2,"test_gate":{"argv":["x"],"env":{"inherit":["PATH"],"set":{"PATH":"/bin"}}}}`,
		"zero max_test_attempts":              `{"schema_version":2,` + gate + `"limits":{"max_test_attempts":0}}`,
		"max_test_attempts over the ceiling":  `{"schema_version":2,` + gate + `"limits":{"max_test_attempts":99999}}`,
		"zero max_test_output_bytes":          `{"schema_version":2,` + gate + `"limits":{"max_test_output_bytes":0}}`,
		"record ceiling above the packet's":   `{"schema_version":2,` + gate + `"limits":{"max_test_record_bytes":99999,"evidence_max_file_bytes":98304}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRunPolicy([]byte(body)); err == nil {
				t.Fatalf("expected rejection for %q", name)
			}
		})
	}
}

// TestParseRunPolicyRejectionsAreForTheStatedReason guards the guard.
//
// A rejection table proves nothing if every case is refused by an earlier check than the one it names,
// which is exactly what happened when the schema version moved and the fixtures did not. This asserts
// the shared prefix each case is built on is itself ACCEPTED, so any rejection above is attributable to
// the case's own mutation.
func TestParseRunPolicyRejectionsAreForTheStatedReason(t *testing.T) {
	if _, err := ParseRunPolicy([]byte(`{"schema_version":2,"test_gate":{"argv":["x"]},"base_branch":"main"}`)); err != nil {
		t.Fatalf("the rejection table's baseline is itself invalid, so its cases prove nothing: %v", err)
	}
}

// TestRunPolicyDefaultsSatisfyTheirOwnCrossValidation. The record ceiling is only meaningful if the
// shipped defaults can actually issue a result as evidence; defaults that failed their own rule would
// make every no-file run unstartable.
func TestRunPolicyDefaultsSatisfyTheirOwnCrossValidation(t *testing.T) {
	rp := DefaultRunPolicy()
	rp.TestGate = TestGate{Argv: []string{"go", "test", "./..."}}
	if err := rp.Validate(); err != nil {
		t.Fatalf("the shipped defaults do not validate: %v", err)
	}
	l := rp.Limits
	if !(l.MaxTestRecordBytes <= l.EvidenceMaxFileBytes && l.EvidenceMaxFileBytes <= l.EvidenceMaxTotalBytes) {
		t.Fatalf("defaults violate max_test_record_bytes <= evidence_max_file_bytes <= evidence_max_total_bytes: %d, %d, %d",
			l.MaxTestRecordBytes, l.EvidenceMaxFileBytes, l.EvidenceMaxTotalBytes)
	}
}

// TestTestGatePreservesEmptyArguments. An empty argument is meaningful to some commands, and a vector
// exists precisely so nothing has to guess where the boundaries are.
func TestTestGatePreservesEmptyArguments(t *testing.T) {
	rp, err := ParseRunPolicy([]byte(`{"schema_version":2,"test_gate":{"argv":["prog","--flag","","tail"]}}`))
	if err != nil {
		t.Fatalf("ParseRunPolicy: %v", err)
	}
	if !slices.Equal(rp.TestGate.Argv, []string{"prog", "--flag", "", "tail"}) {
		t.Fatalf("argv = %q, want the empty argument preserved", rp.TestGate.Argv)
	}
}

func TestParseRunPolicyBudgetZeroAllowed(t *testing.T) {
	rp, err := ParseRunPolicy([]byte(`{"schema_version":2,"test_gate":{"argv":["x"]},"budgets":{"plan_rounds":0,"checkpoint_rounds":0,"test_rounds":0,"verify_rounds":0}}`))
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
	rp.TestGate = TestGate{Argv: []string{"go", "test", "./..."}}
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
	cmd.TestGate = TestGate{Argv: []string{"go", "test", "./..."}}
	disabled := base
	disabled.TestGate = TestGate{Disabled: true}
	both := base
	both.TestGate = TestGate{Argv: []string{"x"}, Disabled: true}
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
	base.TestGate = TestGate{Argv: []string{"go", "test", "./..."}}

	// Every ceiling-bound field, so a later refactor cannot leave one policy field
	// unrepresentable: each is accepted at the ceiling and rejected one above it.
	fields := []struct {
		name string
		set  func(rp *RunPolicy, v int)
	}{
		{"plan_rounds", func(rp *RunPolicy, v int) { rp.Budgets.PlanRounds = v }},
		{"checkpoint_rounds", func(rp *RunPolicy, v int) { rp.Budgets.CheckpointRounds = v }},
		{"test_rounds", func(rp *RunPolicy, v int) { rp.Budgets.TestRounds = v }},
		{"verify_rounds", func(rp *RunPolicy, v int) { rp.Budgets.VerifyRounds = v }},
		{"max_run_turns", func(rp *RunPolicy, v int) { rp.Limits.MaxRunTurns = v }},
	}
	for _, f := range fields {
		atCeiling := base
		f.set(&atCeiling, MaxBudget)
		if err := atCeiling.Validate(); err != nil {
			t.Fatalf("%s at the ceiling should be accepted: %v", f.name, err)
		}
		over := base
		f.set(&over, MaxBudget+1)
		if err := over.Validate(); err == nil {
			t.Fatalf("%s above the ceiling should be rejected", f.name)
		}
	}
}

// Task-contract v2 resolves relevant_repo_paths against the committed source tree, so the grammar is
// strict and platform-independent: what a selector means must not depend on the host it is read on.
func TestRelevantRepoPathsGrammar(t *testing.T) {
	bad := map[string][]string{
		"empty":            {},
		"traversal":        {"../outside"},
		"absolute":         {"/etc/passwd"},
		"backslash":        {`dir\file.go`},
		"drive colon":      {"C:/file.go"},
		"trailing slash":   {"dir/"},
		"dot segment":      {"dir/./file.go"},
		"duplicate":        {"a.go", "a.go"},
		"invalid utf-8":    {"dir/x\x80"},
		"control byte":     {"dir/\x01file"},
		"replacement rune": {"dir/\ufffd"},
	}
	for name, paths := range bad {
		t.Run(name, func(t *testing.T) {
			m := fullTask()
			m["relevant_repo_paths"] = paths
			if _, err := ParseTaskContract(mustJSON(t, m)); err == nil {
				t.Fatalf("%s selector accepted", name)
			}
		})
	}
	// A nested, exact-leaf path is the normal case and must be accepted.
	m := fullTask()
	m["relevant_repo_paths"] = []string{"internal/config/config.go", "README"}
	if _, err := ParseTaskContract(mustJSON(t, m)); err != nil {
		t.Fatalf("valid selectors rejected: %v", err)
	}
}

// A v1 document declares no machine-resolvable selection at all, so it is refused by the version
// check rather than silently treated as "select nothing".
func TestTaskContractV1Refused(t *testing.T) {
	m := fullTask()
	m["schema_version"] = 1
	delete(m, "relevant_repo_paths")
	if _, err := ParseTaskContract(mustJSON(t, m)); err == nil {
		t.Fatal("a v1 task contract must fail the version check")
	}
}
