package redact

import (
	"strings"
	"testing"
)

// The secret literals below mirror the harvested redaction vectors from the
// retired Python suite (see docs/evidence-harvest.md §3 Redaction).

func TestRedactAnthropicKey(t *testing.T) {
	secret := "sk-ant-abcdefghijklmnopqrstuvwxyz"
	got := Text("token=" + secret)
	if strings.Contains(got, secret) {
		t.Fatalf("Text() still contains the secret: %q", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Fatalf("Text() = %q, want a %s marker", got, Placeholder)
	}
}

func TestRedactBearer(t *testing.T) {
	secret := "supersecretbearertoken1234567890"
	got := Text("Authorization: Bearer " + secret)
	if strings.Contains(got, secret) {
		t.Fatalf("Text() still contains bearer token: %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "bearer ") {
		t.Fatalf("Text() dropped the Bearer scheme: %q", got)
	}
}

func TestRedactTokenAssignmentInStructuredValue(t *testing.T) {
	secret := "super-secret-value"
	got := Text(`error="token=` + secret + `"`)
	if strings.Contains(got, secret) {
		t.Fatalf("Text() still contains the assigned secret: %q", got)
	}
}

func TestRedactJSONFieldValue(t *testing.T) {
	secret := "topsecret"
	got := Text(`{"api_key":"` + secret + `","keep":1}`)
	if strings.Contains(got, secret) {
		t.Fatalf("Text() still contains the JSON secret: %q", got)
	}
	if !strings.Contains(got, `"keep":1`) {
		t.Fatalf("Text() damaged surrounding JSON: %q", got)
	}
}

func TestRedactLeavesNonSecretsAlone(t *testing.T) {
	in := "the quick brown fox commits code at 12:00"
	if got := Text(in); got != in {
		t.Fatalf("Text() altered non-secret text: %q -> %q", in, got)
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	in := "token=sk-ant-abcdefghijklmnop and Bearer abcdef1234567890xyz"
	once := Text(in)
	twice := Text(once)
	if once != twice {
		t.Fatalf("Text() not idempotent: %q vs %q", once, twice)
	}
}

func TestBytesMatchesText(t *testing.T) {
	in := "secret=abcdef1234567890"
	if string(Bytes([]byte(in))) != Text(in) {
		t.Fatalf("Bytes and Text disagree")
	}
}
