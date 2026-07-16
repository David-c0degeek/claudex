package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/redact"
	"github.com/David-c0degeek/claudex/internal/state"
)

var (
	// ErrNoRun means the store holds no run state to submit against.
	ErrNoRun = errors.New("transport: no run to submit to")
	// ErrWrongTurn means the submission's turn is not the active turn at the
	// current revision.
	ErrWrongTurn = errors.New("transport: submission is not for the active turn")
	// ErrConflict means a different artifact was already accepted for this turn.
	ErrConflict = errors.New("transport: a different artifact was already accepted for this turn")
	// ErrDecisionInconsistent means requires_human_decision and decision_question
	// disagree.
	ErrDecisionInconsistent = errors.New("transport: requires_human_decision and decision_question are inconsistent")
	// ErrMissingSeam means a required injected dependency was nil.
	ErrMissingSeam = errors.New("transport: submit requires a sink, an authorizer, and a transition")
	// ErrTransitionInvalid means the transition left the consumed turn in place, so
	// accepting it would strand the run.
	ErrTransitionInvalid = errors.New("transport: transition did not consume the turn")
)

const (
	submitMaxAttempts = 64
	submitBackoff     = 200 * time.Microsecond
)

// StaleError reports that a submission was prepared against a superseded state.
type StaleError struct {
	SubmittedRevision uint64
	CurrentRevision   uint64
	Lifecycle         state.Lifecycle
	Phase             state.Phase
	CurrentTurnID     string
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("transport: submission is stale (submitted for revision %d; run is at revision %d, lifecycle %s, phase %s, turn %q) — re-pull the current assignment",
		e.SubmittedRevision, e.CurrentRevision, e.Lifecycle, e.Phase, e.CurrentTurnID)
}

// ArtifactSink stores the immutable, canonical, redacted artifact bytes before
// acceptance is recorded, so a crash can never leave an accepted receipt with no
// artifact. It is idempotent by (turnID, digest) and rejects a same-key write
// whose bytes differ. Put must not retain the caller's slice.
type ArtifactSink interface {
	Put(turnID, digest string, canonical []byte) error
}

// Authorizer decides whether a session may submit for a turn, from durable
// registration/ownership history. It guards every submit, including an
// idempotent replay, and does not depend on the current phase.
type Authorizer func(rs state.RunState, sessionID, turnID string) error

// PreparedSubmit is the immutable, validated submission handed to the transition
// so the engine can evaluate the artifact without a side channel.
type PreparedSubmit struct {
	MessageType           string
	TurnID                string
	Revision              uint64
	Digest                string
	Canonical             []byte // a defensive copy; safe to read
	RequiresHumanDecision bool
	DecisionQuestion      string
}

// Transition advances phase/counters/gates and clears or issues the next
// assignment inside the same state CAS that records acceptance. It MUST be pure,
// deterministic, and side-effect-free: it may run for a mutation that later
// fails, and submit installs the accepted-turn receipt after it runs.
type Transition func(prepared PreparedSubmit, nextRevision uint64, next *state.RunState) error

// SubmitResult is the outcome of an accepted (or idempotently replayed) submit.
type SubmitResult struct {
	Receipt    state.Receipt
	Idempotent bool
	// ReleaseWarning is non-nil when the submit committed but the lock release
	// failed; the acceptance is durable and the receipt is authoritative.
	ReleaseWarning error
}

type submitEnvelope struct {
	TurnID                string  `json:"turn_id"`
	StateRevision         uint64  `json:"state_revision"`
	RequiresHumanDecision bool    `json:"requires_human_decision"`
	DecisionQuestion      *string `json:"decision_question"`
}

