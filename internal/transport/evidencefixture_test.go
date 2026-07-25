package transport

import (
	"fmt"

	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/state"
)

// bindEvidence gives next the evidence binding its assignment requires, or clears it when the turn
// carries a worktree instead. Transport's fixtures build run states directly, so they must satisfy
// the same v7 invariant the issuance authorities produce: a read-only actionable assignment exists
// if and only if a binding does.
func bindEvidence(next *state.RunState, rev uint64) {
	next.Evidence = nil
	if next.Assignment == nil || state.RepoEditPhase(next.Phase) {
		return
	}
	next.Evidence = testBinding(next.Assignment.ID, rev)
}

func testBinding(turnID string, rev uint64) *state.AssignmentEvidence {
	return &state.AssignmentEvidence{
		TurnID:          turnID,
		IssuedRevision:  rev,
		ManifestRelPath: evidence.PacketManifestRel(turnID),
		RootDigest:      testRootDigest(turnID),
	}
}

// testRootDigest is a stable 64-hex value derived from the turn id, so re-binding the same turn
// agrees exactly as a real content-addressed packet would.
func testRootDigest(turnID string) string {
	var sum uint64 = 1469598103934665603
	for i := 0; i < len(turnID); i++ {
		sum ^= uint64(turnID[i])
		sum *= 1099511628211
	}
	return fmt.Sprintf("%016x%016x%016x%016x", sum, sum^0xffffffffffffffff, sum*3, sum*7)
}
