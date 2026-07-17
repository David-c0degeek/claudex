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
	ErrMissingSeam = errors.New("transport: submit requires a store, registry, journal, sink, and prepare")
	// ErrLockMismatch means the submit dependencies do not all mutate under the one
	// run lock, so their reads could not be linearized.
	ErrLockMismatch = errors.New("transport: submit dependencies do not share the run lock")
	// ErrRunMismatch means the registry/run identity does not match the run state.
	ErrRunMismatch = errors.New("transport: registry does not belong to this run")
	// ErrUnauthorized means the session is not the current owner for the phase.
	ErrUnauthorized = errors.New("transport: session is not the current owner for this phase")
	// ErrRecoveryRequired means an ambiguous or unreadable attach journal must be
	// recovered before a submit can be authorized; no artifact is written.
	ErrRecoveryRequired = errors.New("transport: run requires recovery before a submit")
	// ErrTransitionInvalid means the transition left the run in a stranded shape or
	// issued an invalid identity.
	ErrTransitionInvalid = errors.New("transport: transition left the run stranded")
	// ErrNotAccepting means the run is not in a state that can accept a submit.
	ErrNotAccepting = errors.New("transport: run is not accepting submissions")
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
// artifact. It is idempotent by (turnID, digest) and rejects a same-key write whose
// bytes differ. Put must not retain the caller's slice.
type ArtifactSink interface {
	Put(turnID, digest string, canonical []byte) error
}

// Authorizer is an OPTIONAL cheap pre-guard rejection over a lock-free load. It is
// never definitive: the authoritative current-session authorization runs under the
// run guard against the locked Registry.
type Authorizer func(rs state.RunState, sessionID, turnID string) error

// JournalHead is the attach journal's head status observed under the run guard.
type JournalHead int

const (
	// JournalTerminal is a valid completed/aborted record bound to the run.
	JournalTerminal JournalHead = iota
	// JournalNonterminal is a pending record: the run is mid-attach.
	JournalNonterminal
	// JournalAbsent means no journal generation exists — never an open/decode failure.
	JournalAbsent
)

// JournalReader reports the run's attach-journal head under the held run guard, so
// a submit never authorizes through a mid-attach Registry. The concrete reader
// verifies its store mutates under the run lock and binds a terminal record to the
// requested run id; any unreadable/corrupt/mismatched record is a read error (the
// caller fails closed), never Absent.
type JournalReader interface {
	LockPath() string
	Head(g *genstore.Guard, runID string) (JournalHead, error)
}

// PreparedSubmit is the immutable, validated submission handed to Prepare so the
// engine can evaluate the artifact without a side channel. Its facts come only from
// the locked snapshot. CanonicalJSON is a string so a callback cannot mutate it.
type PreparedSubmit struct {
	MessageType           string
	TurnID                string
	Revision              uint64
	Digest                string
	CanonicalJSON         string
	RequiresHumanDecision bool
	DecisionQuestion      string
}

// PreparedTransition is the deterministic, value-only transition Prepare produced
// from a deep snapshot. It exposes the identities it will issue so transport can
// recheck them under the guard before publishing, and applies exactly once inside
// the state CAS. Build it with NewPreparedTransition; its apply must not perform
// I/O or retain external state.
type PreparedTransition struct {
	issuedTurnID string
	issuedGateID string
	apply        func(gen uint64, next *state.RunState) error
}

// NewPreparedTransition constructs a PreparedTransition. issuedTurnID is the next
// assignment id a running edge issues (else ""); issuedGateID is the gate id a gate
// edge issues (else ""); a terminal edge issues neither.
func NewPreparedTransition(issuedTurnID, issuedGateID string, apply func(gen uint64, next *state.RunState) error) PreparedTransition {
	return PreparedTransition{issuedTurnID: issuedTurnID, issuedGateID: issuedGateID, apply: apply}
}

// IssuedTurnID / IssuedGateID expose the identities the transition will issue.
func (p PreparedTransition) IssuedTurnID() string { return p.issuedTurnID }
func (p PreparedTransition) IssuedGateID() string { return p.issuedGateID }

