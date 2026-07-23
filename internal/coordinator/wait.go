package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// Wait long-polls the run for the next event relevant to sessionID, routing EVERY poll observation
// through the aggregate read authority: each poll is one coherent bracket (active pointer + State +
// Registry + all three domain-classified journal heads), so no viewer ever runs on unbracketed
// state and no cross-store view is torn. A transient nonterminal aggregate (a healthy peer submit's
// brief window) is ridden over; one that persists to the deadline surfaces as ErrReadRecoveryRequired
// — the SAME recovery sentinel Status uses, so the CLI has one recovery mapping. A run switch during
// the wait surfaces as ErrRunGone. It holds no lock.
func Wait(ctx context.Context, repoDir, runID, sessionID string, since uint64, timeout time.Duration) (transport.WaitEvent, error) {
	poll := func() (transport.PollObservation, error) {
		obs, err := readCoherent(repoDir, runID, func(loc attach.RunLocation) (transport.PollObservation, error) {
			return observePoll(loc, sessionID)
		})
		if errors.Is(err, ErrReadRecoveryRequired) {
			// A nonterminal aggregate this instant: not serviceable. Wait rides over a transient
			// one and re-checks; a persistent one is surfaced at the deadline.
			return transport.PollObservation{RecoveryRequired: true}, nil
		}
		if err != nil {
			return transport.PollObservation{}, err
		}
		return obs, nil
	}
	ev, err := transport.Wait(ctx, since, timeout, poll)
	if errors.Is(err, transport.ErrWaitRecoveryRequired) {
		return transport.WaitEvent{}, ErrReadRecoveryRequired
	}
	return ev, err
}

// observePoll captures one coherent (State, SessionView) pair INSIDE the bracket: it loads the run
// state and resolves the session's view from the SAME-bracket Registry read (BYO/protocol-only: the
// registration must load and belong to the run), so the view can never be torn from the state it
// describes.
func observePoll(loc attach.RunLocation, sessionID string) (transport.PollObservation, error) {
	rs, ok, err := state.Open(loc.StateDir, loc.RunLock).Load()
	if err != nil {
		return transport.PollObservation{}, err
	}
	if !ok {
		return transport.PollObservation{}, transport.ErrNoRun
	}
	reg, gok, err := state.OpenRegistry(loc.RegistryDir, loc.RunLock).Load()
	if err != nil {
		return transport.PollObservation{}, err
	}
	if !gok || reg.RunID != loc.RunID {
		return transport.PollObservation{}, fmt.Errorf("coordinator: no durable registration for the run")
	}
	assignmentID := ""
	if rs.Assignment != nil {
		assignmentID = rs.Assignment.ID
	}
	view, verr := transport.ResolveSessionView(reg, rs.Phase, assignmentID, sessionID)
	if verr != nil {
		return transport.PollObservation{}, verr
	}
	return transport.PollObservation{State: rs, View: view}, nil
}
