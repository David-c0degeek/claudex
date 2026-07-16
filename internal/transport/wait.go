package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/redact"
	"github.com/David-c0degeek/claudex/internal/state"
)

const waitEventType = "wait_event"

// WaitKind classifies why a wait returned.
type WaitKind string

const (
	WaitUnchanged        WaitKind = "unchanged"
	WaitAssignment       WaitKind = "assignment"
	WaitGate             WaitKind = "gate"
	WaitPausedBudget     WaitKind = "paused_budget"
	WaitRateLimited      WaitKind = "rate_limited"
	WaitCancelled        WaitKind = "cancelled"
	WaitCompleted        WaitKind = "completed"
	WaitFailed           WaitKind = "failed"
	WaitSessionReplaced  WaitKind = "session_replaced"
	WaitRecoveryRequired WaitKind = "recovery_required"
)

var (
	// ErrInvalidTimeout means the wait timeout is not in the allowed range.
	ErrInvalidTimeout = errors.New("transport: wait timeout must be > 0 and <= the maximum")
	// ErrCorruptState means durable state is internally inconsistent (e.g. an
	// AWAIT_GUIDANCE phase with no gate), so wait fails closed.
	ErrCorruptState = errors.New("transport: run state is internally inconsistent")
	// ErrSessionView means the session seam failed or returned invalid facts.
	ErrSessionView = errors.New("transport: session view")
)

const (
	maxWaitTimeout  = time.Hour
	waitPollInitial = 25 * time.Millisecond
	waitPollMax     = 250 * time.Millisecond
)

// ClientAheadError means the caller's cursor is beyond the durable revision.
type ClientAheadError struct {
	SinceRevision   uint64
	CurrentRevision uint64
	Phase           state.Phase
	Lifecycle       state.Lifecycle
}

func (e *ClientAheadError) Error() string {
	return fmt.Sprintf("transport: client cursor %d is ahead of the durable revision %d (phase %s, lifecycle %s)",
		e.SinceRevision, e.CurrentRevision, e.Phase, e.Lifecycle)
}

// WaitEvent is the wire message a wait returns.
type WaitEvent struct {
	ProtocolVersion       int             `json:"protocol_version"`
	MessageType           string          `json:"message_type"`
	Kind                  WaitKind        `json:"kind"`
	Revision              uint64          `json:"revision"`
	Phase                 state.Phase     `json:"phase"`
	Lifecycle             state.Lifecycle `json:"lifecycle"`
	TurnID                *string         `json:"turn_id"`
	GateID                *string         `json:"gate_id"`
	ReplacementGeneration *uint64         `json:"replacement_generation"`
	Code                  *string         `json:"code"`
	Reason                *string         `json:"reason"`
	NextAction            *string         `json:"next_action"`
}

// SessionInput is the immutable, value-only projection handed to the session
// seam, so the seam cannot alias or mutate authoritative state.
type SessionInput struct {
	Revision     uint64
	Phase        state.Phase
	Lifecycle    state.Lifecycle
	ActiveTurnID string // "" when no turn is assigned
}

// SessionView is the fact projection the seam returns. Wait constructs the
// authoritative event; the seam only reports whether this session owns the
// active turn and whether it has been replaced (with the superseding generation).
type SessionView struct {
	OwnsActiveTurn        bool
	Replaced              bool
	ReplacementGeneration uint64
}

// SessionViewer answers the session-specific facts from durable registration. It
// MUST be pure and side-effect-free and return value-free errors. An unknown or
// corrupt session registration must return an error so wait fails closed.
type SessionViewer func(in SessionInput, sessionID string) (SessionView, error)

