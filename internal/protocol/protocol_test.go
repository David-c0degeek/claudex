package protocol

import (
	"errors"
	"strings"
	"testing"
)

const validReceipt = `{
  "protocol_version": 1,
  "message_type": "receipt",
  "turn_id": "t-1",
  "state_revision": 3,
  "artifact_digest": "` + sixtyFour + `"
}`

const sixtyFour = "0123456789012345678901234567890123456789012345678901234567890123"

// Every embedded schema must compile and satisfy the strict authored profile.
func TestEmbeddedSchemasCompile(t *testing.T) {
	set, err := schemas()
	if err != nil {
		t.Fatalf("embedded schemas failed to compile: %v", err)
	}
	if _, ok := set[schemaKey{"receipt", 1}]; !ok {
		t.Fatalf("receipt v1 schema not registered; have %v", keys(set))
	}
}

func TestValidReceiptRoundTrips(t *testing.T) {
	canon, err := Validate("receipt", []byte(validReceipt))
	if err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	// Re-validating the returned canonical bytes must also pass (stable).
	if _, err := Validate("receipt", canon); err != nil {
		t.Fatalf("canonical receipt rejected on re-validate: %v", err)
	}
}

func TestNegativeCorpus(t *testing.T) {
	cases := map[string]string{
		"missing required":   `{"protocol_version":1,"message_type":"receipt","turn_id":"t","state_revision":1}`,
		"extra property":     `{"protocol_version":1,"message_type":"receipt","turn_id":"t","state_revision":1,"artifact_digest":"` + sixtyFour + `","extra":1}`,
		"wrong type":         `{"protocol_version":1,"message_type":"receipt","turn_id":"t","state_revision":"1","artifact_digest":"` + sixtyFour + `"}`,
		"const violation":    `{"protocol_version":1,"message_type":"result","turn_id":"t","state_revision":1,"artifact_digest":"` + sixtyFour + `"}`,
		"too-short digest":   `{"protocol_version":1,"message_type":"receipt","turn_id":"t","state_revision":1,"artifact_digest":"abc"}`,
		"revision below min": `{"protocol_version":1,"message_type":"receipt","turn_id":"t","state_revision":0,"artifact_digest":"` + sixtyFour + `"}`,
		"empty turn_id":      `{"protocol_version":1,"message_type":"receipt","turn_id":"","state_revision":1,"artifact_digest":"` + sixtyFour + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Validate("receipt", []byte(body)); err == nil {
				t.Fatalf("%s should be rejected", name)
			}
		})
	}
}

// "const violation" above sends message_type "result", which negotiate rejects
// as a type mismatch before schema evaluation; assert that path explicitly.
func TestMessageTypeMismatch(t *testing.T) {
	body := `{"protocol_version":1,"message_type":"result","turn_id":"t","state_revision":1,"artifact_digest":"` + sixtyFour + `"}`
	_, err := Validate("receipt", []byte(body))
	if err == nil || !strings.Contains(err.Error(), "message_type") {
		t.Fatalf("err = %v, want a message_type mismatch", err)
	}
	// The submitted type must not be echoed; only the expected type may appear.
	if strings.Contains(err.Error(), "result") {
		t.Fatalf("diagnostic echoed the submitted message_type: %v", err)
	}
}

func TestVersionNegotiation(t *testing.T) {
	var pv *ProtocolVersionError
	// Unsupported major.
	body := `{"protocol_version":2,"message_type":"receipt","turn_id":"t","state_revision":1,"artifact_digest":"` + sixtyFour + `"}`
	_, err := Validate("receipt", []byte(body))
	if !errors.As(err, &pv) {
		t.Fatalf("err = %v, want *ProtocolVersionError", err)
	}
	if !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("version error missing remediation: %v", err)
	}
	// Missing version.
	body = `{"message_type":"receipt","turn_id":"t","state_revision":1,"artifact_digest":"` + sixtyFour + `"}`
	if _, err := Validate("receipt", []byte(body)); !errors.As(err, &pv) {
		t.Fatalf("missing version err = %v, want *ProtocolVersionError", err)
	}
}

// Input that canonjson rejects must be rejected by Validate before schema
// evaluation (the canonicalize-first pipeline).
func TestCanonjsonInvalidRejectedFirst(t *testing.T) {
	bad := []string{
		`{"protocol_version":1,"protocol_version":1,"message_type":"receipt","turn_id":"t","state_revision":1,"artifact_digest":"` + sixtyFour + `"}`, // duplicate key
		`{"protocol_version":1,"message_type":"receipt","turn_id":"t","state_revision":1.0,"artifact_digest":"` + sixtyFour + `"}`,                    // non-integer
		`{"protocol_version":1,"message_type":"receipt"} trailing`,
	}
	for _, b := range bad {
		if _, err := Validate("receipt", []byte(b)); err == nil {
			t.Fatalf("canonjson-invalid input should be rejected: %s", b)
		}
	}
}

