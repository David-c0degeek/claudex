package state

import (
	"fmt"

	"github.com/David-c0degeek/claudex/internal/evidence"
)

// testBinding is the evidence binding a read-only assignment must carry. State's own obligation is
// the invariant — a read-only actionable assignment exists if and only if a binding does, bound to
// the same turn and revision at the derived packet path — so these fixtures supply a well-formed
// binding rather than a real packet, which is internal/evidence's contract.
func testBinding(turnID string, rev uint64) *AssignmentEvidence {
	return &AssignmentEvidence{
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
