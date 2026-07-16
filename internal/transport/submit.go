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
	// ErrTransitionInvalid means the transition left the run in a stranded shape
	// (an un-consumed turn, or a phase/assignment mismatch).
	ErrTransitionInvalid = errors.New("transport: transition left the run stranded")
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
// registration/ownership history. It MUST be pure and side-effect-free: it
// receives a state value for reference only, and submit captures every authority
// fact before calling it, so mutating that value cannot influence the outcome.
type Authorizer func(rs state.RunState, sessionID, turnID string) error

// PreparedSubmit is the immutable, validated submission handed to the transition
// so the engine can evaluate the artifact without a side channel. CanonicalJSON
// is a string precisely so a callback cannot mutate the authoritative bytes.
type PreparedSubmit struct {
	MessageType           string
	TurnID                string
	Revision              uint64
	Digest                string
	CanonicalJSON         string
	RequiresHumanDecision bool
	DecisionQuestion      string
}

// Transition advances phase/counters/gates and clears or issues the next
// assignment inside the same state CAS that records acceptance. It MUST be pure,
// deterministic, and side-effect-free, and MUST return value-free errors (no
// submitted free text): it may run for a mutation that later fails, and submit
// installs the accepted-turn receipt after it runs.
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

// statusSnapshot is the authority captured from a load before the authorizer
// runs, so a mutation of the shared state value cannot change classification.
type statusSnapshot struct {
	revision     uint64
	lifecycle    state.Lifecycle
	phase        state.Phase
	assignedTurn string
	assignedRev  uint64
}

func (s statusSnapshot) staleError(submitted uint64) *StaleError {
	return &StaleError{
		SubmittedRevision: submitted,
		CurrentRevision:   s.revision,
		Lifecycle:         s.lifecycle,
		Phase:             s.phase,
		CurrentTurnID:     s.assignedTurn,
	}
}

// Submit validates an artifact against the authoritative assignment reconstructed
// from durable state, redacts it, persists the immutable artifact, then records
// accept-once acceptance and the caller's transition in a single state CAS.
func Submit(ctx context.Context, store *state.Store, sink ArtifactSink, sessionID string, raw []byte, authorize Authorizer, advance Transition) (SubmitResult, error) {
	if sink == nil || authorize == nil || advance == nil {
		return SubmitResult{}, ErrMissingSeam
	}

	// Errors derived from parsing the RAW artifact are redacted: canonjson
	// diagnostics can echo a duplicate key or number token, which could be a
	// secret crossing the display boundary.
	canonRaw, err := canonjson.Canonicalize(raw)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("transport: submission is not valid canonical JSON: %s", redact.Text(err.Error()))
	}
	canonRedacted, err := canonjson.Canonicalize(redact.Bytes(canonRaw))
	if err != nil {
		return SubmitResult{}, fmt.Errorf("transport: redacted submission is not valid canonical JSON: %s", redact.Text(err.Error()))
	}
	sum := sha256.Sum256(canonRedacted)
	digest := hex.EncodeToString(sum[:])
	var env submitEnvelope
	if err := json.Unmarshal(canonRedacted, &env); err != nil {
		return SubmitResult{}, fmt.Errorf("transport: submission has no readable envelope: %s", redact.Text(err.Error()))
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

		// Capture every authority fact before the authorizer runs, so it cannot
		// influence classification by mutating the shared state value it receives.
		acceptedEntry, acceptedSeen := rs.AcceptedTurns[env.TurnID]
		snap := statusSnapshot{revision: rs.Revision, lifecycle: rs.Lifecycle, phase: rs.Phase}
		if rs.Assignment != nil {
			snap.assignedTurn, snap.assignedRev = rs.Assignment.ID, rs.Assignment.IssuedRevision
		}

		if err := authorize(rs, sessionID, env.TurnID); err != nil {
			return SubmitResult{}, err
		}

		if acceptedSeen {
			if acceptedEntry.ArtifactDigest == digest {
				if err := sink.Put(env.TurnID, digest, canonRedacted); err != nil {
					return SubmitResult{}, err
				}
				return SubmitResult{Receipt: acceptedEntry.Receipt, Idempotent: true}, nil
			}
			return SubmitResult{}, ErrConflict
		}

		if env.StateRevision != snap.revision {
			return SubmitResult{}, snap.staleError(env.StateRevision)
		}
		if snap.assignedTurn == "" {
			return SubmitResult{}, ErrNoActiveTurn
		}
		if env.TurnID != snap.assignedTurn {
			return SubmitResult{}, ErrWrongTurn
		}
		spec, ok := TurnSpec(snap.phase)
		if !ok {
			return SubmitResult{}, fmt.Errorf("%w: %s", ErrPhaseNotActionable, snap.phase)
		}
		// protocol.Validate is value-free by contract, so its errors need no
		// redaction. Validate the original raw first, then the redacted bytes.
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
			Revision:              snap.revision,
			Digest:                digest,
			CanonicalJSON:         string(canonRedacted),
			RequiresHumanDecision: env.RequiresHumanDecision,
			DecisionQuestion:      deref(env.DecisionQuestion),
		}
		var receipt state.Receipt
		committed, merr := store.Mutate(snap.revision, func(gen uint64, next *state.RunState) error {
			if err := advance(prepared, gen, next); err != nil {
				return err
			}
			if err := requireLiveOwner(next, env.TurnID, gen); err != nil {
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
			return res, nil
		}
		select {
		case <-ctx.Done():
			return SubmitResult{}, ctx.Err()
		case <-time.After(submitBackoff):
		}
	}
	return SubmitResult{}, fmt.Errorf("transport: submit did not acquire the run lock after %d attempts: %w", submitMaxAttempts, genstore.ErrBusy)
}

