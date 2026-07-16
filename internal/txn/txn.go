// Package txn is the prepared-transaction journal (D006/01.5). It bridges a
// non-idempotent external operation (subject 04 plugs in the git ref move) and
// the coordinator state CAS, which cannot commit atomically together.
//
// The journal is itself an immutable generation sequence (D017) via genstore, so
// it never depends on the single replace whose failure it diagnoses. A
// transaction lifecycle is: Prepare (record the intent) → the participant commits
// the external operation → MarkCommitted; a failure before commit is resolved by
// Reconcile, which observes the participant and either replays forward
// (MarkCommitted) or rolls back (MarkAborted), deterministically and idempotently.
//
// This package is the generic machine, tested against a fake participant. It
// composes under the run's shared genstore.Guard so the caller can run
// journal-prepare → participant → state-append → journal-commit in one critical
// section.
package txn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/redact"
)

// RecordVersion is the on-disk schema version; an unknown version fails closed.
const RecordVersion = 1

// Sentinel errors.
var (
	ErrRevisionConflict = errors.New("txn: revision conflict")
	ErrPending          = errors.New("txn: a prepared transaction is already pending")
	ErrNoPending        = errors.New("txn: no matching prepared transaction")
)

// Phase is the lifecycle phase of a transaction.
type Phase string

const (
	Prepared  Phase = "prepared"
	Committed Phase = "committed"
	Aborted   Phase = "aborted"
)

// Participant performs and observes the external non-idempotent operation named
// by a transaction's opaque intent (e.g. a git ref CAS).
type Participant interface {
	// Commit performs the operation described by intent.
	Commit(intent []byte) error
	// Observe reports whether the operation described by intent has taken effect.
	Observe(intent []byte) (committed bool, err error)
}

// Record is one journal generation.
type Record struct {
	SchemaVersion int    `json:"schema_version"`
	Revision      uint64 `json:"revision"`
	TxnID         string `json:"txn_id"`
	Phase         Phase  `json:"phase"`
	Intent        []byte `json:"intent"`
}

// Terminal reports whether the record is a completed transaction.
func (r Record) Terminal() bool { return r.Phase == Committed || r.Phase == Aborted }

// Journal persists transaction records over a genstore under the run lock.
type Journal struct {
	gs *genstore.Store
}

// Open returns a journal handle (side-effect-free). lockPath must be the run's
// mutation lock, shared with the run-state store.
func Open(dir, lockPath string) *Journal {
	return &Journal{gs: genstore.Open(dir, lockPath)}
}

// LockPath is the mutation lock guarding this journal.
func (j *Journal) LockPath() string { return j.gs.LockPath() }

// Latest returns the newest journal record (lock-free).
func (j *Journal) Latest() (Record, bool, error) {
	gsRec, r, ok, err := j.latestGS()
	_ = gsRec
	return r, ok, err
}

// latestGS returns the raw genstore record (for its head) and the decoded record.
func (j *Journal) latestGS() (genstore.Record, Record, bool, error) {
	gsRec, ok, err := j.gs.Latest()
	if err != nil {
		return genstore.Record{}, Record{}, false, err
	}
	if !ok {
		return genstore.Record{}, Record{}, false, nil
	}
	r, err := decode(gsRec)
	if err != nil {
		return genstore.Record{}, Record{}, false, err
	}
	return gsRec, r, true, nil
}

// PrepareLocked records a new transaction intent under a held guard. It fails if
// a prepared (non-terminal) transaction is already pending.
func (j *Journal) PrepareLocked(g *genstore.Guard, txnID string, intent []byte) (Record, error) {
	gsRec, cur, ok, err := j.latestGS()
	if err != nil {
		return Record{}, err
	}
	var head genstore.Head
	if ok {
		if !cur.Terminal() {
			return Record{}, fmt.Errorf("%w: %s still %s", ErrPending, cur.TxnID, cur.Phase)
		}
		head = gsRec.Head()
	}
	return j.appendPhase(g, head, txnID, Prepared, intent)
}