// Prepare authorizes-independent semantic preparation: from a deep clone of the
// locked run state and the prepared submit it returns the deterministic transition
// to apply, or a value-free error that rejects the submit before any sink write. It
// must not mutate its snapshot in a way that affects the committed state (it cannot:
// the snapshot is a copy), perform I/O, or mint identities under the guard.
type Prepare func(snapshot state.RunState, prepared PreparedSubmit) (PreparedTransition, error)

// SubmitDeps are the injected, lock-sharing dependencies of a submit.
type SubmitDeps struct {
	Store     *state.Store
	Registry  *state.RegistryStore
	Journal   JournalReader
	Sink      ArtifactSink
	Prepare   Prepare
	Preflight Authorizer // optional
}

func (d SubmitDeps) validate() error {
	if d.Store == nil || d.Registry == nil || d.Journal == nil || d.Sink == nil || d.Prepare == nil {
		return ErrMissingSeam
	}
	lp := d.Store.LockPath()
	if d.Registry.LockPath() != lp || d.Journal.LockPath() != lp {
		return ErrLockMismatch
	}
	return nil
}

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

// Submit validates an artifact against the durable assignment, authorizes the
// session against the locked Registry, publishes the immutable artifact, then
// records accept-once acceptance and the prepared transition — all under one held
// run guard so authorization, publish, and acceptance linearize against a
// concurrent session replacement.
func Submit(ctx context.Context, deps SubmitDeps, sessionID string, raw []byte) (SubmitResult, error) {
	if err := deps.validate(); err != nil {
		return SubmitResult{}, err
	}

	// Read-only prep (no guard). Errors from parsing the RAW artifact are redacted:
	// canonjson diagnostics can echo a token that could be a secret.
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

	// Optional cheap pre-guard rejection over a lock-free load; never definitive.
	if deps.Preflight != nil {
		if rs, ok, lerr := deps.Store.Load(); lerr == nil && ok {
			if perr := deps.Preflight(rs, sessionID, env.TurnID); perr != nil {
				return SubmitResult{}, perr
			}
		}
	}

	for attempt := 0; attempt < submitMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return SubmitResult{}, err
		}
		g, ok, aerr := genstore.Acquire(deps.Store.LockPath())
		if aerr != nil {
			return SubmitResult{}, aerr
		}
		if !ok {
			time.Sleep(submitBackoff)
			continue
		}
		return lockedSubmit(deps, g, sessionID, raw, canonRedacted, digest, env)
	}
	return SubmitResult{}, fmt.Errorf("transport: submit did not acquire the run lock after %d attempts: %w", submitMaxAttempts, genstore.ErrBusy)
}

