package protocol

import (
	"encoding/json"
	"testing"
)

func TestIsKey(t *testing.T) {
	good := []string{"a", "step-one", "k9", "a-b-c", "plan-finding-01"}
	bad := []string{"", "-a", "a-", "a--b", "A", "a_b", "a b", "café"}
	for _, s := range good {
		if !IsKey(s) {
			t.Fatalf("IsKey(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if IsKey(s) {
			t.Fatalf("IsKey(%q) = true, want false", s)
		}
	}
}

func TestValidateKeySet(t *testing.T) {
	if err := ValidateKeySet("findings", []string{"a", "b", "c-d"}); err != nil {
		t.Fatalf("valid key set: %v", err)
	}
	if err := ValidateKeySet("findings", []string{"a", "a"}); err == nil {
		t.Fatalf("duplicate key should be rejected")
	}
	if err := ValidateKeySet("findings", []string{"Bad"}); err == nil {
		t.Fatalf("non-canonical key should be rejected")
	}
}

func TestValidateStepTitles(t *testing.T) {
	if err := ValidateStepTitles([]string{"one", "two"}); err != nil {
		t.Fatalf("valid titles: %v", err)
	}
	if err := ValidateStepTitles(nil); err == nil {
		t.Fatalf("zero steps should be rejected")
	}
	if err := ValidateStepTitles([]string{"dup", "dup"}); err == nil {
		t.Fatalf("duplicate step title should be rejected")
	}
	big := make([]string, MaxPlanSteps+1)
	for i := range big {
		big[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if err := ValidateStepTitles(big); err == nil {
		t.Fatalf("over-max step count should be rejected")
	}
}

// The schema literals must equal the Go step-bound constants, so they cannot drift.
func TestSchemaStepBoundParity(t *testing.T) {
	for _, typ := range []string{"plan", "plan_revision"} {
		b, err := Schema(typ, SupportedVersion)
		if err != nil {
			t.Fatalf("schema %s: %v", typ, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("decode %s: %v", typ, err)
		}
		steps := m["properties"].(map[string]any)["steps"].(map[string]any)
		if got := int(steps["minItems"].(float64)); got != MinPlanSteps {
			t.Fatalf("%s steps.minItems = %d, want %d", typ, got, MinPlanSteps)
		}
		if got := int(steps["maxItems"].(float64)); got != MaxPlanSteps {
			t.Fatalf("%s steps.maxItems = %d, want %d", typ, got, MaxPlanSteps)
		}
	}
}
