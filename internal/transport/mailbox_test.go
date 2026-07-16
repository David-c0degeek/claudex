package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/state"
)

func canonArtifact(t *testing.T, body string) ([]byte, string) {
	t.Helper()
	canon, err := canonjson.Canonicalize([]byte(body))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	sum := sha256.Sum256(canon)
	return canon, hex.EncodeToString(sum[:])
}

func planArtifact(t *testing.T, turnID string) ([]byte, string) {
	return canonArtifact(t, `{"protocol_version":1,"message_type":"plan","turn_id":"`+turnID+`","state_revision":2,"human_context":null,"requires_human_decision":false,"decision_question":null,"plan_markdown":"# P","steps":[{"title":"s","description":"d","files":[],"tests":[]}],"risks":["r1","r2"],"open_questions":[]}`)
}

func reviewArtifact(t *testing.T, turnID string) ([]byte, string) {
	return canonArtifact(t, `{"protocol_version":1,"message_type":"checkpoint_review","turn_id":"`+turnID+`","state_revision":3,"human_context":null,"requires_human_decision":false,"decision_question":null,"verdict":"AGREE","findings":[],"missing_evidence":[],"tests_adequate":true,"tests_critique":"ok"}`)
}

func TestRenderMailbox(t *testing.T) {
	plan, pd := planArtifact(t, "turn-1")
	review, rd := reviewArtifact(t, "turn-2")
	ledger := []state.LedgerEntry{
		{Revision: 2, TurnID: "turn-1", ArtifactDigest: pd, Role: "lead", Phase: state.PhasePlanDraft, MessageType: "plan"},
		{Revision: 3, TurnID: "turn-2", ArtifactDigest: rd, Role: "pair", Phase: state.PhaseCheckpoint, MessageType: "checkpoint_review"},
	}
	load := func(turnID, _ string) ([]byte, error) {
		switch turnID {
		case "turn-1":
			return plan, nil
		case "turn-2":
			return review, nil
		}
		return nil, os.ErrNotExist
	}
	md, err := RenderMailbox(ledger, load)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(md, "===== [LEAD] turn turn-1 | PLAN_DRAFT | ACCEPTED =====") {
		t.Fatalf("missing lead header:\n%s", md)
	}
	if !strings.Contains(md, "plan: steps=1 risks=2 open_questions=0") {
		t.Fatalf("wrong plan summary:\n%s", md)
	}
	if !strings.Contains(md, "===== [PAIR] turn turn-2 | CHECKPOINT | ACCEPTED =====") || !strings.Contains(md, "verdict=AGREE") {
		t.Fatalf("missing pair block:\n%s", md)
	}

	// Append-only: rendering the first entry is an exact prefix of the full render.
	prefix, err := RenderMailbox(ledger[:1], load)
	if err != nil {
		t.Fatalf("render prefix: %v", err)
	}
	if !strings.HasPrefix(md, prefix) {
		t.Fatalf("extended ledger render is not prefixed by the shorter render")
	}
}

func TestRenderMailboxFailsClosedOnMismatch(t *testing.T) {
	plan, pd := planArtifact(t, "turn-1")
	// Ledger claims a checkpoint_review but the artifact is a plan.
	ledger := []state.LedgerEntry{{Revision: 2, TurnID: "turn-1", ArtifactDigest: pd, Role: "pair", Phase: state.PhaseCheckpoint, MessageType: "checkpoint_review"}}
	load := func(string, string) ([]byte, error) { return plan, nil }
	if _, err := RenderMailbox(ledger, load); !errors.Is(err, ErrMailboxMismatch) {
		t.Fatalf("err = %v, want ErrMailboxMismatch", err)
	}
}

// The summary never emits a non-allowlisted verdict or free text.
func TestSummarizeInjectionSafe(t *testing.T) {
	// A verdict carrying backticks and a newline (JSON-escaped) must not survive.
	body := []byte("{\"message_type\":\"checkpoint_review\",\"verdict\":\"x`y\\ninject\",\"findings\":[1,2]}")
	got := summarize("checkpoint_review", body)
	if strings.Contains(got, "inject") || strings.Contains(got, "`") || strings.Contains(got, "\n") {
		t.Fatalf("summary leaked unsafe verdict: %q", got)
	}
	if !strings.Contains(got, "verdict=?") || !strings.Contains(got, "findings=2") {
		t.Fatalf("summary = %q", got)
	}
}

func TestSessionStoreInbox(t *testing.T) {
	dir := t.TempDir()
	ss, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("new session store: %v", err)
	}
	defer ss.Close()

	a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), editInputs()) // SessionID "sess-1"
	if err != nil {
		t.Fatalf("build assignment: %v", err)
	}
	if err := ss.WriteAssignment("sess-1", a); err != nil {
		t.Fatalf("write inbox: %v", err)
	}
	got, err := ss.ReadAssignment("sess-1")
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	// The stored bytes are the canonical, schema-valid assignment.
	canon, _ := a.Marshal()
	if string(got) != string(canon) {
		t.Fatalf("inbox bytes are not the canonical assignment")
	}

	// Session id must match the assignment.
	if err := ss.WriteAssignment("sess-2", a); !errors.Is(err, ErrBadSession) {
		t.Fatalf("mismatched session err = %v, want ErrBadSession", err)
	}
	// Traversal / non-canonical session ids are rejected.
	for _, bad := range []string{"../escape", "a/b", ".."} {
		if err := ss.WriteAssignment(bad, a); !errors.Is(err, ErrBadSession) {
			t.Fatalf("session id %q err = %v, want ErrBadSession", bad, err)
		}
	}
}
