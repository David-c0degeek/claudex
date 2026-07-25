package attach

import "github.com/David-c0degeek/claudex/internal/state"

// EvidenceIssuer publishes the immutable review-evidence packet for the read-only turn a transaction
// is about to issue, and returns its hash-bound locator (the packet manifest's run-relative path and
// the sha256 of its exact canonical bytes).
//
// attach owns WHEN a packet must exist — it is an issuance authority, and a read-only assignment is
// actionable only through one — but never WHAT is in it. Resolving repository content belongs to
// internal/reviewpacket, and routing it through a seam keeps attach free of any Git dependency,
// exactly as BaseResolver, Preflighter, and WorktreeProvisioner already do.
//
// It MUST be called during the locked authorized preparation, before the transaction opens: every
// failure it can return is a deterministic authoring failure (a selector absent from the source
// tree, a bounds breach, an unreadable frozen snapshot), and those must refuse the attach with the
// Registry, the journal, and RunState all untouched.
type EvidenceIssuer interface {
	IssueEvidence(turnID string, phase state.Phase, rs state.RunState) (manifestRelPath, rootDigest string, err error)
}
