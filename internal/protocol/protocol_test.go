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
	if _, ok := set["receipt"]; !ok {
		t.Fatalf("receipt schema not registered; have %v", keys(set))
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

func keys(m map[string]*schemaNode) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