// MarkCommittedLocked records that the pending transaction's participant committed.
func (j *Journal) MarkCommittedLocked(g *genstore.Guard, txnID string) (Record, error) {
	return j.markLocked(g, txnID, Committed)
}

// MarkAbortedLocked records that the pending transaction was rolled back.
func (j *Journal) MarkAbortedLocked(g *genstore.Guard, txnID string) (Record, error) {
	return j.markLocked(g, txnID, Aborted)
}

// ReconcileLocked resolves a pending prepared transaction by observing the
// participant: committed → MarkCommitted (replay forward), otherwise MarkAborted
// (roll back). It is idempotent — with no pending transaction it returns the
// latest record unchanged.
func (j *Journal) ReconcileLocked(g *genstore.Guard, p Participant) (Record, bool, error) {
	_, cur, ok, err := j.latestGS()
	if err != nil {
		return Record{}, false, err
	}
	if !ok || cur.Terminal() {
		return cur, false, nil // nothing pending
	}
	committed, err := p.Observe(cur.Intent)
	if err != nil {
		return Record{}, false, err
	}
	phase := Aborted
	if committed {
		phase = Committed
	}
	out, err := j.markLocked(g, cur.TxnID, phase)
	if err != nil {
		return Record{}, false, err
	}
	return out, true, nil
}

func (j *Journal) markLocked(g *genstore.Guard, txnID string, phase Phase) (Record, error) {
	gsRec, cur, ok, err := j.latestGS()
	if err != nil {
		return Record{}, err
	}
	if !ok || cur.Terminal() || cur.TxnID != txnID {
		return Record{}, fmt.Errorf("%w: %s -> %s", ErrNoPending, txnID, phase)
	}
	return j.appendPhase(g, gsRec.Head(), txnID, phase, cur.Intent)
}

func (j *Journal) appendPhase(g *genstore.Guard, head genstore.Head, txnID string, phase Phase, intent []byte) (Record, error) {
	built, err := j.gs.AppendLocked(g, head, func(gen uint64, _ string) ([]byte, error) {
		next := Record{SchemaVersion: RecordVersion, Revision: gen, TxnID: txnID, Phase: phase, Intent: intent}
		if err := validate(next); err != nil {
			return nil, err
		}
		// The intent is control data: reject a secret rather than laundering it.
		if !bytes.Equal(redact.Bytes(next.Intent), next.Intent) {
			return nil, fmt.Errorf("txn: a secret was detected in the transaction intent")
		}
		return json.Marshal(next)
	})
	if err != nil {
		return Record{}, err
	}
	return decode(built)
}

func validate(r Record) error {
	if r.SchemaVersion != RecordVersion {
		return fmt.Errorf("txn: schema_version %d != %d", r.SchemaVersion, RecordVersion)
	}
	if r.Revision == 0 {
		return fmt.Errorf("txn: revision must be > 0")
	}
	if !validID(r.TxnID) {
		return fmt.Errorf("txn: invalid txn_id %q", r.TxnID)
	}
	switch r.Phase {
	case Prepared, Committed, Aborted:
	default:
		return fmt.Errorf("txn: unknown phase %q", r.Phase)
	}
	return nil
}

func decode(rec genstore.Record) (Record, error) {
	dec := json.NewDecoder(bytes.NewReader(rec.Payload))
	dec.DisallowUnknownFields()
	var r Record
	if err := dec.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("txn: decode generation %d: %w", rec.Generation, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Record{}, fmt.Errorf("txn: unexpected trailing content in generation %d", rec.Generation)
	}
	if r.Revision != rec.Generation {
		return Record{}, fmt.Errorf("txn: revision %d disagrees with generation %d", r.Revision, rec.Generation)
	}
	if err := validate(r); err != nil {
		return Record{}, fmt.Errorf("txn: generation %d invalid: %w", rec.Generation, err)
	}
	return r, nil
}

// validID is a filename-safe bounded identifier: 1..128 of [A-Za-z0-9._-],
// alphanumeric start, not a reserved dot name.
func validID(s string) bool {
	if len(s) == 0 || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	if c := s[0]; !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
