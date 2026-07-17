package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// Normalize is order-independent (canonical JSON), digests the canonical redacted
// bytes, and extracts the bounded envelope facts.
func TestNormalizeCanonicalOrderIndependentAndDigest(t *testing.T) {
	a := []byte(`{"turn_id":"turn-1","state_revision":7,"requires_human_decision":false,"decision_question":null,"z":1}`)
	b := []byte(`{"z":1,"decision_question":null,"requires_human_decision":false,"state_revision":7,"turn_id":"turn-1"}`)

	na, err := Normalize(a)
	if err != nil {
		t.Fatalf("normalize a: %v", err)
	}
	nb, err := Normalize(b)
	if err != nil {
		t.Fatalf("normalize b: %v", err)
	}
	if na.Digest != nb.Digest || na.CanonicalRedacted != nb.CanonicalRedacted {
		t.Fatalf("canonicalization is key-order dependent:\n a=%q\n b=%q", na.CanonicalRedacted, nb.CanonicalRedacted)
	}
	sum := sha256.Sum256([]byte(na.CanonicalRedacted))
	if na.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s != sha256 of canonical bytes", na.Digest)
	}
	if na.TurnID != "turn-1" || na.StateRevision != 7 || na.RequiresHumanDecision {
		t.Fatalf("envelope facts wrong: %+v", na)
	}
	if na.DecisionQuestionPresent {
		t.Fatalf("null decision_question must not be present")
	}
}

func TestNormalizeDecisionQuestionPresent(t *testing.T) {
	c := []byte(`{"turn_id":"t","state_revision":1,"requires_human_decision":true,"decision_question":"which option?"}`)
	n, err := Normalize(c)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !n.RequiresHumanDecision || !n.DecisionQuestionPresent || n.DecisionQuestion != "which option?" {
		t.Fatalf("decision facts wrong: %+v", n)
	}
	// The envelope round-trips the presence exactly.
	if env := n.envelope(); env.DecisionQuestion == nil || *env.DecisionQuestion != "which option?" {
		t.Fatalf("envelope decision question not round-tripped: %+v", env)
	}
}

// A secret in the submission is redacted before the digest is taken, so the
// authoritative digest is over redacted bytes.
func TestNormalizeRedactsBeforeDigest(t *testing.T) {
	s := []byte(`{"turn_id":"t","note":"sk-ant-abcdefghij0123","state_revision":1,"requires_human_decision":false,"decision_question":null}`)
	n, err := Normalize(s)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if strings.Contains(n.CanonicalRedacted, "sk-ant-abcdefghij0123") {
		t.Fatalf("secret survived redaction: %q", n.CanonicalRedacted)
	}
	if !strings.Contains(n.CanonicalRedacted, "[REDACTED]") {
		t.Fatalf("expected redaction placeholder: %q", n.CanonicalRedacted)
	}
}

func TestNormalizeRejectsNonCanonicalJSON(t *testing.T) {
	if _, err := Normalize([]byte(`{not json`)); err == nil {
		t.Fatal("invalid JSON should be rejected")
	}
}