// Submit validates an artifact against the authoritative assignment reconstructed
// from durable state, redacts it, persists the immutable artifact, then records
// accept-once acceptance and the caller's transition in a single state CAS.
func Submit(ctx context.Context, store *state.Store, sink ArtifactSink, sessionID string, raw []byte, authorize Authorizer, advance Transition) (SubmitResult, error) {
	if sink == nil || authorize == nil || advance == nil {
		return SubmitResult{}, ErrMissingSeam
	}

	canonRaw, err := canonjson.Canonicalize(raw)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("transport: submission is not valid canonical JSON: %w", err)
	}
	canonRedacted, err := canonjson.Canonicalize(redact.Bytes(canonRaw))
	if err != nil {
		return SubmitResult{}, fmt.Errorf("transport: redacted submission is not valid canonical JSON: %w", err)
	}
	sum := sha256.Sum256(canonRedacted)
	digest := hex.EncodeToString(sum[:])
	var env submitEnvelope
	if err := json.Unmarshal(canonRedacted, &env); err != nil {
		return SubmitResult{}, fmt.Errorf("transport: submission has no readable envelope: %w", err)
	}

	for attempt := 0; attempt < submitMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return SubmitResult{}, err
		}
		rs, ok, err := store.Load()
		if err != nil {
			return SubmitResult{}, err
		}
		if !ok {
			return SubmitResult{}, ErrNoRun
		}

		// Session ownership guards every submit, including replay, before any
		// artifact write or state read of the accepted-turn table.
		if err := authorize(rs, sessionID, env.TurnID); err != nil {
			return SubmitResult{}, err
		}

		if at, seen := rs.AcceptedTurns[env.TurnID]; seen {
			if at.ArtifactDigest == digest {
				if err := sink.Put(env.TurnID, digest, canonRedacted); err != nil {
					return SubmitResult{}, err
				}
				return SubmitResult{Receipt: at.Receipt, Idempotent: true}, nil
			}
			return SubmitResult{}, ErrConflict
		}

		// A superseded revision is stale regardless of the current turn; only at
		// the same revision does the turn identity matter.
		if env.StateRevision != rs.Revision {
			return SubmitResult{}, staleError(env.StateRevision, rs)
		}
		if rs.Assignment == nil || rs.Assignment.ID == "" {
			return SubmitResult{}, ErrNoActiveTurn
		}
		if env.TurnID != rs.Assignment.ID {
			return SubmitResult{}, ErrWrongTurn
		}
		spec, ok := TurnSpec(rs.Phase)
		if !ok {
			return SubmitResult{}, fmt.Errorf("%w: %s", ErrPhaseNotActionable, rs.Phase)
		}
		if _, err := protocol.Validate(spec.ArtifactMessageType, raw); err != nil {
			return SubmitResult{}, fmt.Errorf("transport: submission fails its schema: %w", err)
		}
		if _, err := protocol.Validate(spec.ArtifactMessageType, canonRedacted); err != nil {
			return SubmitResult{}, fmt.Errorf("transport: redacted submission fails its schema: %w", err)
		}
		if err := checkDecision(env); err != nil {
			return SubmitResult{}, err
		}

		if err := sink.Put(env.TurnID, digest, canonRedacted); err != nil {
			return SubmitResult{}, err
		}

		prepared := PreparedSubmit{
			MessageType:           spec.ArtifactMessageType,
			TurnID:                env.TurnID,
			Revision:              rs.Revision,
			Digest:                digest,
			Canonical:             append([]byte(nil), canonRedacted...),
			RequiresHumanDecision: env.RequiresHumanDecision,
			DecisionQuestion:      deref(env.DecisionQuestion),
		}
		var receipt state.Receipt
		committed, merr := store.Mutate(rs.Revision, func(gen uint64, next *state.RunState) error {
			// The transition runs first and may clear/reissue the turn; acceptance
			// is installed last so the callback cannot erase or replace it.
			if err := advance(prepared, gen, next); err != nil {
				return err
			}
			if err := requireTurnConsumed(next, env.TurnID, gen); err != nil {
				return err
			}
			receipt = state.Receipt{TurnID: env.TurnID, Revision: gen, ArtifactDigest: digest}
			next.AcceptedTurns[env.TurnID] = state.AcceptedTurn{ArtifactDigest: digest, Receipt: receipt}
			return nil
		})
		if merr == nil {
			return SubmitResult{Receipt: receipt, Idempotent: false}, nil
		}
		res, retry, cerr := classifyMutateOutcome(committed, merr, env.TurnID, digest)
		if cerr != nil {
			return SubmitResult{}, cerr
		}
		if !retry {
			return res, nil // committed despite a release failure — reconciled
		}
		select {
		case <-ctx.Done():
			return SubmitResult{}, ctx.Err()
		case <-time.After(submitBackoff):
		}
	}
	// Exhausted retries without ever committing: the lock stayed busy. This is not
	// staleness — no revision necessarily changed.
	return SubmitResult{}, fmt.Errorf("transport: submit did not acquire the run lock after %d attempts: %w", submitMaxAttempts, genstore.ErrBusy)
}

// requireTurnConsumed enforces that the transition consumed the turn: the next
// assignment is cleared, or is a different turn freshly issued at this revision.
// A no-op leaves the original stale ref, which would strand the run.
func requireTurnConsumed(next *state.RunState, turnID string, gen uint64) error {
	if next.Assignment == nil {
		return nil
	}
	if next.Assignment.ID == turnID {
		return fmt.Errorf("%w: the consumed turn is still assigned", ErrTransitionInvalid)
	}
	if next.Assignment.IssuedRevision != gen {
		return fmt.Errorf("%w: the next assignment was not issued at this revision", ErrTransitionInvalid)
	}
	return nil
}

// classifyMutateOutcome interprets a store.Mutate result. A committed
// PostCommitError is a successful acceptance whose lock release failed: reconcile
// the receipt from the committed state and surface the release problem. A
// revision conflict or busy signals a lost race (retry). Any other error is fatal.
func classifyMutateOutcome(committed state.RunState, merr error, turnID, digest string) (res SubmitResult, retry bool, err error) {
	var pce *genstore.PostCommitError
	if errors.As(merr, &pce) {
		at, ok := committed.AcceptedTurns[turnID]
		if !ok || at.ArtifactDigest != digest {
			return SubmitResult{}, false, fmt.Errorf("transport: committed submit could not be reconciled: %w", merr)
		}
		return SubmitResult{Receipt: at.Receipt, Idempotent: false, ReleaseWarning: merr}, false, nil
	}
	if errors.Is(merr, state.ErrRevisionConflict) || errors.Is(merr, genstore.ErrBusy) {
		return SubmitResult{}, true, nil
	}
	return SubmitResult{}, false, merr
}

func staleError(submitted uint64, rs state.RunState) *StaleError {
	turn := ""
	if rs.Assignment != nil {
		turn = rs.Assignment.ID
	}
	return &StaleError{
		SubmittedRevision: submitted,
		CurrentRevision:   rs.Revision,
		Lifecycle:         rs.Lifecycle,
		Phase:             rs.Phase,
		CurrentTurnID:     turn,
	}
}

// checkDecision enforces the envelope's human-decision XOR: a decision is
// required exactly when a non-empty (trimmed) question is present; false requires
// a JSON null question, and an empty-string question is never a decision.
func checkDecision(env submitEnvelope) error {
	hasQuestion := env.DecisionQuestion != nil && strings.TrimSpace(*env.DecisionQuestion) != ""
	if env.RequiresHumanDecision != hasQuestion {
		return ErrDecisionInconsistent
	}
	if !env.RequiresHumanDecision && env.DecisionQuestion != nil {
		return ErrDecisionInconsistent
	}
	return nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