// A validation failure must never echo a submitted value (secrets).
func TestValueFreeDiagnostics(t *testing.T) {
	secret := "sk-ant-abcdefghijklmnopqrstuvwx"
	body := `{"protocol_version":1,"message_type":"receipt","turn_id":"t","state_revision":1,"artifact_digest":"` + sixtyFour + `","` + secret + `":1}`
	_, err := Validate("receipt", []byte(body))
	if err == nil {
		t.Fatalf("extra property should be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("diagnostic echoed a submitted key: %v", err)
	}
}

// The exact embedded bytes must be retrievable for provider instruction, the
// returned copy must not be able to mutate the registry, and Validate must use
// the compiled node built from the same entry.
func TestSchemaReturnsEmbeddedBytesAndIsCopy(t *testing.T) {
	got, err := Schema("receipt", 1)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	embedded, err := schemaFS.ReadFile("schemas/receipt.v1.json")
	if err != nil {
		t.Fatalf("read embedded: %v", err)
	}
	if string(got) != string(embedded) {
		t.Fatalf("Schema bytes differ from the embedded document")
	}
	// Mutating the returned slice must not affect a later call.
	for i := range got {
		got[i] = 'x'
	}
	again, _ := Schema("receipt", 1)
	if string(again) != string(embedded) {
		t.Fatalf("caller mutation altered the registry bytes")
	}
	if _, err := Schema("nope", 1); err == nil {
		t.Fatalf("unknown schema should error")
	}
}

// A future-major message whose body uses a future-domain construct (1.0) must
// still get ProtocolVersionError from the preflight, not a canonjson error.
func TestFutureMajorGetsVersionErrorNotCanonError(t *testing.T) {
	body := `{"protocol_version":2,"message_type":"receipt","state_revision":1.0,"turn_id":"t","artifact_digest":"` + sixtyFour + `"}`
	var pv *ProtocolVersionError
	if _, err := Validate("receipt", []byte(body)); !errors.As(err, &pv) {
		t.Fatalf("err = %v, want *ProtocolVersionError from preflight", err)
	}
}

func TestCompileIsStrictOverItsOwnInput(t *testing.T) {
	// Duplicate schema keyword (last-wins is not allowed).
	if _, err := compile([]byte(`{"type":"string","type":"integer"}`)); err == nil {
		t.Fatalf("a duplicate schema keyword should fail compilation")
	}
	// Trailing document after the schema.
	if _, err := compile([]byte(`{"type":"string"} {}`)); err == nil {
		t.Fatalf("a trailing document should fail compilation")
	}
}

func TestProfileBypassesClosed(t *testing.T) {
	bad := map[string]string{
		"object without properties":   `{"type":"object","additionalProperties":false}`,
		"unconstrained object":        `{"type":"object"}`,
		"required without properties": `{"type":"object","additionalProperties":false,"required":["a"]}`,
		"array without items":         `{"type":"array"}`,
		"string keyword wrong type":   `{"type":"integer","minLength":1}`,
		"numeric keyword wrong type":  `{"type":"string","minimum":1}`,
		"incoherent bounds":           `{"type":"integer","minimum":5,"maximum":1}`,
		"empty enum":                  `{"enum":[]}`,
		"duplicate type":              `{"type":["string","string"]}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := compile([]byte(body)); err == nil {
				t.Fatalf("%s should fail compilation", name)
			}
		})
	}
}

// An object that explicitly forbids extras but declares no properties must not
// then silently accept arbitrary properties at validation time. (Belt-and-
// suspenders: the profile already rejects such a schema at compile.)
func TestAdditionalPropsFalseRejectsExtrasWithoutProperties(t *testing.T) {
	n := &schemaNode{types: []string{"object"}, additionalProps: false, hasAdditional: true}
	if err := n.validate(map[string]interface{}{"x": "y"}, "$"); err == nil {
		t.Fatalf("additionalProperties:false must reject an unexpected property")
	}
}

func TestRegistryIdentityBinding(t *testing.T) {
	// message_type const not equal to the filename type.
	bad := []byte(`{"type":"object","additionalProperties":false,"required":["protocol_version","message_type"],"properties":{"protocol_version":{"type":"integer","const":1},"message_type":{"type":"string","const":"wrong"}}}`)
	node, err := compile(bad)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := checkIdentity(node, "receipt", 1); err == nil {
		t.Fatalf("a mismatched message_type const must fail identity binding")
	}
	// protocol_version const not equal to the filename major.
	bad2 := []byte(`{"type":"object","additionalProperties":false,"required":["protocol_version","message_type"],"properties":{"protocol_version":{"type":"integer","const":2},"message_type":{"type":"string","const":"receipt"}}}`)
	node2, _ := compile(bad2)
	if err := checkIdentity(node2, "receipt", 1); err == nil {
		t.Fatalf("a mismatched protocol_version const must fail identity binding")
	}
	// A union root type (object|null) must not satisfy identity: negotiate only
	// accepts an object, so provider schema and coordinator gate must agree.
	union := []byte(`{"type":["object","null"],"additionalProperties":false,"required":["protocol_version","message_type"],"properties":{"protocol_version":{"type":"integer","const":1},"message_type":{"type":"string","const":"receipt"}}}`)
	node3, err := compile(union)
	if err != nil {
		t.Fatalf("compile union: %v", err)
	}
	if err := checkIdentity(node3, "receipt", 1); err == nil {
		t.Fatalf("a union root type must fail identity binding")
	}
	// A union type on an identity property must also fail.
	unionProp := []byte(`{"type":"object","additionalProperties":false,"required":["protocol_version","message_type"],"properties":{"protocol_version":{"type":"integer","const":1},"message_type":{"type":["string","null"],"const":"receipt"}}}`)
	node4, err := compile(unionProp)
	if err != nil {
		t.Fatalf("compile unionProp: %v", err)
	}
	if err := checkIdentity(node4, "receipt", 1); err == nil {
		t.Fatalf("a union type on message_type must fail identity binding")
	}
}

func keys(m map[schemaKey]*entry) []schemaKey {
	out := make([]schemaKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
