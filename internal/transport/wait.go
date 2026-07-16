package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

const waitEventType = "wait_event"

// WaitKind classifies why a wait returned.
type WaitKind string

const (
	WaitUnchanged        WaitKind = "unchanged"
	WaitAssignment       WaitKind = "assignment"
	WaitGate             WaitKind = "gate"
	WaitCancelled        WaitKind = "cancelled"
	WaitCompleted        WaitKind = "completed"
	WaitFailed           WaitKind = "failed"
	WaitSessionReplaced  WaitKind = "session_replaced"
	WaitRecoveryRequired WaitKind = "recovery_required"
)

var (
	// ErrInvalidTimeout means the wait timeout is not in the allowed range.
	ErrInvalidTimeout = errors.New("transport: wait timeout must be > 0 and <= the maximum")
)

const (
	maxWaitTimeout  = time.Hour
	waitPollInitial = 25 * time.Millisecond
	waitPollMax     = 250 * time.Millisecond
)

// ClientAheadError means the caller's cursor is beyond the durable revision, so
// its view is inconsistent with the coordinator.
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

// WaitEvent is the wire message a wait returns: the wake reason plus the observed
// status and every discriminant needed to act.
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
}

// SessionView is the immutable projection the session seam returns: whether this
// session owns the active turn, and whether it has been replaced. Wait builds the
// authoritative event fields and applies the wake priority, so a seam can neither
// forge state fields nor suppress a global stop.
type SessionView struct {
	OwnsActiveTurn        bool
	Replaced              bool
	ReplacementGeneration uint64
}

// SessionViewer answers the session-specific facts from durable registration. It
// MUST be pure and side-effect-free and return only facts, not events.
type SessionViewer func(rs state.RunState, sessionID string) SessionView

// waitClock is the injectable clock so tests drive time deterministically.
type waitClock interface {
	timeout(d time.Duration) <-chan time.Time
	poll(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) timeout(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) poll(d time.Duration) <-chan time.Time    { return time.After(d) }

// Wait is a bounded, lock-free long-poll. It returns the next event relevant to
// this session once the revision advances past sinceRevision, or WaitUnchanged on
// timeout. It reads state without the mutation lock and never mutates, so it is
// idempotent and Ctrl-C-safe: a cancelled ctx returns ctx.Err(). Missing or
// corrupt state is an error, never unchanged, and a client cursor ahead of the
// durable revision is a typed ClientAheadError. The poll interval is internal
// (a caller cannot force a hot loop).
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
			// One final read, so an event committed at the boundary is not missed.
			// pollOnce returns the wake event, or the freshest unchanged snapshot.
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
			// Reset the backoff when the revision moves (activity), otherwise grow
			// it toward the ceiling so an idle wait does not re-enumerate history.
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

// pollOnce reads state once (lock-free) and classifies it. It returns the wake
// event (wake=true), or the current unchanged snapshot (wake=false); an error for
// missing/corrupt state or a client-ahead cursor.
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
	if rs.Revision > since {
		if ev, wake := classifyWait(rs, sessionID, view); wake {
			return ev, true, nil
		}
	}
	return newEvent(WaitUnchanged, rs), false, nil
}

// classifyWait applies the wake priority: a terminal/failure lifecycle dominates
// everything; then session replacement (the obsolete session must learn first);
// then a gate/paused stop; then a required recovery; then this session's own
// assignment. Global stops are read from durable state, never from the seam.
func classifyWait(rs state.RunState, sessionID string, view SessionViewer) (WaitEvent, bool) {
	if state.IsTerminalLifecycle(rs.Lifecycle) {
		return newEvent(terminalKind(rs.Lifecycle), rs), true
	}
	v := view(rs, sessionID)
	if v.Replaced {
		ev := newEvent(WaitSessionReplaced, rs)
		if v.ReplacementGeneration > 0 {
			g := v.ReplacementGeneration
			ev.ReplacementGeneration = &g
		}
		return ev, true
	}
	if rs.Gate != nil || state.IsPausedLifecycle(rs.Lifecycle) || rs.Phase == state.PhaseAwaitGuidance {
		ev := newEvent(WaitGate, rs)
		if rs.Gate != nil {
			id := rs.Gate.ID
			ev.GateID = &id
		}
		return ev, true
	}
	if rs.Recovery != nil {
		return newEvent(WaitRecoveryRequired, rs), true
	}
	if v.OwnsActiveTurn && rs.Assignment != nil {
		ev := newEvent(WaitAssignment, rs)
		id := rs.Assignment.ID
		ev.TurnID = &id
		return ev, true
	}
	return WaitEvent{}, false
}

func terminalKind(lc state.Lifecycle) WaitKind {
	switch lc {
	case state.LifecycleCancelled:
		return WaitCancelled
	case state.LifecycleCompleted:
		return WaitCompleted
	default: // failed_terminal, failed_retryable
		return WaitFailed
	}
}

func newEvent(kind WaitKind, rs state.RunState) WaitEvent {
	return WaitEvent{
		ProtocolVersion: protocol.SupportedVersion,
		MessageType:     waitEventType,
		Kind:            kind,
		Revision:        rs.Revision,
		Phase:           rs.Phase,
		Lifecycle:       rs.Lifecycle,
	}
}

// Validate checks the per-kind discriminant invariants the JSON schema cannot
// express (which optional field each kind requires or forbids).
func (e WaitEvent) Validate() error {
	switch e.Kind {
	case WaitAssignment:
		if e.TurnID == nil {
			return fmt.Errorf("%w: assignment needs a turn_id", ErrAssignmentInvalid)
		}
		if e.GateID != nil || e.ReplacementGeneration != nil {
			return fmt.Errorf("%w: assignment must not carry a gate or replacement", ErrAssignmentInvalid)
		}
	case WaitGate:
		if e.TurnID != nil || e.ReplacementGeneration != nil {
			return fmt.Errorf("%w: a gate must not carry a turn or replacement", ErrAssignmentInvalid)
		}
	case WaitSessionReplaced:
		if e.ReplacementGeneration == nil {
			return fmt.Errorf("%w: session_replaced needs a replacement_generation", ErrAssignmentInvalid)
		}
		if e.TurnID != nil || e.GateID != nil {
			return fmt.Errorf("%w: session_replaced must not carry a turn or gate", ErrAssignmentInvalid)
		}
	case WaitUnchanged, WaitCancelled, WaitCompleted, WaitFailed, WaitRecoveryRequired:
		if e.TurnID != nil || e.GateID != nil || e.ReplacementGeneration != nil {
			return fmt.Errorf("%w: %s must not carry a turn, gate, or replacement", ErrAssignmentInvalid, e.Kind)
		}
	default:
		return fmt.Errorf("%w: unknown wait kind %q", ErrAssignmentInvalid, e.Kind)
	}
	return nil
}

// Marshal validates the event (discriminants + schema) and returns canonical wire
// bytes.
func (e WaitEvent) Marshal() ([]byte, error) {
	if err := e.Validate(); err != nil {
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
