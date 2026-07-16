// Package txn is the prepared-transaction journal (D006/01.5). It bridges the
// several ordered, durable cuts of a coordinator transaction that cannot commit
// atomically together (subject 04 supplies the git ref move, index reconcile, and
// state CAS as concrete steps).
//
// It is a durable PROGRESS machine, not a two-phase flag: the journal records the
// ordered step ids and how many have durably completed. A transaction is COMPLETE
// only after every step's effect is durable and observed. Recovery reconstructs
// the plan, verifies it matches the journalled step ids, re-observes the durable
// prefix, then drives the remaining idempotent steps — Applied advances,
// NotApplied applies and is re-observed, Indeterminate fails closed. So the
// classic "external effect done, crash before state" cut is repaired forward, and
// a transaction is never falsely terminalized. The journal itself is an immutable
// genstore generation sequence (D017), so it never depends on the replace it
// diagnoses, and it composes under the run's shared guard with the state store.
//
// The steps and the intent payload codec are the caller's (subject 04 for git).
// This package is the generic engine, tested against fake steps.
package txn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/redact"
)

const (
	RecordVersion = 1
	IntentVersion = 1
	maxPayload    = 64 * 1024
	maxSteps      = 64
)

// Sentinel errors.
var (
	ErrPending          = errors.New("txn: a non-terminal transaction is already pending")
	ErrNoPending        = errors.New("txn: no matching pending transaction")
	ErrCannotAbort      = errors.New("txn: cannot abort; a step effect is present or applied")
	ErrRecoveryRequired = errors.New("txn: transaction is in an indeterminate state; recovery required")
	ErrPlanMismatch     = errors.New("txn: recovery plan does not match the journalled transaction")
)

// StepStatus reports whether a step's durable effect is already present.
type StepStatus string

const (
	StatusApplied       StepStatus = "applied"
	StatusNotApplied    StepStatus = "not-applied"
	StatusIndeterminate StepStatus = "indeterminate"
)

// Step is one ordered, idempotent durable effect of a transaction.
type Step struct {
	Name   string
	Status func() (StepStatus, error)
	Apply  func() error
}

// Intent is the typed, versioned transaction envelope. The payload is a bounded,
// kind-specific JSON blob whose codec belongs to the caller (subject 04 for git).
type Intent struct {
	Version               int             `json:"version"`
	Kind                  string          `json:"kind"`
	TxnID                 string          `json:"txn_id"`
	ExpectedStateRevision uint64          `json:"expected_state_revision"`
	Payload               json.RawMessage `json:"payload"`
}

// Plan is an intent plus the ordered steps that realize it.
type Plan struct {
	Intent Intent
	Steps  []Step
}

// Record is one journal generation: the durable progress of a transaction.
type Record struct {
	SchemaVersion int      `json:"schema_version"`
	Revision      uint64   `json:"revision"`
	Intent        Intent   `json:"intent"`
	StepIDs       []string `json:"step_ids"`
	StepsDone     int      `json:"steps_done"`
	Complete      bool     `json:"complete"`
	Aborted       bool     `json:"aborted"`
}

// Terminal reports whether the transaction is finished (complete or aborted).
func (r Record) Terminal() bool { return r.Complete || r.Aborted }

// TxnID returns the transaction id.
func (r Record) TxnID() string { return r.Intent.TxnID }

// NextStep is the id of the next step to run, or "" when none remain. It lets a
// read-only status project the exact next action (01.6).
func (r Record) NextStep() string {
	if r.Terminal() || r.StepsDone >= len(r.StepIDs) {
		return ""
	}
	return r.StepIDs[r.StepsDone]
}

// Journal persists transaction progress over a genstore under the run lock.
type Journal struct {
	gs *genstore.Store
}

// Open returns a journal handle (side-effect-free).
func Open(dir, lockPath string) *Journal { return &Journal{gs: genstore.Open(dir, lockPath)} }

// LockPath is the mutation lock guarding this journal.
func (j *Journal) LockPath() string { return j.gs.LockPath() }

// Latest returns the newest journal record (lock-free).
func (j *Journal) Latest() (Record, bool, error) {
	_, r, ok, err := j.latestGS()
	return r, ok, err
}

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

// Run drives a NEW transaction to completion under a held guard.
func (j *Journal) Run(g *genstore.Guard, plan Plan) (Record, error) {
	if err := j.gs.CheckGuard(g); err != nil {
		return Record{}, err
	}
	names, err := validatePlan(plan)
	if err != nil {
		return Record{}, err
	}
	gsRec, cur, ok, err := j.latestGS()
	if err != nil {
		return Record{}, err
	}
	var head genstore.Head
	if ok {
		if !cur.Terminal() {
			return Record{}, fmt.Errorf("%w: %s", ErrPending, cur.TxnID())
		}
		head = gsRec.Head()
	}
	rec, err := j.append(g, head, Record{Intent: plan.Intent, StepIDs: names})
	if err != nil {
		return Record{}, err
	}
	return j.drive(g, rec, plan.Steps)
}

