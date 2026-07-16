package state

import "sort"

// LedgerEntry is one accepted artifact, projected for a human-readable ledger,
// with the coordinator-authored role/phase/message_type carried through.
type LedgerEntry struct {
	Revision       uint64 `json:"revision"`
	TurnID         string `json:"turn_id"`
	ArtifactDigest string `json:"artifact_digest"`
	Role           string `json:"role"`
	Phase          Phase  `json:"phase"`
	MessageType    string `json:"message_type"`
}

// Ledger derives the append-only ledger purely from the accepted artifacts. It
// is a projection, never an independent source of truth: two states with the
// same accepted turns yield byte-identical ledgers regardless of any other
// field, and the ledger reconstructs from AcceptedTurns alone.
//
// Entries are ordered by the receipt revision that accepted them, then by
// turn_id, so the order is deterministic and stable across recovery.
func Ledger(rs RunState) []LedgerEntry {
	entries := make([]LedgerEntry, 0, len(rs.AcceptedTurns))
	for id, t := range rs.AcceptedTurns {
		entries = append(entries, LedgerEntry{
			Revision:       t.Receipt.Revision,
			TurnID:         id,
			ArtifactDigest: t.ArtifactDigest,
			Role:           t.Role,
			Phase:          t.Phase,
			MessageType:    t.MessageType,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Revision != entries[j].Revision {
			return entries[i].Revision < entries[j].Revision
		}
		return entries[i].TurnID < entries[j].TurnID
	})
	return entries
}
