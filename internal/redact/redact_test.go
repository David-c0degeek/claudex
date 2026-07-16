package redact

import (
	"strings"
	"testing"
)

// Token-shaped test inputs are assembled from fragments at runtime so no
// contiguous credential literal ever appears in source — that keeps secret
// scanners (and push protection) from flagging these deliberately-fake fixtures
// while still exercising the redactor against the real token shapes. The shapes
// mirror the harvested vectors (docs/evidence-harvest.md §3) plus the additional
// provider families the ADOPT disposition promises.

func TestRedactSecretFamilies(t *testing.T) {
	cases := []struct {
		name  string
		token string              // the secret, assembled from fragments
		wrap  func(string) string // how it appears in a line
	}{
		{"anthropic", "sk-" + "ant-" + "abcdefghijklmnopqrstuvwxyz", func(s string) string { return "token=" + s }},
		{"openai-classic", "sk-" + "abcdefghijklmnopqrstuvwxyz012345", func(s string) string { return "key: " + s }},
		{"openai-proj", "sk-" + "proj-" + "abcDEF123456_ghIJKL789012mnop", func(s string) string { return "OPENAI_API_KEY=" + s }},
		{"openai-svcacct", "sk-" + "svcacct-" + "abcDEF123456ghIJKL789012", func(s string) string { return s }},
		{"github-pat", "github" + "_pat_" + "11ABCDEFG0abcdefghij1234", func(s string) string { return s }},
		{"github-ghp", "ghp" + "_" + "abcdefghijklmnopqrstuvwxyz0123", func(s string) string { return s }},
		{"slack", "xox" + "b-" + "111111111111-abcdefghijklmno", func(s string) string { return s }},
		{"aws-akia", "AKIA" + "IOSFODNN7EXAMPLE", func(s string) string { return "aws_access_key_id = " + s }},
		{"bearer", "supersecretbearertoken1234567890", func(s string) string { return "Authorization: Bearer " + s }},
		{"token-assign", "super-secret-value", func(s string) string { return `error="token=` + s + `"` }},
		{"json-field", "topsecret123", func(s string) string { return `{"api_key":"` + s + `","keep":1}` }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.wrap(c.token)
			got := Text(in)
			if strings.Contains(got, c.token) {
				t.Fatalf("Text(%q) still contains secret %q -> %q", in, c.token, got)
			}
			if !strings.Contains(got, Placeholder) {
				t.Fatalf("Text(%q) = %q, want a %s marker", in, got, Placeholder)
			}
		})
	}
}

func TestRedactFalsePositives(t *testing.T) {
	// Non-secret strings that must survive untouched.
	safe := []string{
		"the quick brown fox commits code at 12:00",
		"skip-ahead to the next section",       // starts with sk- but not a key
		"see docs/architecture.md for details", // no credential
		`{"keep":1,"count":42}`,
		"pushd/popd are shell builtins",
	}
	for _, s := range safe {
		if got := Text(s); got != s {
			t.Fatalf("Text(%q) altered non-secret text -> %q", s, got)
		}
	}
}

func TestRedactBearerKeepsScheme(t *testing.T) {
	got := Text("Authorization: Bearer abcDEF1234567890xyz")
	if !strings.Contains(strings.ToLower(got), "bearer ") {
		t.Fatalf("Text() dropped the Bearer scheme: %q", got)
	}
	if strings.Contains(got, "abcDEF1234567890xyz") {
		t.Fatalf("Text() kept the bearer token: %q", got)
	}
}

func TestRedactPreservesSurroundingJSON(t *testing.T) {
	got := Text(`{"api_key":"topsecret123","keep":1}`)
	if !strings.Contains(got, `"keep":1`) {
		t.Fatalf("Text() damaged surrounding JSON: %q", got)
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	in := "token=" + "sk-" + "ant-" + "abcdefghijklmnop" + " and Bearer abcdef1234567890xyz"
	once := Text(in)
	if twice := Text(once); once != twice {
		t.Fatalf("Text() not idempotent: %q vs %q", once, twice)
	}
}

func TestBytesMatchesText(t *testing.T) {
	in := "secret=abcdef1234567890"
	if string(Bytes([]byte(in))) != Text(in) {
		t.Fatalf("Bytes and Text disagree")
	}
}