// Recover resumes a pending transaction under a held guard. It reconstructs the
// plan, requires it to match the journalled step ids, re-observes the durable
// prefix, then drives the remaining steps.
func (j *Journal) Recover(g *genstore.Guard, planFor func(Intent) (Plan, error)) (Record, bool, error) {
	// Validate the guard BEFORE any step callback can perform an external effect.
	if err := j.gs.CheckGuard(g); err != nil {
		return Record{}, false, err
	}
	if planFor == nil {
		return Record{}, false, fmt.Errorf("txn: planFor is required")
	}
	_, cur, ok, err := j.latestGS()
	if err != nil {
		return Record{}, false, err
	}
	if !ok || cur.Terminal() {
		return cur, false, nil
	}
	plan, err := planFor(cur.Intent)
	if err != nil {
		return Record{}, false, err
	}
	names, err := validatePlan(plan)
	if err != nil {
		return Record{}, false, err
	}
	if !reflect.DeepEqual(plan.Intent, cur.Intent) || !slicesEqual(names, cur.StepIDs) {
		return Record{}, false, ErrPlanMismatch
	}
	// The durable prefix must still be observably applied.
	for i := 0; i < cur.StepsDone; i++ {
		st, serr := plan.Steps[i].Status()
		if serr != nil {
			return Record{}, false, serr
		}
		if st != StatusApplied {
			return Record{}, false, fmt.Errorf("%w: prefix step %d (%s) is %q", ErrRecoveryRequired, i, plan.Steps[i].Name, st)
		}
	}
	out, err := j.drive(g, cur, plan.Steps)
	return out, true, err
}

// Abort terminates a transaction that has applied no steps. It reconstructs the
// plan and OBSERVES step 0 to close the applied-but-unrecorded window: only a
// definitively NotApplied first step permits abort; Applied must recover forward
// and Indeterminate fails closed.
func (j *Journal) Abort(g *genstore.Guard, plan Plan) (Record, error) {
	if err := j.gs.CheckGuard(g); err != nil {
		return Record{}, err
	}
	names, err := validatePlan(plan)
	if err != nil {
		return Record{}, err
	}
	gsRec, cur, ok, err := j.latestGS()
	if err != nil {
		return Record{}, err
	}
	if !ok || cur.Terminal() || cur.TxnID() != plan.Intent.TxnID {
		return Record{}, fmt.Errorf("%w: %s", ErrNoPending, plan.Intent.TxnID)
	}
	// Bind the supplied intent to the journalled one, so a same-id/same-steps plan
	// with a different payload cannot observe a different target and terminalize
	// the original transaction.
	if !reflect.DeepEqual(plan.Intent, cur.Intent) || !slicesEqual(names, cur.StepIDs) {
		return Record{}, ErrPlanMismatch
	}
	if cur.StepsDone != 0 {
		return Record{}, fmt.Errorf("%w: %d steps applied", ErrCannotAbort, cur.StepsDone)
	}
	switch st, serr := plan.Steps[0].Status(); {
	case serr != nil:
		return Record{}, serr
	case st == StatusNotApplied:
		// safe: nothing has been applied
	case st == StatusApplied:
		return Record{}, fmt.Errorf("%w: step 0 effect is present, recover forward", ErrRecoveryRequired)
	default:
		return Record{}, fmt.Errorf("%w: step 0 is indeterminate", ErrRecoveryRequired)
	}
	return j.append(g, gsRec.Head(), Record{Intent: cur.Intent, StepIDs: cur.StepIDs, Aborted: true})
}

// drive applies steps from rec.StepsDone onward, re-observing each applied step
// before recording durable progress, and marks complete after the last one.
func (j *Journal) drive(g *genstore.Guard, rec Record, steps []Step) (Record, error) {
	for i := rec.StepsDone; i < len(steps); i++ {
		st, err := steps[i].Status()
		if err != nil {
			return Record{}, err
		}
		switch st {
		case StatusApplied:
			// effect already durable (crash after apply, before progress)
		case StatusNotApplied:
			if err := steps[i].Apply(); err != nil {
				return Record{}, err
			}
			// Terminality is based on observed durable state, not the callback.
			after, aerr := steps[i].Status()
			if aerr != nil {
				return Record{}, aerr
			}
			if after != StatusApplied {
				return Record{}, fmt.Errorf("%w: step %d (%s) not observed applied after Apply", ErrRecoveryRequired, i, steps[i].Name)
			}
		default:
			return Record{}, fmt.Errorf("%w: step %d (%s) is %q", ErrRecoveryRequired, i, steps[i].Name, st)
		}
		rec, err = j.advance(g, i+1, i+1 == len(steps))
		if err != nil {
			return Record{}, err
		}
	}
	return rec, nil
}

// advance appends the next progress record, carrying the intent and step ids forward.
func (j *Journal) advance(g *genstore.Guard, stepsDone int, complete bool) (Record, error) {
	gsRec, cur, ok, err := j.latestGS()
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, fmt.Errorf("txn: advance with no prepared record")
	}
	return j.append(g, gsRec.Head(), Record{
		Intent:    cur.Intent,
		StepIDs:   cur.StepIDs,
		StepsDone: stepsDone,
		Complete:  complete,
	})
}

