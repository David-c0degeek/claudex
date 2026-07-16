// Package state is the coordinator's typed, validated run state, persisted as
// immutable generations (D017) via internal/genstore. State never overwrites in
// place; each mutation is a compare-and-swap that appends the next generation.
//
// The mutator runs INSIDE the generation builder, after genstore has chosen the
// next (possibly gap-skipped) generation number, so identities and receipts bind
// to the exact resulting revision (D004/01.2). Every mutation is validated for
// its own invariants and as a transition from the previous state (bootstrap
// fields immutable, counters non-decreasing, accepted turns append-only). Free
// text is redacted before persisting; a secret in an executable/control field is
// rejected (never silently rewritten). The returned value is decoded from the
// exact persisted bytes, so it never diverges from durable state.
//
// State holds only typed identities, digests, and relative references — never the
// protocol artifacts themselves (those arrive with subject 02).
package state

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/genstore"
)

// RunStateVersion is the on-disk schema version; an unknown version fails closed.
const RunStateVersion = 1

// ErrRevisionConflict is returned when a mutation's expected revision does not
// match the current head.
var ErrRevisionConflict = errors.New("state: revision conflict")

// Lifecycle is the durable macro lifecycle of a run.
type Lifecycle string

const (
	LifecycleRunning         Lifecycle = "running"
	LifecyclePaused          Lifecycle = "paused"
	LifecyclePausedBudget    Lifecycle = "paused_budget"
	LifecycleRateLimited     Lifecycle = "rate_limited"
	LifecycleCancelled       Lifecycle = "cancelled"
	LifecycleFailedRetryable Lifecycle = "failed_retryable"
	LifecycleFailedTerminal  Lifecycle = "failed_terminal"
	LifecycleCompleted       Lifecycle = "completed"
)

// Phase is the current phase of the pairing loop. State owns the known values;
// the engine (subject 03) owns the allowed transitions between them.
type Phase string

const (
	PhaseInit          Phase = "INIT"
	PhasePlanDraft     Phase = "PLAN_DRAFT"
	PhasePlanCritique  Phase = "PLAN_CRITIQUE"
	PhasePlanRevise    Phase = "PLAN_REVISE"
	PhaseImplementStep Phase = "IMPLEMENT_STEP"
	PhaseCheckpoint    Phase = "CHECKPOINT"
	PhaseFix           Phase = "FIX"
	PhaseTests         Phase = "TESTS"
	PhaseVerify        Phase = "VERIFY"
	PhaseAwaitGuidance Phase = "AWAIT_GUIDANCE"
	PhaseDone          Phase = "DONE"
)

// SnapshotRef points at a hashed, copied input inside the run directory (relative
// local path + exact digest of the bytes), never the live source.
type SnapshotRef struct {
	RelPath string `json:"rel_path"`
	Digest  string `json:"digest"`
}

// FSResult is the frozen filesystem classification and any operator ack.
type FSResult struct {
	Class        string `json:"class"`
	Reason       string `json:"reason"` // free text, redactable
	Acknowledged bool   `json:"acknowledged"`
}

// Counters are lead-response budgets consumed so far. Non-negative, non-decreasing.
type Counters struct {
	PlanRevisions int   `json:"plan_revisions"`
	TestFixes     int   `json:"test_fixes"`
	VerifyFixes   int   `json:"verify_fixes"`
	StepFixes     []int `json:"step_fixes"`
}

// Ref binds an issued identity to the revision that issued it (D004/01.2).
type Ref struct {
	ID             string `json:"id"`
	IssuedRevision uint64 `json:"issued_revision"`
}

// Receipt is the durable acknowledgement of an accepted submit.
type Receipt struct {
	TurnID         string `json:"turn_id"`
	Revision       uint64 `json:"revision"`
	ArtifactDigest string `json:"artifact_digest"`
}

// AcceptedTurn records what was accepted for a turn (keyed by turn_id in the map).
type AcceptedTurn struct {
	ArtifactDigest string  `json:"artifact_digest"`
	Receipt        Receipt `json:"receipt"`
}

// Projection is a typed recovery or failure summary.
type Projection struct {
	Code       string `json:"code"`
	Reason     string `json:"reason"` // free text, redactable
	NextAction string `json:"next_action"`
	AtRevision uint64 `json:"at_revision"`
}

// RunState is the authoritative typed state of a run. Absent refs/projections are
// nil (distinct from a present all-zero record).
type RunState struct {
	SchemaVersion   int                     `json:"schema_version"`
	RunID           string                  `json:"run_id"`
	Revision        uint64                  `json:"revision"`
	Lifecycle       Lifecycle               `json:"lifecycle"`
	Phase           Phase                   `json:"phase"`
	CreatedUnix     int64                   `json:"created_unix"`
	StartedUnix     int64                   `json:"started_unix"`
	DeadlineUnix    int64                   `json:"deadline_unix"`
	TaskSnapshot    SnapshotRef             `json:"task_snapshot"`
	PolicySnapshot  SnapshotRef             `json:"policy_snapshot"`
	EffectivePolicy config.RunPolicy        `json:"effective_policy"`
	FS              FSResult                `json:"fs"`
	Base            string                  `json:"base"`
	BaseCommit      string                  `json:"base_commit"`
	Counters        Counters                `json:"counters"`
	Assignment      *Ref                    `json:"assignment,omitempty"`
	Gate            *Ref                    `json:"gate,omitempty"`
	AcceptedTurns   map[string]AcceptedTurn `json:"accepted_turns"`
	PendingTxnID    string                  `json:"pending_txn_id"`
	Recovery        *Projection             `json:"recovery,omitempty"`
	Failure         *Projection             `json:"failure,omitempty"`
}

