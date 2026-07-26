package state

import "sort"

// LedgerEntry is one accepted artifact, projected for a human-readable ledger,
// carrying the coordinator-authored accepted Phase (role and artifact type are
// derived from it by the consumer via the turn spec).
type LedgerEntry struct {
	Revision       uint64 `json:"revision"`
	TurnID         string `json:"turn_id"`
	ArtifactDigest string `json:"artifact_digest"`
	Phase          Phase  `json:"phase"`
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
			Phase:          t.Phase,
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

// LatestGitCommit returns the git-commit evidence of the most recent IMPLEMENT_STEP/FIX acceptance
// (the highest receipt revision), or (nil, false) if the run has no git acceptance yet. It is the
// single projection the review-evidence packet and the test gate use to select the head
// commit, so selection is never reinvented.
func LatestGitCommit(rs RunState) (*GitCommitEvidence, bool) {
	var latest *GitCommitEvidence
	for _, e := range Ledger(rs) { // receipt-revision order; the last with evidence is the newest
		if at := rs.AcceptedTurns[e.TurnID]; at.GitCommit != nil {
			latest = at.GitCommit
		}
	}
	return latest, latest != nil
}
