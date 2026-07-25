package attach

import (
	"fmt"

	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/state"
)

// stubIssuer stands in for the real reviewpacket issuer in attach's unit tests. attach's own
// obligation is to publish BEFORE the transaction opens and to bind exactly what it published; what
// a packet contains is internal/evidence's contract and internal/reviewpacket's resolution, both
// tested there and end to end in the coordinator and CLI suites. The stub therefore returns the
// derived path for the turn (which the intent validators check) and a deterministic digest.
type stubIssuer struct {
	err   error    // when set, models a deterministic packet failure
	calls []string // turn ids it was asked to publish, in order
}

func (s *stubIssuer) IssueEvidence(turnID string, _ state.Phase, _ state.RunState) (string, string, error) {
	if s.err != nil {
		return "", "", s.err
	}
	s.calls = append(s.calls, turnID)
	return evidence.PacketManifestRel(turnID), stubDigest(turnID), nil
}

// stubDigest is a stable 64-hex value derived from the turn id, so two publications of the same turn
// agree exactly as a real content-addressed packet would.
func stubDigest(turnID string) string {
	var sum uint64 = 1469598103934665603
	for i := 0; i < len(turnID); i++ {
		sum ^= uint64(turnID[i])
		sum *= 1099511628211
	}
	return fmt.Sprintf("%016x%016x%016x%016x", sum, sum^0xffffffffffffffff, sum*3, sum*7)
}
