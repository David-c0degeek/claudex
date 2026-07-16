// Package redact removes credential-like secrets from text before it crosses any
// persistence or display boundary.
//
// It is a leaf package (no imports beyond the standard library) so every layer —
// state, transport, evidence, status — can redact at its own boundary. The
// canonical pipeline for a persisted artifact is: canonicalize (RFC 8785) →
// redact → persist, and the durable digest is taken over the exact persisted
// (redacted) bytes, so two submissions that differ only in a redacted secret
// collapse to the same receipt deterministically.
//
// Redaction is heuristic and pattern-based: it targets the shapes proven by the
// harvested vectors (Anthropic/OpenAI keys, HTTP bearer tokens, and
// key=value/"key":"value" credential assignments). Schema-aware field-level
// redaction of known-sensitive fields is layered on top by the protocol package;
// this leaf is the defence-in-depth backstop over the serialized bytes.
package redact

import "regexp"

// Placeholder replaces any detected secret value.
const Placeholder = "[REDACTED]"

var (
	// Bare provider API keys.
	anthropicKey = regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{6,}`)
	openaiKey    = regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`)

	// HTTP bearer credentials: keep the scheme, drop the token.
	bearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]+`)

	// key=value or "key":"value" credential assignments (plaintext and JSON).
	// The optional quotes around the delimiter absorb JSON's `"key":"value"`; the
	// value stops at whitespace, quotes, commas, and closing braces so JSON
	// structure is preserved around the redacted value.
	keyValue = regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|token|secret|access[_-]?token|refresh[_-]?token|password|passwd)\b("?\s*[:=]\s*"?)([^\s",}]+)`)
)

// Text returns s with detected secrets replaced by Placeholder. It is
// idempotent: redacting already-redacted text is a no-op.
func Text(s string) string {
	s = anthropicKey.ReplaceAllString(s, Placeholder)
	s = openaiKey.ReplaceAllString(s, Placeholder)
	s = bearer.ReplaceAllString(s, "${1}"+Placeholder)
	s = keyValue.ReplaceAllString(s, "${1}${2}"+Placeholder)
	return s
}

// Bytes redacts a byte slice, returning a new slice. The input is not mutated.
func Bytes(b []byte) []byte {
	return []byte(Text(string(b)))
}