func (j *Journal) append(g *genstore.Guard, head genstore.Head, next Record) (Record, error) {
	built, err := j.gs.AppendLocked(g, head, func(gen uint64, _ string) ([]byte, error) {
		next.SchemaVersion = RecordVersion
		next.Revision = gen
		if err := validate(next); err != nil {
			return nil, err
		}
		if head != (genstore.Head{}) {
			prev, perr := j.headRecord()
			if perr != nil {
				return nil, perr
			}
			if prev.TxnID() == next.TxnID() {
				if err := validateTransition(prev, next); err != nil {
					return nil, err
				}
			} else if !prev.Terminal() {
				return nil, fmt.Errorf("%w: %s", ErrPending, prev.TxnID())
			}
		}
		return json.Marshal(next)
	})
	if err != nil {
		return Record{}, err
	}
	return decode(built)
}

func (j *Journal) headRecord() (Record, error) {
	_, r, ok, err := j.latestGS()
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, fmt.Errorf("txn: expected a head record")
	}
	return r, nil
}

func validate(r Record) error {
	if r.SchemaVersion != RecordVersion {
		return fmt.Errorf("txn: schema_version %d != %d", r.SchemaVersion, RecordVersion)
	}
	if r.Revision == 0 {
		return fmt.Errorf("txn: revision must be > 0")
	}
	if err := validateIntent(r.Intent); err != nil {
		return err
	}
	if err := validateStepIDs(r.StepIDs); err != nil {
		return err
	}
	total := len(r.StepIDs)
	if r.StepsDone < 0 || r.StepsDone > total {
		return fmt.Errorf("txn: steps_done %d out of range 0..%d", r.StepsDone, total)
	}
	if r.Complete && r.Aborted {
		return fmt.Errorf("txn: complete and aborted are mutually exclusive")
	}
	if r.Complete && r.StepsDone != total {
		return fmt.Errorf("txn: complete requires all %d steps done, have %d", total, r.StepsDone)
	}
	if r.Aborted && r.StepsDone != 0 {
		return fmt.Errorf("txn: abort requires zero applied steps, have %d", r.StepsDone)
	}
	// A non-terminal record must have work remaining, so a stuck all-done
	// non-terminal state cannot exist.
	if !r.Terminal() && r.StepsDone >= total {
		return fmt.Errorf("txn: a non-terminal record must have a next step (%d/%d)", r.StepsDone, total)
	}
	return nil
}

func validateTransition(old, next Record) error {
	if !reflect.DeepEqual(old.Intent, next.Intent) {
		return fmt.Errorf("txn: intent is immutable within a transaction")
	}
	if !slicesEqual(old.StepIDs, next.StepIDs) {
		return fmt.Errorf("txn: step ids are immutable within a transaction")
	}
	if old.Terminal() {
		return fmt.Errorf("txn: transaction is already terminal")
	}
	if next.StepsDone < old.StepsDone || next.StepsDone > old.StepsDone+1 {
		return fmt.Errorf("txn: steps_done must advance by 0 or 1 (from %d to %d)", old.StepsDone, next.StepsDone)
	}
	if next.StepsDone == old.StepsDone && !next.Terminal() {
		return fmt.Errorf("txn: a non-terminal record must advance a step")
	}
	return nil
}

func validatePlan(plan Plan) ([]string, error) {
	if err := validateIntent(plan.Intent); err != nil {
		return nil, err
	}
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("txn: a plan must have at least one step")
	}
	names := make([]string, len(plan.Steps))
	for i, s := range plan.Steps {
		if s.Status == nil || s.Apply == nil {
			return nil, fmt.Errorf("txn: step %d (%q) has a nil Status or Apply", i, s.Name)
		}
		names[i] = s.Name
	}
	if err := validateStepIDs(names); err != nil {
		return nil, err
	}
	return names, nil
}

func validateStepIDs(ids []string) error {
	if len(ids) == 0 || len(ids) > maxSteps {
		return fmt.Errorf("txn: step count %d out of range 1..%d", len(ids), maxSteps)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !validID(id) {
			return fmt.Errorf("txn: invalid step id %q", id)
		}
		if seen[id] {
			return fmt.Errorf("txn: duplicate step id %q", id)
		}
		seen[id] = true
	}
	return nil
}

func validateIntent(in Intent) error {
	if in.Version != IntentVersion {
		return fmt.Errorf("txn: intent version %d != %d", in.Version, IntentVersion)
	}
	if !validID(in.TxnID) {
		return fmt.Errorf("txn: invalid txn_id %q", in.TxnID)
	}
	if len(in.Kind) > 64 || !validID(in.Kind) {
		return fmt.Errorf("txn: invalid kind %q", in.Kind)
	}
	if len(in.Payload) == 0 || len(in.Payload) > maxPayload {
		return fmt.Errorf("txn: payload must be non-empty and <= %d bytes", maxPayload)
	}
	if !json.Valid(in.Payload) {
		return fmt.Errorf("txn: payload is not valid json")
	}
	if !bytes.Equal(redact.Bytes(in.Payload), in.Payload) {
		return fmt.Errorf("txn: a secret was detected in the transaction payload")
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

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
