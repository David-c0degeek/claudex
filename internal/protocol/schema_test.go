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

func TestRejectsUnconstrainedNodes(t *testing.T) {
	bad := map[string]string{
		"empty root":            `{}`,
		"array of empty items":  `{"type":"array","items":{}}`,
		"empty property schema": `{"type":"object","additionalProperties":false,"required":["payload"],"properties":{"payload":{}}}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := compile([]byte(body)); err == nil {
				t.Fatalf("%s should fail compilation", name)
			}
		})
	}
	// A typeless const or non-empty enum is still allowed.
	for _, ok := range []string{`{"const":1}`, `{"enum":["a","b"]}`} {
		if _, err := compile([]byte(ok)); err != nil {
			t.Fatalf("%s should compile: %v", ok, err)
		}
	}
}

func TestObjectMustDeclareRequired(t *testing.T) {
	if _, err := compile([]byte(`{"type":"object","additionalProperties":false,"properties":{}}`)); err == nil {
		t.Fatalf("an object without a required keyword should fail")
	}
	// An explicitly empty strict object compiles.
	if _, err := compile([]byte(`{"type":"object","additionalProperties":false,"required":[],"properties":{}}`)); err != nil {
		t.Fatalf("an empty strict object should compile: %v", err)
	}
}

func TestConstEnumTypeConsistency(t *testing.T) {
	bad := map[string]string{
		"const type mismatch": `{"type":"string","const":1}`,
		"enum type mismatch":  `{"type":"string","enum":["a",1]}`,
		"duplicate enum":      `{"enum":["a","a"]}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := compile([]byte(body)); err == nil {
				t.Fatalf("%s should fail compilation", name)
			}
		})
	}
}

// A JSON null must not silently satisfy a keyword via Go's zero-value decoding.
func TestNullIsNotAcceptedAsKeywordValue(t *testing.T) {
	bad := map[string]string{
		"null annotation":           `{"type":"string","description":null}`,
		"null properties":           `{"type":"object","properties":null,"required":[],"additionalProperties":false}`,
		"null required":             `{"type":"object","properties":{},"required":null,"additionalProperties":false}`,
		"null additionalProperties": `{"type":"object","properties":{},"required":[],"additionalProperties":null}`,
		"null minLength":            `{"type":"string","minLength":null}`,
		"null type":                 `{"type":null}`,
		"null enum container":       `{"enum":null}`,
		"null in required array":    `{"type":"object","properties":{},"required":[null],"additionalProperties":false}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := compile([]byte(body)); err == nil {
				t.Fatalf("%s should fail compilation", name)
			}
		})
	}
}

// null remains legal as a schema literal (const, enum member, declared type).
func TestNullLegalAsLiteral(t *testing.T) {
	sch, err := compile([]byte(`{"const":null}`))
	if err != nil {
		t.Fatalf("const null should compile: %v", err)
	}
	if err := sch.validate(nil, "$"); err != nil {
		t.Fatalf("null should satisfy const null: %v", err)
	}
	if err := sch.validate("x", "$"); err == nil {
		t.Fatalf("a non-null should not satisfy const null")
	}
	if _, err := compile([]byte(`{"enum":["a",null]}`)); err != nil {
		t.Fatalf("enum with a null member should compile: %v", err)
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
