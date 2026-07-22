package transport

import (
	"encoding/json"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

// receiptMessageType is the wire discriminator for a submit receipt.
const receiptMessageType = "receipt"

// wireReceipt is the on-the-wire submit receipt. It carries the protocol envelope
// (protocol_version + message_type) the durable state.Receipt lacks, and renames Revision to
// the schema's state_revision, so it validates against receipt.v1.json — state.Receipt itself
// must never be marshaled directly onto the wire.
type wireReceipt struct {
	ProtocolVersion int    `json:"protocol_version"`
	MessageType     string `json:"message_type"`
	TurnID          string `json:"turn_id"`
	StateRevision   uint64 `json:"state_revision"`
	ArtifactDigest  string `json:"artifact_digest"`
}

// MarshalReceipt renders a submit receipt as CANONICAL, schema-valid receipt.v1.json bytes: it
// builds the wire envelope, then runs it through protocol.Validate (the same embedded-schema
// path every protocol message uses), returning the canonical bytes. A malformed receipt fails
// closed here rather than emitting an unvalidated shape.
func MarshalReceipt(r state.Receipt) ([]byte, error) {
	w := wireReceipt{
		ProtocolVersion: protocol.SupportedVersion,
		MessageType:     receiptMessageType,
		TurnID:          r.TurnID,
		StateRevision:   r.Revision,
		ArtifactDigest:  r.ArtifactDigest,
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	return protocol.Validate(receiptMessageType, raw)
}
