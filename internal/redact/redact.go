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
// Redaction is heuristic and pattern-based. It does not promise to catch every
// possible secret; it does guarantee the token families listed below (the ones
// the harvested vectors adopted) plus generic key=value / "key":"value"
// credential assignments. Schema-aware field-level redaction of known-sensitive
// fields is layered on top by the protocol package; this leaf is the
// defence-in-depth backstop over the serialized bytes.
package redact

import "regexp"

// Placeholder replaces any detected secret value.
const Placeholder = "[REDACTED]"

// fullMatch patterns replace the entire matched token with Placeholder.
var fullMatch = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{6,}`),     // Anthropic API key
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),        // OpenAI key incl. sk-proj-/sk-svcacct-
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), // GitHub fine-grained PAT
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),   // GitHub token (ghp_/gho_/ghu_/ghs_/ghr_)
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`), // Slack token
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),             // AWS access key id
}

// bearer keeps the HTTP scheme and drops the token.
var bearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]+`)

// keyValue redacts key=value or "key":"value" credential assignments (plaintext
// and JSON). The optional quotes around the delimiter absorb JSON's
// `"key":"value"`; the value stops at whitespace, quotes, commas, and closing
// braces so JSON structure is preserved around the redacted value.
// (HTTP Authorization headers are handled by the dedicated bearer pattern, which
// preserves the scheme word; do not add "authorization" here or it would redact
// the scheme too.)
var keyValue = regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|token|secret|access[_-]?token|refresh[_-]?token|password|passwd)\b("?\s*[:=]\s*"?)([^\s",}]+)`)

// Text returns s with detected secrets replaced by Placeholder. It is
// idempotent: redacting already-redacted text is a no-op.
func Text(s string) string {
	for _, re := range fullMatch {
		s = re.ReplaceAllString(s, Placeholder)
	}
	s = bearer.ReplaceAllString(s, "${1}"+Placeholder)
	s = keyValue.ReplaceAllString(s, "${1}${2}"+Placeholder)
	return s
}

// Bytes redacts a byte slice, returning a new slice. The input is not mutated.
func Bytes(b []byte) []byte {
	return []byte(Text(string(b)))
}