// lockedSubmit runs the whole authorize -> publish -> accept sequence under the held
// guard g, which it releases exactly once on every path.
func lockedSubmit(deps SubmitDeps, g *genstore.Guard, sessionID string, raw, canonRedacted []byte, digest string, env submitEnvelope) (SubmitResult, error) {
	released := false
	release := func() error {
		if released {
			return nil
		}
		released = true
		return g.Release()
	}
	defer release() // safety net for reject paths; a no-op once released
	reject := func(opErr error) (SubmitResult, error) {
		return SubmitResult{}, errors.Join(opErr, releaseOutcome(false, 0, release()))
	}

	rs, ok, lerr := deps.Store.Load()
	if lerr != nil {
		return reject(lerr)
	}
	if !ok {
		return reject(ErrNoRun)
	}
	reg, ok, rerr := deps.Registry.Load()
	if rerr != nil {
		return reject(rerr)
	}
	if !ok {
		return reject(ErrRunMismatch)
	}

	// Attach-journal policy: fail closed on a nonterminal or unreadable journal.
	head, jerr := deps.Journal.Head(g, rs.RunID)
	if jerr != nil {
		return reject(fmt.Errorf("%w: %v", ErrRecoveryRequired, jerr))
	}
	if head == JournalNonterminal {
		return reject(ErrRecoveryRequired)
	}
	// Run identity: the registry must belong to this run.
	if reg.RunID != rs.RunID {
		return reject(ErrRunMismatch)
	}
	// Definitive authorization: the session must be a current (non-replaced) session.
	registration := reg.Resolve(sessionID)
	if registration.Status != state.RegCurrent {
		return reject(ErrUnauthorized)
	}

	// Accept-once replay (after authorization). A same-digest replay re-confirms the
	// sink and returns the durable receipt even if the run is now terminal; a
	// different digest conflicts without touching the sink. No transition/mutation.
	if acc, seen := rs.AcceptedTurns[env.TurnID]; seen {
		if acc.ArtifactDigest != digest {
			return reject(ErrConflict)
		}
		if perr := deps.Sink.Put(env.TurnID, digest, canonRedacted); perr != nil {
			return reject(perr)
		}
		return SubmitResult{Receipt: acc.Receipt, Idempotent: true, ReleaseWarning: releaseOutcome(true, acc.Receipt.Revision, release())}, nil
	}

	// Staleness first, then liveness — a paused/gated/recovering run refuses before
	// any turn/phase reasoning (its phase has no turn spec), then turn identity.
	if env.StateRevision != rs.Revision {
		return reject(staleErr(rs, env.StateRevision))
	}
	if rs.Lifecycle != state.LifecycleRunning || rs.Recovery != nil || rs.Gate != nil {
		return reject(ErrNotAccepting)
	}
	if rs.Assignment == nil {
		return reject(ErrNoActiveTurn)
	}
	if env.TurnID != rs.Assignment.ID {
		return reject(ErrWrongTurn)
	}
	if rs.Assignment.IssuedRevision != rs.Revision {
		return reject(ErrNotAccepting)
	}

	// The phase's role must be the session's current role.
	spec, ok := TurnSpec(rs.Phase)
	if !ok {
		return reject(fmt.Errorf("%w: %s", ErrPhaseNotActionable, rs.Phase))
	}
	wantSlot, ok := slotForRole(spec.Role)
	if !ok {
		return reject(ErrUnauthorized)
	}
	if registration.Role != wantSlot {
		return reject(ErrUnauthorized)
	}

	// Schema-validate both the raw and the canonical-redacted bytes (value-free).
	if _, verr := protocol.Validate(spec.ArtifactMessageType, raw); verr != nil {
		return reject(fmt.Errorf("transport: submission fails its schema: %w", verr))
	}
	if _, verr := protocol.Validate(spec.ArtifactMessageType, canonRedacted); verr != nil {
		return reject(fmt.Errorf("transport: redacted submission fails its schema: %w", verr))
	}
	if derr := checkDecision(env); derr != nil {
		return reject(derr)
	}

	prepared := PreparedSubmit{
		MessageType:           spec.ArtifactMessageType,
		TurnID:                env.TurnID,
		Revision:              rs.Revision,
		Digest:                digest,
		CanonicalJSON:         string(canonRedacted),
		RequiresHumanDecision: env.RequiresHumanDecision,
		DecisionQuestion:      deref(env.DecisionQuestion),
	}
	pt, perr := deps.Prepare(cloneRunState(rs), prepared)
	if perr != nil {
		return reject(perr)
	}
	// Recheck the issued identities against the locked state before publishing.
	if idErr := recheckIssuedIDs(pt, rs, env.TurnID); idErr != nil {
		return reject(idErr)
	}

	// PUBLISH the immutable artifact before recording acceptance.
	if perr := deps.Sink.Put(env.TurnID, digest, canonRedacted); perr != nil {
		return reject(perr)
	}

	// ACCEPT: from here the append completes regardless of context cancellation, and
	// runs exactly once. A failure leaves an orphan artifact for an identical retry.
	committed, merr := deps.Store.MutateLocked(g, rs.Revision, func(gen uint64, next *state.RunState) error {
		if aerr := pt.apply(gen, next); aerr != nil {
			return aerr
		}
		if lerr := requireLiveOwner(next, env.TurnID, gen); lerr != nil {
			return lerr
		}
		next.AcceptedTurns[env.TurnID] = state.AcceptedTurn{
			ArtifactDigest: digest,
			Receipt:        state.Receipt{TurnID: env.TurnID, Revision: gen, ArtifactDigest: digest},
			Phase:          rs.Phase,
		}
		return nil
	})
	if merr != nil {
		// The transition is deterministic and was id-rechecked, so a MutateLocked
		// failure is a transition/store error to return, not a lost race to retry.
		// The published artifact is an unaccepted orphan; an identical later submit
		// re-confirms it (idempotent Put) before accepting. A conflict/busy here is an
		// invariant (the guard is held), not a CAS retry.
		_ = release()
		if errors.Is(merr, state.ErrRevisionConflict) || errors.Is(merr, genstore.ErrBusy) {
			return SubmitResult{}, fmt.Errorf("transport: unexpected lock/revision state under the run guard: %w", merr)
		}
		return SubmitResult{}, merr
	}
	acc, ok := committed.AcceptedTurns[env.TurnID]
	if !ok || acc.ArtifactDigest != digest {
		_ = release()
		return SubmitResult{}, fmt.Errorf("transport: committed submit could not be reconciled")
	}
	return SubmitResult{Receipt: acc.Receipt, ReleaseWarning: releaseOutcome(true, committed.Revision, release())}, nil
}

