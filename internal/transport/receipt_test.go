package transport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

// MarshalReceipt renders a durable receipt as canonical bytes that carry the protocol envelope
// (protocol_version + message_type) and the schema's state_revision field, and it re-validates
// them against the embedded receipt.v1.json — state.Receipt (which lacks those fields and names
// revision differently) is never emitted directly.
func TestMarshalReceipt(t *testing.T) {
	r := state.Receipt{TurnID: "turn-1", Revision: 7, ArtifactDigest: strings.Repeat("a", 64)}
	b, err := MarshalReceipt(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// It re-validates against the embedded schema.
	if _, err := protocol.Validate("receipt", b); err != nil {
		t.Fatalf("emitted receipt does not re-validate: %v", err)
	}
	var got struct {
		ProtocolVersion int    `json:"protocol_version"`
		MessageType     string `json:"message_type"`
		TurnID          string `json:"turn_id"`
		StateRevision   uint64 `json:"state_revision"`
		ArtifactDigest  string `json:"artifact_digest"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ProtocolVersion != 1 || got.MessageType != "receipt" || got.TurnID != "turn-1" ||
		got.StateRevision != 7 || got.ArtifactDigest != r.ArtifactDigest {
		t.Fatalf("wire receipt wrong: %+v", got)
	}
	// state.Receipt's own JSON is NOT schema-valid (no envelope, wrong field name).
	raw, _ := json.Marshal(r)
	if _, err := protocol.Validate("receipt", raw); err == nil {
		t.Fatal("raw state.Receipt should not validate as a wire receipt")
	}
}
