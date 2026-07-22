package transport

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// countingGate is a WorktreeClean seam that records how many times it was invoked, so a
// test can prove the gate is NOT consulted on a replay or an unauthorized request.
func countingGate(clean bool, err error, calls *atomic.Int64) WorktreeClean {
	return func() (bool, error) {
		calls.Add(1)
		return clean, err
	}
}

// A read-only-phase submit (the CHECKPOINT fixture is a pair turn) against a DIRTY worktree
// is refused with ErrRepoMutationInReadOnlyPhase before any sink or state effect.
func TestSubmitReadOnlyDirtyRefused(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	var calls atomic.Int64
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
	deps.WorktreeClean = countingGate(false, nil, &calls)
	if _, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x")); !errors.Is(err, ErrRepoMutationInReadOnlyPhase) {
		t.Fatalf("dirty read-only submit err = %v, want ErrRepoMutationInReadOnlyPhase", err)
	}
	if sink.count() != 0 {
		t.Fatalf("a refused read-only submit published to the sink")
	}
	if loaded, _, _ := store.Load(); loaded.Revision != rev {
		t.Fatalf("a refused read-only submit advanced the run to %d", loaded.Revision)
	}
	if calls.Load() != 1 {
		t.Fatalf("gate invoked %d times, want exactly 1", calls.Load())
	}
}

// A clean worktree accepts the read-only-phase submit normally.
func TestSubmitReadOnlyCleanAccepts(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
	deps.WorktreeClean = func() (bool, error) { return true, nil }
	res, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "did it"))
	if err != nil {
		t.Fatalf("clean read-only submit: %v", err)
	}
	if res.Idempotent || res.Receipt.Revision != rev+1 {
		t.Fatalf("clean read-only submit receipt = %+v", res.Receipt)
	}
}

// An UNOBSERVABLE worktree (the gate's own error) fails closed as ErrWorktreeUnobserved —
// never mislabeled a mutation — before any sink or state effect.
func TestSubmitReadOnlyObservationFailureFailsClosed(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
	deps.WorktreeClean = func() (bool, error) { return false, errors.New("git status boom") }
	_, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x"))
	if !errors.Is(err, ErrWorktreeUnobserved) {
		t.Fatalf("unobservable worktree err = %v, want ErrWorktreeUnobserved", err)
	}
	if errors.Is(err, ErrRepoMutationInReadOnlyPhase) {
		t.Fatalf("an unobservable worktree was falsely reported as an observed mutation")
	}
	if sink.count() != 0 {
		t.Fatalf("an unobserved read-only submit published to the sink")
	}
	if loaded, _, _ := store.Load(); loaded.Revision != rev {
		t.Fatalf("an unobserved read-only submit advanced the run")
	}
}

// A gate that fails with context.Canceled (the request was cancelled while git status ran)
// preserves BOTH classifications: the worktree failed closed as unobserved AND the caller can
// still detect the cancellation — no sink or state effect.
func TestSubmitReadOnlyGateCancellationPreserved(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
	deps.WorktreeClean = func() (bool, error) { return false, context.Canceled }
	_, err := Submit(context.Background(), deps, pairSess, report("turn-1", rev, "x"))
	if !errors.Is(err, ErrWorktreeUnobserved) {
		t.Fatalf("err = %v, want it to wrap ErrWorktreeUnobserved", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the underlying context.Canceled to survive the chain", err)
	}
	if errors.Is(err, ErrRepoMutationInReadOnlyPhase) {
		t.Fatalf("a cancelled observation was falsely reported as a mutation")
	}
	if sink.count() != 0 {
		t.Fatalf("a cancelled read-only submit published to the sink")
	}
	if loaded, _, _ := store.Load(); loaded.Revision != rev {
		t.Fatalf("a cancelled read-only submit advanced the run")
	}
}

// An accept-once replay of an already-accepted read-only turn returns its durable receipt
// even when the worktree is now dirty — the gate is never consulted on a replay.
func TestSubmitReadOnlyGateSkippedOnReplay(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	clean := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
	clean.WorktreeClean = func() (bool, error) { return true, nil }
	first, err := Submit(context.Background(), clean, pairSess, report("turn-1", rev, "did it"))
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	// The lost-response retry arrives with the worktree now dirty; a counting gate proves it
	// is not consulted, and the original receipt is returned.
	var calls atomic.Int64
	replay := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
	replay.WorktreeClean = countingGate(false, nil, &calls)
	res, err := Submit(context.Background(), replay, pairSess, report("turn-1", rev, "did it"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !res.Idempotent || res.Receipt != first.Receipt {
		t.Fatalf("replay = %+v (idem %v), want the original receipt", res.Receipt, res.Idempotent)
	}
	if calls.Load() != 0 {
		t.Fatalf("the gate was consulted %d times on a replay, want 0", calls.Load())
	}
}

// A dirty-worktree submit that is ALSO unauthorized returns its authorization error, not the
// dirt error — the gate runs only after the exact turn is authorized, and is never consulted
// for an unauthorized request.
func TestSubmitReadOnlyGateSkippedForUnauthorized(t *testing.T) {
	store, rev := newRunWithActiveTurn(t)
	sink := newMemSink()
	var calls atomic.Int64
	deps := depsWith(store, sink, terminalJournal(store), openRunRegistry(store), checkpointPrep())
	deps.WorktreeClean = countingGate(false, nil, &calls)
	// leadSess is the wrong role for the pair-owned CHECKPOINT turn.
	if _, err := Submit(context.Background(), deps, leadSess, report("turn-1", rev, "x")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong-role dirty submit err = %v, want ErrUnauthorized", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("the gate was consulted %d times for an unauthorized request, want 0", calls.Load())
	}
	// A stale-revision dirty submit returns the stale error, not the dirt error.
	var se *StaleError
	_, serr := Submit(context.Background(), deps, pairSess, report("turn-1", rev-1, "x"))
	if !errors.As(serr, &se) {
		t.Fatalf("stale dirty submit err = %v, want *StaleError", serr)
	}
	if calls.Load() != 0 {
		t.Fatalf("the gate was consulted for a stale request, want 0")
	}
}
