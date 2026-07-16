package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/redact"
	"github.com/David-c0degeek/claudex/internal/state"
)

const (
	inboxPerm      = 0o600
	maxInboxBytes  = 1 << 20
	maxMailboxSize = 8 << 20
)

var (
	// ErrBadSession means a session id is not canonical or does not match the
	// assignment.
	ErrBadSession = errors.New("transport: invalid session")
	// ErrMailboxMismatch means an accepted-turn ledger entry disagrees with its
	// stored artifact, or the ledger is out of order.
	ErrMailboxMismatch = errors.New("transport: mailbox entry disagrees with its artifact")
	// ErrMailboxTooLarge means the rendered transcript exceeds the readable bound,
	// so the writer refuses to persist a file its own reader would reject.
	ErrMailboxTooLarge = errors.New("transport: rendered transcript exceeds the mailbox size limit")
)

// SessionStore writes and reads role-addressed assignment inboxes under a
// confined session root, so a session id can never escape it. The inbox is a
// mutable, rebuildable projection, atomically replaced each turn.
type SessionStore struct {
	root *os.Root
}

// NewSessionStore roots a session store at dir (e.g. .claudex/session).
func NewSessionStore(dir string) (*SessionStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &SessionStore{root: r}, nil
}

// Close releases the root handle.
func (s *SessionStore) Close() error { return s.root.Close() }

// WriteAssignment delivers the assignment to sessionID's inbox atomically.
// sessionID must be canonical and equal the assignment's SessionID.
func (s *SessionStore) WriteAssignment(sessionID string, a Assignment) error {
	if !state.IsRunID(sessionID) {
		return fmt.Errorf("%w: session id", ErrBadSession)
	}
	if a.SessionID != sessionID {
		return fmt.Errorf("%w: assignment session id disagrees", ErrBadSession)
	}
	canon, err := a.Marshal() // validated canonical bytes
	if err != nil {
		return err
	}
	if err := atomicfile.MkdirInRoot(s.root, sessionID, 0o700); err != nil {
		return err
	}
	return atomicfile.ReplaceInRoot(s.root, sessionID+"/assignment.json", canon, inboxPerm)
}

// ReadAssignment reads and fully validates a session's current inbox: a bounded
// regular file whose canonical, schema-valid, semantically-valid assignment is
// addressed to sessionID. It never returns torn or tampered bytes.
func (s *SessionStore) ReadAssignment(sessionID string) (Assignment, error) {
	if !state.IsRunID(sessionID) {
		return Assignment{}, fmt.Errorf("%w: session id", ErrBadSession)
	}
	raw, err := atomicfile.ReadInRoot(s.root, sessionID+"/assignment.json", maxInboxBytes)
	if err != nil {
		return Assignment{}, err
	}
	// Validate against the schema and require the bytes are already canonical
	// (protocol.Validate returns the canonical form): a whitespace-reformatted but
	// otherwise valid tampering is refused, not silently accepted.
	canonical, err := protocol.Validate(assignmentType, raw)
	if err != nil {
		return Assignment{}, fmt.Errorf("%w: inbox schema", ErrBadSession)
	}
	if !bytes.Equal(canonical, raw) {
		return Assignment{}, fmt.Errorf("%w: inbox not canonical", ErrBadSession)
	}
	var a Assignment
	if err := json.Unmarshal(canonical, &a); err != nil {
		return Assignment{}, fmt.Errorf("%w: inbox decode", ErrBadSession)
	}
	// Collapse semantic-validation failures to the stable, value-free inbox error
	// so no arbitrary validation text crosses the display boundary.
	if err := a.Validate(); err != nil {
		return Assignment{}, fmt.Errorf("%w: inbox invalid", ErrBadSession)
	}
	if a.SessionID != sessionID {
		return Assignment{}, fmt.Errorf("%w: inbox session id disagrees", ErrBadSession)
	}
	return a, nil
}

// MailboxStore writes the single human-readable transcript under a confined root.
type MailboxStore struct {
	root *os.Root
}

// NewMailboxStore roots the mailbox at dir (e.g. .claudex).
func NewMailboxStore(dir string) (*MailboxStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &MailboxStore{root: r}, nil
}

// Close releases the root handle.
func (m *MailboxStore) Close() error { return m.root.Close() }

// Write renders the transcript from the ledger and artifacts and atomically
// replaces mailbox.md. The transcript is a rebuildable projection.
func (m *MailboxStore) Write(ledger []state.LedgerEntry, load func(turnID, digest string) ([]byte, error)) error {
	md, err := RenderMailbox(ledger, load)
	if err != nil {
		return err
	}
	// Never persist a transcript the reader would refuse as over-limit.
	if int64(len(md)) > maxMailboxSize {
		return ErrMailboxTooLarge
	}
	return atomicfile.ReplaceInRoot(m.root, "mailbox.md", []byte(md), inboxPerm)
}

// Read returns the current transcript bytes.
func (m *MailboxStore) Read() ([]byte, error) {
	return atomicfile.ReadInRoot(m.root, "mailbox.md", maxMailboxSize)
}

