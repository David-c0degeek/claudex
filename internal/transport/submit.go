package transport

import (
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
	// ErrWrongTurn means the submission's turn is not the active turn.
	ErrWrongTurn = errors.New("transport: submission is not for the active turn")
	// ErrConflict means a different artifact was already accepted for this turn.
	ErrConflict = errors.New("transport: a different artifact was already accepted for this turn")
	// ErrDecisionInconsistent means requires_human_decision and decision_question
	// disagree.
	ErrDecisionInconsistent = errors.New("transport: requires_human_decision and decision_question are inconsistent")
	// ErrMissingSeam means a required injected dependency was nil; there is no
	// production submit path that skips ownership, artifact persistence, or the
	// transition.
	ErrMissingSeam = errors.New("transport: submit requires a sink, an authorizer, and a transition")
)

const submitMaxAttempts = 64

// StaleError reports that a submission was prepared against a superseded state.
// It carries the current status so a caller can re-pull and retry.
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
// whose bytes differ (a digest collision).
type ArtifactSink interface {
	Put(turnID, digest string, canonical []byte) error
}

// Authorizer decides whether a session may submit for a turn. Session ownership
// is routing correctness; the registration layer supplies the real check.
type Authorizer func(rs state.RunState, sessionID, turnID string, spec TurnSpecEntry) error

// Transition advances phase/counters/gates and clears or issues the next
// assignment inside the same state CAS that records acceptance, so acceptance and
// the transition that issues the next turn are one mutation.
type Transition func(nextRevision uint64, next *state.RunState) error

// SubmitResult is the outcome of an accepted (or idempotently replayed) submit.
type SubmitResult struct {
	Receipt           state.Receipt
	CanonicalArtifact []byte
	Idempotent        bool
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
// Identity is the canonical digest of the redacted bytes, so two submissions
// differing only in a secret collapse idempotently.
func Submit(store *state.Store, sink ArtifactSink, sessionID string, raw []byte, authorize Authorizer, advance Transition) (SubmitResult, error) {
	if sink == nil || authorize == nil || advance == nil {
		return SubmitResult{}, ErrMissingSeam
	}

	// The digest is stable across attempts: redact, canonicalize, hash.
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
		rs, ok, err := store.Load()
		if err != nil {
			return SubmitResult{}, err
		}
		if !ok {
			return SubmitResult{}, ErrNoRun
		}

		// Idempotency/conflict is keyed by (turn_id, digest) and needs no schema
		// re-validation: an accepted turn was validated when it was accepted.
		if at, seen := rs.AcceptedTurns[env.TurnID]; seen {
			if at.ArtifactDigest == digest {
				if err := sink.Put(env.TurnID, digest, canonRedacted); err != nil {
					return SubmitResult{}, err
				}
				return SubmitResult{Receipt: at.Receipt, CanonicalArtifact: canonRedacted, Idempotent: true}, nil
			}
			return SubmitResult{}, ErrConflict
		}

		// A fresh submission must be for the current active turn and fully valid.
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
		if err := authorize(rs, sessionID, env.TurnID, spec); err != nil {
			return SubmitResult{}, err
		}
		// Validate the ORIGINAL raw against the schema first, so redaction cannot
		// launder an invalid contract; then validate the exact redacted bytes.
		if _, err := protocol.Validate(spec.ArtifactMessageType, raw); err != nil {
			return SubmitResult{}, fmt.Errorf("transport: submission fails its schema: %w", err)
		}
		if _, err := protocol.Validate(spec.ArtifactMessageType, canonRedacted); err != nil {
			return SubmitResult{}, fmt.Errorf("transport: redacted submission fails its schema: %w", err)
		}
		if env.StateRevision != rs.Revision {
			return SubmitResult{}, staleError(env.StateRevision, rs)
		}
		if err := checkDecision(env); err != nil {
			return SubmitResult{}, err
		}

		// Persist the immutable artifact BEFORE the state CAS. A crash here leaves
		// an unreferenced artifact (safe, reusable), never accepted state with no
		// bytes.
		if err := sink.Put(env.TurnID, digest, canonRedacted); err != nil {
			return SubmitResult{}, err
		}

		var receipt state.Receipt
		_, merr := store.Mutate(rs.Revision, func(gen uint64, next *state.RunState) error {
			receipt = state.Receipt{TurnID: env.TurnID, Revision: gen, ArtifactDigest: digest}
			next.AcceptedTurns[env.TurnID] = state.AcceptedTurn{ArtifactDigest: digest, Receipt: receipt}
			return advance(gen, next)
		})
		if merr == nil {
			return SubmitResult{Receipt: receipt, CanonicalArtifact: canonRedacted, Idempotent: false}, nil
		}
		// Lost the race for this revision: reload and reclassify (idempotent
		// duplicate / conflict / stale) at the top of the loop.
		if errors.Is(merr, state.ErrRevisionConflict) || errors.Is(merr, genstore.ErrBusy) {
			time.Sleep(200 * time.Microsecond)
			continue
		}
		return SubmitResult{}, merr
	}

	rs, _, _ := store.Load()
	return SubmitResult{}, staleError(env.StateRevision, rs)
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
// required exactly when a non-empty (trimmed) question is present; false
// requires a JSON null question, and an empty-string question is never a
// decision.
func checkDecision(env submitEnvelope) error {
	hasQuestion := env.DecisionQuestion != nil && strings.TrimSpace(*env.DecisionQuestion) != ""
	if env.RequiresHumanDecision != hasQuestion {
		return ErrDecisionInconsistent
	}
	if !env.RequiresHumanDecision && env.DecisionQuestion != nil {
		return ErrDecisionInconsistent // false requires JSON null, not an empty string
	}
	return nil
}
