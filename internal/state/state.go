// Package state is the coordinator's typed run state, persisted as immutable
// generations (D017) via internal/genstore. State never overwrites in place; each
// mutation is a compare-and-swap that appends the next generation, so the
// generation number, the record, and the payload's Revision always agree.
//
// State holds only typed identities, digests, and relative references — never the
// protocol artifacts themselves (those arrive with subject 02). Every payload is
// redacted before it is written, and the durable digest is over the redacted
// bytes, so a secret never lands in state and two mutations differing only in a
// redacted value collapse identically.
package state

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/redact"
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

// Phase is the current phase of the pairing loop.
type Phase string

// SnapshotRef points at a hashed, copied input inside the run directory (relative
// path + exact digest of the bytes), never the live source.
type SnapshotRef struct {
	RelPath string `json:"rel_path"`
	Digest  string `json:"digest"`
}

// FSResult is the frozen filesystem classification and any operator ack.
type FSResult struct {
	Class        string `json:"class"`
	Reason       string `json:"reason"`
	Acknowledged bool   `json:"acknowledged"`
}

// Counters are the lead-response budgets consumed so far. They only increase.
// Per-step checkpoint fixes are indexed by implementation step (typed, not a
// free-string map).
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

// AcceptedTurn records what was accepted for a turn. Keyed by turn_id so a repeat
// submit with the same turn_id but a different digest is a detectable conflict.
type AcceptedTurn struct {
	ArtifactDigest string  `json:"artifact_digest"`
	Receipt        Receipt `json:"receipt"`
}

// Projection is a typed recovery or failure summary.
type Projection struct {
	Code       string `json:"code"`
	Reason     string `json:"reason"`
	NextAction string `json:"next_action"`
	AtRevision uint64 `json:"at_revision"`
}

// RunState is the authoritative typed state of a run.
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
	Assignment      Ref                     `json:"assignment"`
	Gate            Ref                     `json:"gate"`
	AcceptedTurns   map[string]AcceptedTurn `json:"accepted_turns"`
	PendingTxnID    string                  `json:"pending_txn_id"`
	Recovery        Projection              `json:"recovery"`
	Failure         Projection              `json:"failure"`
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

// Load returns the current run state. (_, false, nil) means no state exists yet.
func (s *Store) Load() (RunState, bool, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return RunState{}, false, err
	}
	if !ok {
		return RunState{}, false, nil
	}
	rs, err := unmarshal(rec)
	if err != nil {
		return RunState{}, false, err
	}
	return rs, true, nil
}

// Mutate applies fn to the current state as a compare-and-swap against
// expectedRevision, appending the next generation. It acquires and releases the
// guard itself; use MutateLocked to compose with other stores under one guard.
func (s *Store) Mutate(expectedRevision uint64, fn func(*RunState) error) (RunState, error) {
	g, ok, err := genstore.Acquire(s.gs.LockPath())
	if err != nil {
		return RunState{}, err
	}
	if !ok {
		return RunState{}, genstore.ErrBusy
	}
	rs, merr := s.MutateLocked(g, expectedRevision, fn)
	rerr := g.Release()
	if merr == nil && rerr != nil {
		return rs, fmt.Errorf("state generation %d committed but lock release failed: %w", rs.Revision, rerr)
	}
	return rs, merr
}

// MutateLocked is the composable form: it appends under an already-held guard.
func (s *Store) MutateLocked(g *genstore.Guard, expectedRevision uint64, fn func(*RunState) error) (RunState, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return RunState{}, err
	}

	var cur RunState
	var head genstore.Head
	if ok {
		cur, err = unmarshal(rec)
		if err != nil {
			return RunState{}, err
		}
		if cur.Revision != expectedRevision {
			return RunState{}, fmt.Errorf("%w: expected %d, have %d", ErrRevisionConflict, expectedRevision, cur.Revision)
		}
		head = rec.Head()
	} else if expectedRevision != 0 {
		return RunState{}, fmt.Errorf("%w: expected %d on an empty store", ErrRevisionConflict, expectedRevision)
	}

	next := cur
	if err := fn(&next); err != nil {
		return RunState{}, err
	}

	built, err := s.gs.AppendLocked(g, head, func(gen uint64, _ string) ([]byte, error) {
		next.Revision = gen
		next.SchemaVersion = RunStateVersion
		raw, merr := json.Marshal(next)
		if merr != nil {
			return nil, merr
		}
		// Redact before persisting; the digest is taken over the redacted bytes.
		return redact.Bytes(raw), nil
	})
	if err != nil {
		return RunState{}, err
	}
	next.Revision = built.Generation
	return next, nil
}

func unmarshal(rec genstore.Record) (RunState, error) {
	var rs RunState
	if err := json.Unmarshal(rec.Payload, &rs); err != nil {
		return RunState{}, fmt.Errorf("state: decode generation %d: %w", rec.Generation, err)
	}
	if rs.SchemaVersion != RunStateVersion {
		return RunState{}, fmt.Errorf("state: unsupported schema_version %d (want %d)", rs.SchemaVersion, RunStateVersion)
	}
	if rs.Revision != rec.Generation {
		return RunState{}, fmt.Errorf("state: revision %d disagrees with generation %d", rs.Revision, rec.Generation)
	}
	return rs, nil
}