// RenderMailbox derives the append-only transcript from the accepted-turn ledger
// (in strictly-increasing revision order) and the immutable artifacts. It fails
// closed and independently re-validates every artifact — recomputed digest,
// canonical and redacted bytes, and the schema for the type derived from
// TurnSpec(entry.Phase) — so the loader cannot feed corrupt or unaddressed bytes.
// Role and artifact type come solely from the phase; summaries are injection-safe
// typed counts and allowlisted enums. A longer ledger's render has a shorter
// one's as an exact prefix.
func RenderMailbox(ledger []state.LedgerEntry, load func(turnID, digest string) ([]byte, error)) (string, error) {
	if len(ledger) > 0 && load == nil {
		return "", fmt.Errorf("%w: nil artifact loader", ErrMailboxMismatch)
	}
	var b strings.Builder
	var prevRevision uint64
	seen := make(map[string]bool)
	for i, e := range ledger {
		if e.Revision == 0 {
			return "", fmt.Errorf("%w: ledger entry has revision 0", ErrMailboxMismatch)
		}
		if i > 0 && e.Revision <= prevRevision {
			return "", fmt.Errorf("%w: ledger revisions are not strictly increasing", ErrMailboxMismatch)
		}
		prevRevision = e.Revision
		if seen[e.TurnID] {
			return "", fmt.Errorf("%w: duplicate turn in the ledger", ErrMailboxMismatch)
		}
		seen[e.TurnID] = true

		if !state.IsRunID(e.TurnID) || !state.IsHex64(e.ArtifactDigest) {
			return "", fmt.Errorf("%w: malformed ledger entry", ErrMailboxMismatch)
		}
		spec, ok := TurnSpec(e.Phase)
		if !ok {
			return "", fmt.Errorf("%w: ledger phase is not actionable", ErrMailboxMismatch)
		}

		artifact, err := load(e.TurnID, e.ArtifactDigest)
		if err != nil {
			return "", err
		}
		if err := validateForRender(e, spec, artifact); err != nil {
			return "", err
		}

		fmt.Fprintf(&b, "===== [%s] turn %s | %s | ACCEPTED =====\n", strings.ToUpper(string(spec.Role)), e.TurnID, e.Phase)
		fmt.Fprintf(&b, "%s: %s\n\n", spec.ArtifactMessageType, summarize(spec.ArtifactMessageType, artifact))
	}
	return b.String(), nil
}

// validateForRender re-verifies an artifact against its ledger entry and the
// phase's expected type, independent of whatever the loader returned.
func validateForRender(e state.LedgerEntry, spec TurnSpecEntry, artifact []byte) error {
	if int64(len(artifact)) == 0 || int64(len(artifact)) > maxArtifactBytes {
		return fmt.Errorf("%w: artifact size", ErrMailboxMismatch)
	}
	c, err := canonjson.Canonicalize(artifact)
	if err != nil || !bytes.Equal(c, artifact) {
		return fmt.Errorf("%w: artifact not canonical", ErrMailboxMismatch)
	}
	r, err := canonjson.Canonicalize(redact.Bytes(artifact))
	if err != nil || !bytes.Equal(r, artifact) {
		return fmt.Errorf("%w: artifact not redacted", ErrMailboxMismatch)
	}
	sum := sha256.Sum256(artifact)
	if hex.EncodeToString(sum[:]) != e.ArtifactDigest {
		return fmt.Errorf("%w: artifact digest", ErrMailboxMismatch)
	}
	if _, err := protocol.Validate(spec.ArtifactMessageType, artifact); err != nil {
		return fmt.Errorf("%w: artifact schema", ErrMailboxMismatch)
	}
	var env struct {
		TurnID string `json:"turn_id"`
	}
	if err := json.Unmarshal(artifact, &env); err != nil || env.TurnID != e.TurnID {
		return fmt.Errorf("%w: artifact turn id", ErrMailboxMismatch)
	}
	return nil
}

// summarize renders an injection-safe, redacted one-line summary using only
// structural counts and allowlisted enum values — never raw free text.
func summarize(messageType string, artifact []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(artifact, &m); err != nil {
		return "unreadable"
	}
	switch messageType {
	case "plan":
		return fmt.Sprintf("steps=%d risks=%d open_questions=%d", arrLen(m["steps"]), arrLen(m["risks"]), arrLen(m["open_questions"]))
	case "plan_critique":
		return fmt.Sprintf("verdict=%s findings=%d checks=%d", enumField(m["verdict"]), arrLen(m["findings"]), arrLen(m["implementation_checks"]))
	case "plan_revision":
		return fmt.Sprintf("responses=%d", arrLen(m["responses"]))
	case "implementation_report":
		return fmt.Sprintf("files_changed=%d deviations=%d", arrLen(m["files_changed"]), arrLen(m["deviations_from_plan"]))
	case "checkpoint_review":
		return fmt.Sprintf("verdict=%s findings=%d missing_evidence=%d", enumField(m["verdict"]), arrLen(m["findings"]), arrLen(m["missing_evidence"]))
	case "verification":
		return fmt.Sprintf("verdict=%s criteria=%d", enumField(m["verdict"]), arrLen(m["criteria"]))
	default:
		return "accepted"
	}
}

func arrLen(raw json.RawMessage) int {
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		return 0
	}
	return len(a)
}

// enumField returns a short verdict only if it is a clean allowlisted token, so
// no free text (or markdown/control characters) reaches the transcript.
func enumField(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "?"
	}
	switch s {
	case "AGREE", "REVISE", "pass", "fail":
		return s
	}
	return "?"
}
