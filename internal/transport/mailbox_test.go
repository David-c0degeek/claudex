package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
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
		{Revision: 2, TurnID: "turn-1", ArtifactDigest: pd, Phase: state.PhasePlanDraft},
		{Revision: 3, TurnID: "turn-2", ArtifactDigest: rd, Phase: state.PhaseCheckpoint},
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
	// Role and artifact type are derived from the phase alone.
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

// The renderer derives the expected artifact type from the phase and validates
// against it, so a plan filed under a checkpoint phase fails closed.
func TestRenderMailboxFailsClosedOnTypeMismatch(t *testing.T) {
	plan, pd := planArtifact(t, "turn-1")
	ledger := []state.LedgerEntry{{Revision: 2, TurnID: "turn-1", ArtifactDigest: pd, Phase: state.PhaseCheckpoint}}
	load := func(string, string) ([]byte, error) { return plan, nil }
	if _, err := RenderMailbox(ledger, load); !errors.Is(err, ErrMailboxMismatch) {
		t.Fatalf("err = %v, want ErrMailboxMismatch", err)
	}
}

// The renderer recomputes the digest, so a ledger digest that disagrees with the
// loaded bytes fails closed rather than trusting the loader.
func TestRenderMailboxFailsClosedOnDigestMismatch(t *testing.T) {
	plan, _ := planArtifact(t, "turn-1")
	wrong := strings.Repeat("a", 64)
	ledger := []state.LedgerEntry{{Revision: 2, TurnID: "turn-1", ArtifactDigest: wrong, Phase: state.PhasePlanDraft}}
	load := func(string, string) ([]byte, error) { return plan, nil }
	if _, err := RenderMailbox(ledger, load); !errors.Is(err, ErrMailboxMismatch) {
		t.Fatalf("err = %v, want ErrMailboxMismatch", err)
	}
}

// The renderer requires strictly-increasing ledger revisions.
func TestRenderMailboxFailsClosedOnOrder(t *testing.T) {
	plan, pd := planArtifact(t, "turn-1")
	review, rd := reviewArtifact(t, "turn-2")
	load := func(turnID, _ string) ([]byte, error) {
		if turnID == "turn-1" {
			return plan, nil
		}
		return review, nil
	}
	ledger := []state.LedgerEntry{
		{Revision: 3, TurnID: "turn-1", ArtifactDigest: pd, Phase: state.PhasePlanDraft},
		{Revision: 3, TurnID: "turn-2", ArtifactDigest: rd, Phase: state.PhaseCheckpoint}, // not increasing
	}
	if _, err := RenderMailbox(ledger, load); !errors.Is(err, ErrMailboxMismatch) {
		t.Fatalf("err = %v, want ErrMailboxMismatch", err)
	}
}

// A phase with no turn spec is not actionable and cannot be rendered.
func TestRenderMailboxFailsClosedOnNonActionablePhase(t *testing.T) {
	plan, pd := planArtifact(t, "turn-1")
	ledger := []state.LedgerEntry{{Revision: 2, TurnID: "turn-1", ArtifactDigest: pd, Phase: state.PhaseInit}}
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
	// The read assignment round-trips to the same canonical bytes.
	canon, _ := a.Marshal()
	gotCanon, _ := got.Marshal()
	if !bytes.Equal(gotCanon, canon) {
		t.Fatalf("inbox assignment does not round-trip to the canonical bytes")
	}
	if got.SessionID != "sess-1" {
		t.Fatalf("inbox session id = %q", got.SessionID)
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

// A tampered inbox (non-canonical / schema-invalid bytes) is never returned as a
// valid assignment: ReadAssignment validates independently of the writer.
func TestReadAssignmentRejectsTamperedInbox(t *testing.T) {
	dir := t.TempDir()
	ss, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("new session store: %v", err)
	}
	defer ss.Close()

	a, err := BuildAssignment(runStateAt(state.PhaseImplementStep), editInputs())
	if err != nil {
		t.Fatalf("build assignment: %v", err)
	}
	if err := ss.WriteAssignment("sess-1", a); err != nil {
		t.Fatalf("write inbox: %v", err)
	}
	// Corrupt the durable inbox bytes out from under the reader.
	inbox := filepath.Join(dir, "sess-1", "assignment.json")
	if err := os.WriteFile(inbox, []byte(`{"message_type":"assignment"`), 0o600); err != nil {
		t.Fatalf("corrupt inbox: %v", err)
	}
	if _, err := ss.ReadAssignment("sess-1"); !errors.Is(err, ErrBadSession) {
		t.Fatalf("read tampered inbox err = %v, want ErrBadSession", err)
	}
}

// The mailbox writer round-trips: what Write persists, Read returns.
func TestMailboxStoreWriteRead(t *testing.T) {
	dir := t.TempDir()
	ms, err := NewMailboxStore(dir)
	if err != nil {
		t.Fatalf("new mailbox store: %v", err)
	}
	defer ms.Close()

	plan, pd := planArtifact(t, "turn-1")
	ledger := []state.LedgerEntry{{Revision: 2, TurnID: "turn-1", ArtifactDigest: pd, Phase: state.PhasePlanDraft}}
	load := func(string, string) ([]byte, error) { return plan, nil }
	if err := ms.Write(ledger, load); err != nil {
		t.Fatalf("write mailbox: %v", err)
	}
	got, err := ms.Read()
	if err != nil {
		t.Fatalf("read mailbox: %v", err)
	}
	want, _ := RenderMailbox(ledger, load)
	if string(got) != want {
		t.Fatalf("mailbox bytes = %q, want %q", got, want)
	}
}