// releaseOutcome classifies a lock-release result: before acceptance a release error
// is an ordinary error (the caller joins it with any operation error); after a
// committed append it is a genstore.PostCommitError over the committed revision (the
// write is durable and must not be retried). It is pure and independently testable.
func releaseOutcome(accepted bool, committedRev uint64, relErr error) error {
	if relErr == nil {
		return nil
	}
	if accepted {
		return &genstore.PostCommitError{Generation: committedRev, Err: relErr}
	}
	return relErr
}

// slotForRole is the exact, closed conversion from a turn-spec role to a registry
// slot role; there is no default.
func slotForRole(r Role) (state.SlotRole, bool) {
	switch r {
	case RoleLead:
		return state.SlotLead, true
	case RolePair:
		return state.SlotPair, true
	}
	return "", false
}

// recheckIssuedIDs re-validates the identities a transition will issue against the
// locked state before publishing: exactly one of an assignment or a gate id (or
// neither for a terminal edge), each canonical, and a new assignment that is neither
// the consumed turn nor an already-accepted turn.
func recheckIssuedIDs(pt PreparedTransition, rs state.RunState, submittedTurn string) error {
	turn, gate := pt.issuedTurnID, pt.issuedGateID
	if turn != "" && gate != "" {
		return fmt.Errorf("%w: a transition issues an assignment or a gate, not both", ErrTransitionInvalid)
	}
	if turn != "" {
		if !state.IsRunID(turn) {
			return fmt.Errorf("%w: the issued assignment id is not canonical", ErrTransitionInvalid)
		}
		if turn == submittedTurn {
			return fmt.Errorf("%w: the issued assignment reuses the consumed turn", ErrTransitionInvalid)
		}
		if _, ok := rs.AcceptedTurns[turn]; ok {
			return fmt.Errorf("%w: the issued assignment reuses an accepted turn", ErrTransitionInvalid)
		}
	}
	if gate != "" && !state.IsRunID(gate) {
		return fmt.Errorf("%w: the issued gate id is not canonical", ErrTransitionInvalid)
	}
	return nil
}

// cloneRunState deep-copies a run state via a canonical round-trip, so Prepare
// receives an independent snapshot it cannot use to mutate transport's state.
func cloneRunState(rs state.RunState) state.RunState {
	b, err := json.Marshal(rs)
	if err != nil {
		return state.RunState{}
	}
	var c state.RunState
	if err := json.Unmarshal(b, &c); err != nil {
		return state.RunState{}
	}
	return c
}

func staleErr(rs state.RunState, submitted uint64) *StaleError {
	turn := ""
	if rs.Assignment != nil {
		turn = rs.Assignment.ID
	}
	return &StaleError{SubmittedRevision: submitted, CurrentRevision: rs.Revision, Lifecycle: rs.Lifecycle, Phase: rs.Phase, CurrentTurnID: turn}
}

// requireLiveOwner enforces that the transition left the run with a real next owner,
// so accepting the submit can never strand it. See the transition table for the
// exact legal shapes.
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

// checkDecision enforces the envelope's human-decision XOR.
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
