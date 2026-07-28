package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
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

// Each case must be rejected for the reason it NAMES, so every entry asserts the error TEXT and not
// merely that an error happened.
//
// Both halves of that were learned the hard way. The table once declared schema_version 1 throughout,
// so every case was refused by the version check before reaching its own defect; and the env cases were
// silently passing on key spelling and on a duplicate `test_gate` key rather than on the environment
// rules they claimed to cover. An `err != nil` assertion cannot tell any of that apart.
func TestParseRunPolicyRejections(t *testing.T) {
	const gate = `"test_gate":{"argv":["x"]},`
	cases := map[string]struct{ body, want string }{
		"empty":            {``, "empty document"},
		"missing version":  {`{` + gate + `"base_branch":"main"}`, "schema_version is required"},
		"bad version":      {`{"schema_version":9,` + gate + `"base_branch":"main"}`, "schema_version must be 2"},
		"negative budget":  {`{"schema_version":2,` + gate + `"budgets":{"plan_rounds":-1}}`, "budgets.plan_rounds must be >= 0"},
		"zero limit":       {`{"schema_version":2,` + gate + `"limits":{"max_run_turns":0}}`, "limits.max_run_turns must be positive"},
		"bad fs policy":    {`{"schema_version":2,` + gate + `"unknown_fs_policy":"maybe"}`, "unknown_fs_policy must be"},
		"blank base":       {`{"schema_version":2,` + gate + `"base_branch":"   "}`, "base_branch is required"},
		"file>total":       {`{"schema_version":2,` + gate + `"limits":{"evidence_max_file_bytes":999999,"evidence_max_total_bytes":1000}}`, "exceeds evidence_max_total_bytes"},
		"null value":       {`{"schema_version":2,` + gate + `"base_branch":null}`, "explicit null"},
		"case variant":     {`{"Schema_Version":2}`, "non-canonical key spelling"},
		"duplicate key":    {`{"schema_version":2,"schema_version":2}`, "duplicate key"},
		"trailing content": {`{"schema_version":2} x`, "after top-level value"},

		// A REAL v1 document. Before the version check moved ahead of decoding, this was reported as an
		// unknown field — telling the operator their file was malformed when it was simply a previous
		// version of a schema that had moved.
		"a genuine v1 document": {`{"schema_version":1,"test_gate":{"command":"go test ./..."},"base_branch":"main"}`, "schema_version must be 2"},

		// Run-policy v2's own rules.
		"gate with neither argv nor disabled": {`{"schema_version":2,"test_gate":{}}`, "exactly one of argv or disabled"},
		"gate with both":                      {`{"schema_version":2,"test_gate":{"argv":["x"],"disabled":true}}`, "exactly one of argv or disabled"},
		"blank argv[0]":                       {`{"schema_version":2,"test_gate":{"argv":["   ","y"]}}`, "argv[0] must name a command"},
		"v2 gate still using command":         {`{"schema_version":2,"test_gate":{"command":"go test ./..."}}`, "unknown field"},
		"env name not a name":                 {`{"schema_version":2,"test_gate":{"argv":["x"],"env":{"inherit":["not-a-name"]}}}`, "is not [A-Za-z_][A-Za-z0-9_]*"},
		"env name starts with a digit":        {`{"schema_version":2,"test_gate":{"argv":["x"],"env":{"inherit":["1PATH"]}}}`, "starts with a digit"},
		"env inherited twice":                 {`{"schema_version":2,"test_gate":{"argv":["x"],"env":{"inherit":["PATH","PATH"]}}}`, "inherit names PATH twice"},
		"env set twice":                       {`{"schema_version":2,"test_gate":{"argv":["x"],"env":{"set":[{"name":"TOKEN","value":"a"},{"name":"TOKEN","value":"b"}]}}}`, "set names TOKEN twice"},
		"env both inherited and set":          {`{"schema_version":2,"test_gate":{"argv":["x"],"env":{"inherit":["PATH"],"set":[{"name":"PATH","value":"/bin"}]}}}`, "both inherit and set"},
		"zero max_test_attempts":              {`{"schema_version":2,` + gate + `"limits":{"max_test_attempts":0}}`, "limits.max_test_attempts must be positive"},
		"max_test_attempts over the ceiling":  {`{"schema_version":2,` + gate + `"limits":{"max_test_attempts":99999}}`, "max_test_attempts must be <="},
		"zero max_test_output_bytes":          {`{"schema_version":2,` + gate + `"limits":{"max_test_output_bytes":0}}`, "limits.max_test_output_bytes must be positive"},
		"record ceiling above the packet's":   {`{"schema_version":2,` + gate + `"limits":{"max_test_record_bytes":99999,"evidence_max_file_bytes":98304}}`, "could never be issued as evidence"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRunPolicy([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected rejection for %q", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

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

// TestEnvSetAcceptsOrdinaryUppercaseNames is the positive half the rejection table cannot supply.
//
// Environment names are conventionally UPPER-case, and the strict reader refuses any object KEY that is
// not canonical lower-case at every depth. As a JSON map, `"set":{"PATH":"..."}` was therefore
// unrepresentable — the walker fired on the name before any environment rule ran, so the collision case
// that claimed to test "inherit and set" was really passing on key spelling. An entry array moves the
// data into values, and this test is what proves the shape can express what it is for.
func TestEnvSetAcceptsOrdinaryUppercaseNames(t *testing.T) {
	rp, err := ParseRunPolicy([]byte(`{"schema_version":2,"test_gate":{"argv":["go","test"],` +
		`"env":{"inherit":["PATH"],"set":[{"name":"CI","value":"1"},{"name":"GOFLAGS","value":"-count=1"}]}}}`))
	if err != nil {
		t.Fatalf("ParseRunPolicy: %v", err)
	}
	want := []EnvAssignment{{Name: "CI", Value: "1"}, {Name: "GOFLAGS", Value: "-count=1"}}
	if !slices.Equal(rp.TestGate.Env.Set, want) {
		t.Fatalf("env.set = %+v, want %+v", rp.TestGate.Env.Set, want)
	}
}

// TestPolicyMarshalParseRoundTrips pins the property the empty-not-null wire choice exists for, over
// the shapes that actually occur.
//
// The frozen policy is embedded in persisted run state and BootstrapIntent requires the re-parsed
// policy to equal it exactly, so a value that marshals to something this package's own parser refuses
// is a value that can strand a run. The zero TestGate did precisely that: nil collections marshal as
// null, and null is refused on the way back in.
func TestPolicyMarshalParseRoundTrips(t *testing.T) {
	base := DefaultRunPolicy()
	for _, tc := range []struct {
		name string
		gate TestGate
	}{
		{"disabled with zero-valued collections", TestGate{Disabled: true}},
		{"argv with explicitly empty collections", TestGate{Argv: []string{"go", "test"}, Env: TestGateEnv{Inherit: []string{}, Set: []EnvAssignment{}}}},
		{"uppercase env.set", TestGate{
			Argv: []string{"go", "test"},
			Env:  TestGateEnv{Inherit: []string{"PATH"}, Set: []EnvAssignment{{Name: "CI", Value: "1"}}},
		}},
		{"an argument that is deliberately empty", TestGate{Argv: []string{"prog", "", "tail"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rp := base
			rp.TestGate = tc.gate
			b, err := json.Marshal(rp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := ParseRunPolicy(b)
			if err != nil {
				t.Fatalf("the marshalled policy could not be parsed back: %v\n%s", err, b)
			}
			// Semantic equality, which is what the bootstrap invariant actually compares.
			if !reflect.DeepEqual(got, mustParse(t, b)) {
				t.Fatal("parsing the same bytes twice disagreed with itself")
			}
			if got.TestGate.Disabled != tc.gate.Disabled || !slices.Equal(got.TestGate.Argv, nonNil(tc.gate.Argv)) {
				t.Fatalf("gate round-trip lost data: %+v -> %+v", tc.gate, got.TestGate)
			}
			if !slices.Equal(got.TestGate.Env.Set, nonNilAssignments(tc.gate.Env.Set)) {
				t.Fatalf("env.set round-trip lost data: %+v -> %+v", tc.gate.Env.Set, got.TestGate.Env.Set)
			}
		})
	}
}

func mustParse(t *testing.T, b []byte) RunPolicy {
	t.Helper()
	rp, err := ParseRunPolicy(b)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	return rp
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilAssignments(s []EnvAssignment) []EnvAssignment {
	if s == nil {
		return []EnvAssignment{}
	}
	return s
}

// TestPlatformRequiredEnvIsClosedAndNamed. The platform set is part of the authority model, so it must
// be enumerated rather than assembled at resolution time — otherwise "nothing unnamed reaches the
// command" is contradicted by the very code that builds the environment.
func TestPlatformRequiredEnvIsClosedAndNamed(t *testing.T) {
	win := PlatformRequired("windows")
	for _, want := range []string{"ComSpec", "PATHEXT", "SystemRoot"} {
		if !slices.Contains(win, want) {
			t.Fatalf("windows platform set %v omits %q", win, want)
		}
	}
	for _, n := range win {
		if err := validateEnvName(n); err != nil {
			t.Fatalf("platform-required name %q is not a valid environment name: %v", n, err)
		}
	}
	if got := PlatformRequired("linux"); len(got) != 0 {
		t.Fatalf("linux platform set = %v, want empty; PATH is already the default allowlist", got)
	}
	// Returned by value: a caller must not be able to enlarge the authority model by appending to it.
	win = append(win, "SECRET")
	if slices.Contains(PlatformRequired("windows"), "SECRET") {
		t.Fatal("PlatformRequired handed out its own backing array; the authorized set is mutable")
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