type waitClock interface {
	timeout(d time.Duration) <-chan time.Time
	poll(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) timeout(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) poll(d time.Duration) <-chan time.Time    { return time.After(d) }

// Wait is a bounded, lock-free long-poll returning the next event relevant to a
// session, or WaitUnchanged on timeout. It never acquires the mutation lock and
// never mutates. Missing/corrupt state and a client-ahead cursor are typed
// errors; every returned event is fully validated against its schema.
func Wait(ctx context.Context, store *state.Store, sessionID string, sinceRevision uint64, timeout time.Duration, view SessionViewer) (WaitEvent, error) {
	return waitWithClock(ctx, store, sessionID, sinceRevision, timeout, view, realClock{})
}

func waitWithClock(ctx context.Context, store *state.Store, sessionID string, sinceRevision uint64, timeout time.Duration, view SessionViewer, clk waitClock) (WaitEvent, error) {
	if view == nil {
		return WaitEvent{}, ErrMissingSeam
	}
	if timeout <= 0 || timeout > maxWaitTimeout {
		return WaitEvent{}, ErrInvalidTimeout
	}

	last, wake, err := pollOnce(store, sessionID, sinceRevision, view)
	if err != nil {
		return WaitEvent{}, err
	}
	if wake {
		return last, nil
	}

	deadline := clk.timeout(timeout)
	interval := waitPollInitial
	for {
		select {
		case <-ctx.Done():
			return WaitEvent{}, ctx.Err()
		case <-deadline:
			ev, _, err := pollOnce(store, sessionID, sinceRevision, view)
			if err != nil {
				return WaitEvent{}, err
			}
			return ev, nil
		case <-clk.poll(interval):
			ev, wake, err := pollOnce(store, sessionID, sinceRevision, view)
			if err != nil {
				return WaitEvent{}, err
			}
			if wake {
				return ev, nil
			}
			if ev.Revision != last.Revision {
				interval = waitPollInitial
			} else if interval < waitPollMax {
				interval *= 2
				if interval > waitPollMax {
					interval = waitPollMax
				}
			}
			last = ev
		}
	}
}

// pollOnce reads state once (lock-free), classifies it, and validates the
// resulting event so every wait return is a valid wire message.
func pollOnce(store *state.Store, sessionID string, since uint64, view SessionViewer) (WaitEvent, bool, error) {
	rs, ok, err := store.Load()
	if err != nil {
		return WaitEvent{}, false, err
	}
	if !ok {
		return WaitEvent{}, false, ErrNoRun
	}
	if rs.Revision < since {
		return WaitEvent{}, false, &ClientAheadError{SinceRevision: since, CurrentRevision: rs.Revision, Phase: rs.Phase, Lifecycle: rs.Lifecycle}
	}

	facts := captureFacts(rs)
	classified, wake, cerr := classify(facts, sessionID, since, view)
	if cerr != nil {
		return WaitEvent{}, false, cerr
	}
	ev := classified
	if !wake {
		ev = newEvent(WaitUnchanged, facts)
	}
	if err := ev.validate(); err != nil {
		return WaitEvent{}, false, fmt.Errorf("transport: constructed wait event is invalid: %w", err)
	}
	return ev, wake, nil
}

// runFacts is the authoritative snapshot captured before the seam runs, so the
// seam cannot influence classification.
type runFacts struct {
	revision     uint64
	phase        state.Phase
	lifecycle    state.Lifecycle
	assignmentID string
	gateID       string
	recovery     *projFacts
	failure      *projFacts
}

type projFacts struct{ code, reason, nextAction string }

func captureFacts(rs state.RunState) runFacts {
	f := runFacts{revision: rs.Revision, phase: rs.Phase, lifecycle: rs.Lifecycle}
	if rs.Assignment != nil {
		f.assignmentID = rs.Assignment.ID
	}
	if rs.Gate != nil {
		f.gateID = rs.Gate.ID
	}
	if rs.Recovery != nil {
		f.recovery = &projFacts{rs.Recovery.Code, redact.Text(rs.Recovery.Reason), rs.Recovery.NextAction}
	}
	if rs.Failure != nil {
		f.failure = &projFacts{rs.Failure.Code, redact.Text(rs.Failure.Reason), rs.Failure.NextAction}
	}
	return f
}

// classify applies the wake priority. Order: state coherence (fail closed on a
// corrupt gate/pause shape); the session seam, which runs on EVERY poll and fails
// closed for an unknown/corrupt session before any event priority; a replacement,
// which is registration-driven and wakes even at the same run revision; then the
// revision-gated run-state events (terminal, gate, budget/rate pause, recovery,
// own assignment).
func classify(f runFacts, sessionID string, since uint64, view SessionViewer) (WaitEvent, bool, error) {
	if err := coherenceCheck(f); err != nil {
		return WaitEvent{}, false, err
	}

	v, err := view(SessionInput{Revision: f.revision, Phase: f.phase, Lifecycle: f.lifecycle, ActiveTurnID: f.assignmentID}, sessionID)
	if err != nil {
		// The viewer's error string is arbitrary; surface only the stable sentinel.
		return WaitEvent{}, false, ErrSessionView
	}
	if err := validateView(v, f.assignmentID); err != nil {
		return WaitEvent{}, false, err
	}

	if v.Replaced {
		ev := newEvent(WaitSessionReplaced, f)
		g := v.ReplacementGeneration
		ev.ReplacementGeneration = &g
		return ev, true, nil
	}

	if f.revision <= since {
		return WaitEvent{}, false, nil
	}

	if state.IsTerminalLifecycle(f.lifecycle) {
		return terminalEvent(f)
	}
	if f.phase == state.PhaseAwaitGuidance {
		ev := newEvent(WaitGate, f) // coherence guarantees a gate id + paused lifecycle
		id := f.gateID
		ev.GateID = &id
		return ev, true, nil
	}
	switch f.lifecycle {
	case state.LifecyclePausedBudget:
		return newEvent(WaitPausedBudget, f), true, nil
	case state.LifecycleRateLimited:
		return newEvent(WaitRateLimited, f), true, nil
	}
	if f.recovery != nil {
		ev := newEvent(WaitRecoveryRequired, f)
		setProjection(&ev, f.recovery)
		return ev, true, nil
	}
	if v.OwnsActiveTurn {
		ev := newEvent(WaitAssignment, f)
		id := f.assignmentID
		ev.TurnID = &id
		return ev, true, nil
	}
	return WaitEvent{}, false, nil
}

// coherenceCheck fails closed on an internally inconsistent gate/pause shape: a
// human-decision gate is exactly AWAIT_GUIDANCE with a gate and a paused
// lifecycle, and none of those three appears without the others.
func coherenceCheck(f runFacts) error {
	atGate := f.phase == state.PhaseAwaitGuidance
	hasGate := f.gateID != ""
	isPaused := f.lifecycle == state.LifecyclePaused
	if atGate {
		if !hasGate || !isPaused {
			return fmt.Errorf("%w: AWAIT_GUIDANCE requires a gate and a paused lifecycle", ErrCorruptState)
		}
		return nil
	}
	if hasGate {
		return fmt.Errorf("%w: a gate is set outside AWAIT_GUIDANCE", ErrCorruptState)
	}
	if isPaused {
		return fmt.Errorf("%w: a paused lifecycle outside AWAIT_GUIDANCE", ErrCorruptState)
	}
	return nil
}

// validateView rejects contradictory seam facts.
func validateView(v SessionView, activeTurnID string) error {
	if v.Replaced == (v.ReplacementGeneration == 0) {
		return fmt.Errorf("%w: replaced and replacement generation disagree", ErrSessionView)
	}
	if v.OwnsActiveTurn && v.Replaced {
		return fmt.Errorf("%w: a session cannot both own a turn and be replaced", ErrSessionView)
	}
	if v.OwnsActiveTurn && activeTurnID == "" {
		return fmt.Errorf("%w: claims turn ownership but no turn is active", ErrSessionView)
	}
	return nil
}

func terminalEvent(f runFacts) (WaitEvent, bool, error) {
	switch f.lifecycle {
	case state.LifecycleCancelled:
		return newEvent(WaitCancelled, f), true, nil
	case state.LifecycleCompleted:
		return newEvent(WaitCompleted, f), true, nil
	default: // failed_terminal, failed_retryable
		if f.failure == nil {
			return WaitEvent{}, false, fmt.Errorf("%w: a failed lifecycle has no failure projection", ErrCorruptState)
		}
		ev := newEvent(WaitFailed, f)
		setProjection(&ev, f.failure)
		return ev, true, nil
	}
}

func setProjection(ev *WaitEvent, p *projFacts) {
	code, reason, action := p.code, p.reason, p.nextAction
	ev.Code, ev.Reason, ev.NextAction = &code, &reason, &action
}

func newEvent(kind WaitKind, f runFacts) WaitEvent {
	return WaitEvent{
		ProtocolVersion: protocol.SupportedVersion,
		MessageType:     waitEventType,
		Kind:            kind,
		Revision:        f.revision,
		Phase:           f.phase,
		Lifecycle:       f.lifecycle,
	}
}

// semanticValidate checks the per-kind discriminant invariants the JSON schema
// cannot express.
func (e WaitEvent) semanticValidate() error {
	has := func(p any) bool {
		switch v := p.(type) {
		case *string:
			return v != nil
		case *uint64:
			return v != nil
		}
		return false
	}
	turn, gate, gen := has(e.TurnID), has(e.GateID), has(e.ReplacementGeneration)
	proj := has(e.Code) && has(e.Reason) && has(e.NextAction)
	anyProj := has(e.Code) || has(e.Reason) || has(e.NextAction)

	fail := func(msg string) error { return fmt.Errorf("%w: %s", ErrAssignmentInvalid, msg) }
	_, agentPhase := TurnSpec(e.Phase)
	switch e.Kind {
	case WaitAssignment:
		if !turn || gate || gen || anyProj {
			return fail("assignment needs a turn_id only")
		}
		if !agentPhase || e.Lifecycle != state.LifecycleRunning {
			return fail("assignment must be a running agent phase")
		}
	case WaitGate:
		if !gate || turn || gen || anyProj {
			return fail("gate needs a gate_id only")
		}
		if e.Phase != state.PhaseAwaitGuidance || e.Lifecycle != state.LifecyclePaused {
			return fail("gate must be AWAIT_GUIDANCE and paused")
		}
	case WaitPausedBudget:
		if turn || gate || gen || anyProj {
			return fail("paused_budget carries no discriminant")
		}
		if e.Lifecycle != state.LifecyclePausedBudget {
			return fail("paused_budget requires a paused_budget lifecycle")
		}
	case WaitRateLimited:
		if turn || gate || gen || anyProj {
			return fail("rate_limited carries no discriminant")
		}
		if e.Lifecycle != state.LifecycleRateLimited {
			return fail("rate_limited requires a rate_limited lifecycle")
		}
	case WaitCancelled:
		if turn || gate || gen || anyProj || e.Lifecycle != state.LifecycleCancelled {
			return fail("cancelled requires a cancelled lifecycle and no discriminant")
		}
	case WaitCompleted:
		if turn || gate || gen || anyProj || e.Lifecycle != state.LifecycleCompleted {
			return fail("completed requires a completed lifecycle and no discriminant")
		}
	case WaitFailed:
		if !proj || turn || gate || gen {
			return fail("failed needs code, reason, and next_action")
		}
		if !state.IsFailureLifecycle(e.Lifecycle) {
			return fail("failed requires a failure lifecycle")
		}
	case WaitRecoveryRequired:
		if !proj || turn || gate || gen {
			return fail("recovery_required needs code, reason, and next_action")
		}
		if e.Lifecycle != state.LifecycleRunning {
			return fail("recovery_required requires a running lifecycle")
		}
	case WaitSessionReplaced:
		if !gen || turn || gate || anyProj {
			return fail("session_replaced needs a replacement_generation only")
		}
	case WaitUnchanged:
		if turn || gate || gen || anyProj {
			return fail("unchanged carries no discriminant")
		}
	default:
		return fail("unknown wait kind " + string(e.Kind))
	}
	return nil
}

// validate fully checks the event: discriminant invariants plus the schema.
func (e WaitEvent) validate() error {
	_, err := e.Marshal()
	return err
}

// Marshal validates the event and returns canonical wire bytes.
func (e WaitEvent) Marshal() ([]byte, error) {
	if err := e.semanticValidate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("transport: marshal wait event: %w", err)
	}
	canon, err := protocol.Validate(waitEventType, raw)
	if err != nil {
		return nil, fmt.Errorf("transport: wait event fails its schema: %w", err)
	}
	return canon, nil
}
