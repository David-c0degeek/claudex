package config

import (
	"strings"
	"testing"
)

func TestParseTaskContractValid(t *testing.T) {
	data := []byte(`{
		"schema_version": 1,
		"goal": "Build the attach protocol",
		"desired_behavior": "two terminals converge by agreement",
		"acceptance_criteria": ["pull returns an assignment", "submit advances state"]
	}`)
	tc, err := ParseTaskContract(data)
	if err != nil {
		t.Fatalf("ParseTaskContract: %v", err)
	}
	if tc.Goal == "" || len(tc.AcceptanceCriteria) != 2 {
		t.Fatalf("parsed contract missing fields: %+v", tc)
	}
}

func TestParseTaskContractDefaultsVersion(t *testing.T) {
	data := []byte(`{"goal":"g","desired_behavior":"d","acceptance_criteria":["a"]}`)
	tc, err := ParseTaskContract(data)
	if err != nil {
		t.Fatalf("ParseTaskContract: %v", err)
	}
	if tc.SchemaVersion != TaskContractVersion {
		t.Fatalf("version = %d, want %d", tc.SchemaVersion, TaskContractVersion)
	}
}

func TestParseTaskContractRejections(t *testing.T) {
	cases := map[string]string{
		"bad version":      `{"schema_version":99,"goal":"g","desired_behavior":"d","acceptance_criteria":["a"]}`,
		"missing goal":     `{"desired_behavior":"d","acceptance_criteria":["a"]}`,
		"missing desired":  `{"goal":"g","acceptance_criteria":["a"]}`,
		"no acceptance":    `{"goal":"g","desired_behavior":"d","acceptance_criteria":[]}`,
		"blank acceptance": `{"goal":"g","desired_behavior":"d","acceptance_criteria":["   "]}`,
		"unknown field":    `{"goal":"g","desired_behavior":"d","acceptance_criteria":["a"],"typo":1}`,
		"trailing content": `{"goal":"g","desired_behavior":"d","acceptance_criteria":["a"]} garbage`,
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

func TestParseRunPolicyDefaults(t *testing.T) {
	rp, err := ParseRunPolicy(nil)
	if err != nil {
		t.Fatalf("ParseRunPolicy(nil): %v", err)
	}
	def := DefaultRunPolicy()
	if rp != def {
		t.Fatalf("empty policy = %+v, want defaults %+v", rp, def)
	}
}

func TestParseRunPolicyOverrideKeepsOtherDefaults(t *testing.T) {
	rp, err := ParseRunPolicy([]byte(`{"test_command":"go test ./...","caps":{"max_wall_seconds":600}}`))
	if err != nil {
		t.Fatalf("ParseRunPolicy: %v", err)
	}
	if rp.TestCommand != "go test ./..." {
		t.Fatalf("test_command = %q", rp.TestCommand)
	}
	if rp.Caps.MaxWallSeconds != 600 {
		t.Fatalf("max_wall_seconds = %d, want overridden 600", rp.Caps.MaxWallSeconds)
	}
	// An unspecified cap keeps its default.
	if rp.Caps.MaxCheckpointRounds != DefaultRunPolicy().Caps.MaxCheckpointRounds {
		t.Fatalf("max_checkpoint_rounds lost its default: %d", rp.Caps.MaxCheckpointRounds)
	}
}

func TestParseRunPolicyRejections(t *testing.T) {
	cases := map[string]string{
		"bad version":   `{"schema_version":42}`,
		"unknown field": `{"nope":1}`,
		"negative cap":  `{"caps":{"max_wall_seconds":-1}}`,
		"zero cap":      `{"caps":{"max_plan_rounds":0}}`,
		"blank base":    `{"base_branch":"   "}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRunPolicy([]byte(body)); err == nil {
				t.Fatalf("expected rejection for %q", name)
			}
		})
	}
}

func TestHashStableAndSensitive(t *testing.T) {
	a := Hash([]byte("one"))
	b := Hash([]byte("one"))
	c := Hash([]byte("two"))
	if a != b {
		t.Fatalf("Hash not stable: %s vs %s", a, b)
	}
	if a == c {
		t.Fatalf("Hash collided on different inputs")
	}
	if len(a) != 64 {
		t.Fatalf("Hash length = %d, want 64 hex chars", len(a))
	}
}

func TestTestCommandMayBeEmpty(t *testing.T) {
	// An empty test_command is valid — it means "no mechanical gate".
	rp, err := ParseRunPolicy([]byte(`{"base_branch":"main"}`))
	if err != nil {
		t.Fatalf("ParseRunPolicy: %v", err)
	}
	if strings.TrimSpace(rp.TestCommand) != "" {
		t.Fatalf("expected empty test_command")
	}
}
