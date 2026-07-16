package protocol

import (
	"strings"
	"testing"
)

func TestCompileRejectsUnsupportedKeyword(t *testing.T) {
	// pattern is deliberately not in the supported subset yet.
	_, err := compile([]byte(`{"type":"string","pattern":"^a"}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported keyword") {
		t.Fatalf("err = %v, want an unsupported-keyword failure", err)
	}
}

func TestCompileAcceptsAndTypeChecksAnnotations(t *testing.T) {
	ok := `{"title":"t","description":"d","$schema":"s","type":"string"}`
	if _, err := compile([]byte(ok)); err != nil {
		t.Fatalf("annotations should be accepted: %v", err)
	}
	// A non-string annotation is rejected.
	if _, err := compile([]byte(`{"description":123,"type":"string"}`)); err == nil {
		t.Fatalf("a non-string description should be rejected")
	}
}

func TestStrictObjectProfile(t *testing.T) {
	bad := map[string]string{
		"missing additionalProperties": `{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}`,
		"additionalProperties true":    `{"type":"object","additionalProperties":true,"required":["a"],"properties":{"a":{"type":"string"}}}`,
		"required omits a property":    `{"type":"object","additionalProperties":false,"required":[],"properties":{"a":{"type":"string"}}}`,
		"required names unknown":       `{"type":"object","additionalProperties":false,"required":["a","b"],"properties":{"a":{"type":"string"}}}`,
		"duplicate in required":        `{"type":"object","additionalProperties":false,"required":["a","a"],"properties":{"a":{"type":"string"}}}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := compile([]byte(body)); err == nil {
				t.Fatalf("%s should fail compilation", name)
			}
		})
	}

	good := `{"type":"object","additionalProperties":false,"required":["a","b"],"properties":{"a":{"type":"string"},"b":{"type":["integer","null"]}}}`
	if _, err := compile([]byte(good)); err != nil {
		t.Fatalf("a strict object with a nullable-required property should compile: %v", err)
	}
}

func TestCompileRejectsNumberType(t *testing.T) {
	if _, err := compile([]byte(`{"type":"number"}`)); err == nil {
		t.Fatalf("type number should be rejected (integer-only domain)")
	}
}

// A nullable union accepts either the typed value or null.
func TestNullableUnionValidation(t *testing.T) {
	sch, err := compile([]byte(`{"type":["string","null"]}`))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := sch.validate("hi", "$"); err != nil {
		t.Fatalf("string should validate: %v", err)
	}
	if err := sch.validate(nil, "$"); err != nil {
		t.Fatalf("null should validate: %v", err)
	}
	if err := sch.validate(true, "$"); err == nil {
		t.Fatalf("bool should not validate against string|null")
	}
}