// requireLiveOwner enforces that the transition left the run with a real next
// owner, so accepting the submit can never strand it. The engine owns the exact
// legal transition; this is the safety postcondition it must satisfy:
//
//   - an assignment present  -> an agent phase (has a TurnSpec), lifecycle
//     RUNNING, and a different turn freshly issued at this revision;
//   - an assignment absent    -> a terminal/failure lifecycle (a failure carries
//     its failure projection), or a RUNNING mechanical TESTS gate, or an
//     AWAIT_GUIDANCE human gate that is paused with a gate issued at this
//     revision.
//
// Every other shape — INIT with no assignment, an actionable phase with no
// assignment, an assignment under a terminal/paused lifecycle — is ownerless and
// rejected.
func requireLiveOwner(next *state.RunState, turnID string, gen uint64) error {
	lc := next.Lifecycle
	_, agentPhase := TurnSpec(next.Phase)

	if next.Assignment != nil {
		if !agentPhase {
			return fmt.Errorf("%w: an assignment is set but phase %s is not an agent phase", ErrTransitionInvalid, next.Phase)
		}
		if lc != state.LifecycleRunning {
			return fmt.Errorf("%w: an agent turn is assigned but lifecycle is %s", ErrTransitionInvalid, lc)
		}
		if next.Assignment.ID == turnID {
			return fmt.Errorf("%w: the consumed turn is still assigned", ErrTransitionInvalid)
		}
		if next.Assignment.IssuedRevision != gen {
			return fmt.Errorf("%w: the next assignment was not issued at this revision", ErrTransitionInvalid)
		}
		return nil
	}

	if agentPhase {
		return fmt.Errorf("%w: the assignment was cleared but phase %s still expects an agent turn", ErrTransitionInvalid, next.Phase)
	}
	switch {
	case state.IsTerminalLifecycle(lc):
		if state.IsFailureLifecycle(lc) && next.Failure == nil {
			return fmt.Errorf("%w: failure lifecycle %s without a failure projection", ErrTransitionInvalid, lc)
		}
		return nil
	case next.Phase == state.PhaseTests:
		if lc != state.LifecycleRunning {
			return fmt.Errorf("%w: the TESTS gate must be running, got %s", ErrTransitionInvalid, lc)
		}
		return nil
	case next.Phase == state.PhaseAwaitGuidance:
		if lc != state.LifecyclePaused {
			return fmt.Errorf("%w: a human gate must pause the run, got lifecycle %s", ErrTransitionInvalid, lc)
		}
		if next.Gate == nil || next.Gate.ID == "" || next.Gate.IssuedRevision != gen {
			return fmt.Errorf("%w: a human gate needs a gate issued at this revision", ErrTransitionInvalid)
		}
		return nil
	default:
		return fmt.Errorf("%w: ownerless run shape (phase %s, lifecycle %s)", ErrTransitionInvalid, next.Phase, lc)
	}
}

// classifyMutateOutcome interprets a store.Mutate result. A committed
// PostCommitError is a successful acceptance whose lock release failed: reconcile
// the receipt from the committed state. A revision conflict or busy signals a
// lost race (retry). Any other error is fatal.
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
