package attach

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// CommitTxnIntentKind is the intent kind of the implementation-submit git-commit transaction: a
// snapshot commit, its ref-CAS, the checked-out index sync, and the state acceptance, journaled as
// one atomic transaction. The coordinator owns the payload codec; attach only binds the envelope.
const CommitTxnIntentKind = "commit-txn"

func (l layout) commitTxnJournalDir(runDir string) string {
	return filepath.Join(runDir, "commit")
}

// CommitTxnJournalClass is the classification of a run's git-commit transaction journal head.
type CommitTxnJournalClass int

const (
	// CommitTxnJournalAbsent means the run has never begun an implementation-submit git commit.
	// This is valid ONLY before the run has any accepted git-commit evidence; a vanished journal
	// after such evidence is corruption, which the state-level chain invariant surfaces separately.
	CommitTxnJournalAbsent CommitTxnJournalClass = iota
	// CommitTxnJournalNonTerminal means a git commit transaction is present but not complete — an
	// in-flight one (recovery required) or a cleanly aborted one. No other run mutator may advance
	// the expected state revision while it is pending: it could leave a ref moved with the state
	// not yet accepted, so the operation is recovery-required until the transaction completes.
	CommitTxnJournalNonTerminal
	// CommitTxnJournalTerminal means a complete, identity-bound git commit transaction; the ref,
	// index, and state acceptance are durably reflected and an ordinary operation may proceed.
	CommitTxnJournalTerminal
)

// commitTxnEnvelope is the minimal identity attach binds from the payload the coordinator wrote;
// the full acceptance plan is opaque here.
type commitTxnEnvelope struct {
	RunID string `json:"run_id"`
}

// ClassifyCommitTxnJournal classifies a resolved run's git-commit transaction journal head under a
// held run guard. Like the pair/replace classifiers it proves g is this journal's live lock before
// reading, binds every present record to its envelope (kind, version, txn id, expected state
// revision) and payload run id, and only then reports a complete record Terminal and a
// pending/aborted one NonTerminal. A mis-bound record is an error (recovery-required), never a class.
func ClassifyCommitTxnJournal(g *genstore.Guard, loc RunLocation) (CommitTxnJournalClass, error) {
	journal := txn.Open(loc.CommitTxnDir, loc.RunLock)
	if err := journal.CheckGuard(g); err != nil {
		return CommitTxnJournalAbsent, err
	}
	rec, ok, err := journal.Latest()
	if err != nil {
		return CommitTxnJournalAbsent, err
	}
	return classifyCommitTxnRecord(rec, ok, loc.RunID)
}

func classifyCommitTxnRecord(rec txn.Record, ok bool, runID string) (CommitTxnJournalClass, error) {
	if !ok {
		return CommitTxnJournalAbsent, nil
	}
	if rec.Intent.Version != txn.IntentVersion || rec.Intent.Kind != CommitTxnIntentKind {
		return CommitTxnJournalAbsent, fmt.Errorf("%w: commit-txn journal kind/version mismatch", ErrJoinUnauthorized)
	}
	var env commitTxnEnvelope
	if err := json.Unmarshal(rec.Intent.Payload, &env); err != nil {
		return CommitTxnJournalAbsent, fmt.Errorf("%w: commit-txn payload undecodable: %v", ErrJoinUnauthorized, err)
	}
	if env.RunID != runID {
		return CommitTxnJournalAbsent, fmt.Errorf("%w: commit-txn journal identity mismatch", ErrJoinUnauthorized)
	}
	if rec.Complete && !rec.Aborted {
		return CommitTxnJournalTerminal, nil
	}
	return CommitTxnJournalNonTerminal, nil
}
