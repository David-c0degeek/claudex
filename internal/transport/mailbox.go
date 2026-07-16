package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/David-c0degeek/claudex/internal/state"
)

const inboxPerm = 0o600

var (
	// ErrBadSession means a session id is not canonical or does not match the
	// assignment.
	ErrBadSession = errors.New("transport: invalid session")
	// ErrMailboxMismatch means an accepted-turn ledger entry disagrees with its
	// stored artifact.
	ErrMailboxMismatch = errors.New("transport: mailbox entry disagrees with its artifact")
)

// SessionStore writes role-addressed assignment inboxes under a confined session
// root, so a session id can never escape it. The inbox is a mutable, rebuildable
// projection: it is atomically replaced each turn.
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

// WriteAssignment delivers the assignment to sessionID's inbox. sessionID must
// be canonical and equal the assignment's SessionID; the canonical assignment
// bytes replace <sessionID>/assignment.json atomically within the root.
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
	if err := s.root.Mkdir(sessionID, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return s.replaceInRoot(sessionID+"/assignment.json", canon)
}

// ReadAssignment reads a session's current inbox bytes.
func (s *SessionStore) ReadAssignment(sessionID string) ([]byte, error) {
	if !state.IsRunID(sessionID) {
		return nil, fmt.Errorf("%w: session id", ErrBadSession)
	}
	f, err := s.root.Open(sessionID + "/assignment.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxArtifactBytes+1))
}

// replaceInRoot writes data to a fresh temp within the root, then renames it over
// the target — an atomic replace on POSIX; on Windows the OS provides no
// old-or-new guarantee (see atomicfile), which is acceptable for a rebuildable
// mutable projection.
func (s *SessionStore) replaceInRoot(rel string, data []byte) (err error) {
	dir := rel[:strings.LastIndex(rel, "/")]
	tmp := dir + "/.claudex-tmp-" + randHex()
	f, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, inboxPerm)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = s.root.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = s.root.Rename(tmp, rel); err != nil {
		return err
	}
	renamed = true
	return nil
}

// RenderMailbox derives the append-only human-readable transcript from the
// accepted-turn ledger (in order) and the immutable artifacts. It fails closed
// if an entry disagrees with its artifact, and every block is built from
// coordinator-known role/phase plus injection-safe typed summaries — no raw
// free text — so a longer ledger's rendering has a shorter one's as an exact
// prefix.
func RenderMailbox(ledger []state.LedgerEntry, load func(turnID, digest string) ([]byte, error)) (string, error) {
	var b strings.Builder
	for _, e := range ledger {
		if !state.IsRunID(e.TurnID) || (e.Role != "lead" && e.Role != "pair") || e.MessageType == "" {
			return "", fmt.Errorf("%w: malformed ledger entry", ErrMailboxMismatch)
		}
		artifact, err := load(e.TurnID, e.ArtifactDigest)
		if err != nil {
			return "", err
		}
		var env struct {
			MessageType string `json:"message_type"`
			TurnID      string `json:"turn_id"`
		}
		if err := json.Unmarshal(artifact, &env); err != nil {
			return "", fmt.Errorf("%w: envelope", ErrMailboxMismatch)
		}
		if env.TurnID != e.TurnID || env.MessageType != e.MessageType {
			return "", fmt.Errorf("%w: artifact does not match the ledger entry", ErrMailboxMismatch)
		}
		fmt.Fprintf(&b, "===== [%s] turn %s | %s | ACCEPTED =====\n", strings.ToUpper(e.Role), e.TurnID, e.Phase)
		fmt.Fprintf(&b, "%s: %s\n\n", e.MessageType, summarize(e.MessageType, artifact))
	}
	return b.String(), nil
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
	// Defense: never echo an unexpected value.
	return "?"
}