// Store is the run-state store over a genstore.
type Store struct {
	gs *genstore.Store
}

// Open returns a state store handle (side-effect-free).
func Open(dir, lockPath string) *Store {
	return &Store{gs: genstore.Open(dir, lockPath)}
}

// LockPath is the mutation lock guarding this store.
func (s *Store) LockPath() string { return s.gs.LockPath() }

// Load returns the current run state, strictly decoded and fully validated.
// (_, false, nil) means no state exists yet.
func (s *Store) Load() (RunState, bool, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return RunState{}, false, err
	}
	if !ok {
		return RunState{}, false, nil
	}
	rs, err := decodeRunState(rec)
	if err != nil {
		return RunState{}, false, err
	}
	return rs, true, nil
}

// Mutate applies fn as a compare-and-swap against expectedRevision, appending the
// next generation. It acquires and releases the guard itself; use MutateLocked to
// compose with other stores under one guard.
func (s *Store) Mutate(expectedRevision uint64, fn func(nextRevision uint64, next *RunState) error) (RunState, error) {
	g, ok, err := genstore.Acquire(s.gs.LockPath())
	if err != nil {
		return RunState{}, err
	}
	if !ok {
		return RunState{}, genstore.ErrBusy
	}
	rs, merr := s.MutateLocked(g, expectedRevision, fn)
	if rerr := g.Release(); merr == nil && rerr != nil {
		return rs, &genstore.PostCommitError{Generation: rs.Revision, Err: rerr}
	}
	return rs, merr
}

// MutateLocked appends the next generation under an already-held guard. fn runs
// inside the builder with the resulting revision, so identities/receipts bind to
// the real generation even across a quarantined-slot gap.
func (s *Store) MutateLocked(g *genstore.Guard, expectedRevision uint64, fn func(nextRevision uint64, next *RunState) error) (RunState, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return RunState{}, err
	}

	var prev *RunState
	var head genstore.Head
	if ok {
		p, derr := decodeRunState(rec)
		if derr != nil {
			return RunState{}, derr
		}
		if p.Revision != expectedRevision {
			return RunState{}, fmt.Errorf("%w: expected %d, have %d", ErrRevisionConflict, expectedRevision, p.Revision)
		}
		prev = &p
		head = rec.Head()
	} else if expectedRevision != 0 {
		return RunState{}, fmt.Errorf("%w: expected %d on an empty store", ErrRevisionConflict, expectedRevision)
	}

	built, err := s.gs.AppendLocked(g, head, func(gen uint64, _ string) ([]byte, error) {
		next := cloneForNext(prev)
		next.Revision = gen
		next.SchemaVersion = RunStateVersion

		if err := fn(gen, next); err != nil {
			return nil, err
		}
		// Schema-aware redaction: redact free text, reject secrets in control
		// fields. Then validate the new state and the transition from prev.
		if err := redactAndGuard(next); err != nil {
			return nil, err
		}
		if err := validate(next); err != nil {
			return nil, err
		}
		if prev == nil {
			if err := validateInit(next); err != nil {
				return nil, err
			}
		} else if err := validateTransition(prev, next); err != nil {
			return nil, err
		}
		return json.Marshal(next)
	})
	if err != nil {
		return RunState{}, err
	}
	// Return the authoritative state decoded from the exact persisted bytes.
	return decodeRunState(built)
}

// cloneForNext deep-copies prev (or returns a normalized fresh state) so the
// mutator always sees usable collections and transition validation can compare
// old vs new without aliasing.
func cloneForNext(prev *RunState) *RunState {
	if prev == nil {
		return &RunState{AcceptedTurns: map[string]AcceptedTurn{}, Counters: Counters{StepFixes: []int{}}}
	}
	n := *prev
	n.Counters.StepFixes = append([]int(nil), prev.Counters.StepFixes...)
	n.AcceptedTurns = make(map[string]AcceptedTurn, len(prev.AcceptedTurns))
	for k, v := range prev.AcceptedTurns {
		n.AcceptedTurns[k] = v
	}
	if prev.Assignment != nil {
		a := *prev.Assignment
		n.Assignment = &a
	}
	if prev.Gate != nil {
		g := *prev.Gate
		n.Gate = &g
	}
	if prev.Recovery != nil {
		r := *prev.Recovery
		n.Recovery = &r
	}
	if prev.Failure != nil {
		f := *prev.Failure
		n.Failure = &f
	}
	return &n
}

// decodeRunState strictly decodes and fully validates a persisted record.
func decodeRunState(rec genstore.Record) (RunState, error) {
	rs, err := strictDecodeRunState(rec.Payload)
	if err != nil {
		return RunState{}, fmt.Errorf("state: decode generation %d: %w", rec.Generation, err)
	}
	normalize(&rs)
	if rs.Revision != rec.Generation {
		return RunState{}, fmt.Errorf("state: revision %d disagrees with generation %d", rs.Revision, rec.Generation)
	}
	if err := validate(&rs); err != nil {
		return RunState{}, fmt.Errorf("state: generation %d invalid: %w", rec.Generation, err)
	}
	return rs, nil
}

// normalize ensures nil-vs-empty collections do not create accidental variants.
func normalize(rs *RunState) {
	if rs.AcceptedTurns == nil {
		rs.AcceptedTurns = map[string]AcceptedTurn{}
	}
	if rs.Counters.StepFixes == nil {
		rs.Counters.StepFixes = []int{}
	}
}
