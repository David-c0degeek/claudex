// Package coordinator wires the pure phase engine to the durable stores behind one
// production entrypoint. OpenRun binds a run to its canonical paths, and Run.Submit
// drives an accepted submit through the locked transport with an engine-backed
// Prepare that is PRECOMPUTED off the run guard: the fact reconstruction (artifact
// I/O) and identity minting (RNG) happen against an optimistic snapshot before
// transport acquires the lock, so the guarded critical section is only the pure
// engine work (re-validate refs, Project, Evaluate, choose the pre-minted id, Apply).
//
// Scope: this build serves the PLAN_DRAFT..FIX submit subset the engine supports. It
// does NOT implement TESTS/VERIFY, gate resolution, session replacement, or any CLI;
// an unsupported edge fails closed before any artifact is published.
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/engine"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
)

var (
	// ErrNotReady means the run is not a completed pairing, so it cannot serve submits.
	ErrNotReady = errors.New("coordinator: run is not a completed pairing")
	// ErrClosed means Submit was called after the run was closed.
	ErrClosed = errors.New("coordinator: run is closed")
	// ErrFactDrift means the reconstructed candidate facts disagree with the durable
	// state — the immutable history and the run state are inconsistent, so the submit
	// is rejected before any artifact is published.
	ErrFactDrift = errors.New("coordinator: reconstructed facts disagree with the durable state")
	// ErrEvidence means a referenced planning artifact is missing or corrupt in the
	// store, so the candidate cannot be reconstructed.
	ErrEvidence = errors.New("coordinator: referenced artifact evidence is missing or corrupt")
	// ErrReplayInPrepare means Prepare was invoked for an already-accepted turn, which
	// the locked transport resolves before Prepare — a defensive invariant.
	ErrReplayInPrepare = errors.New("coordinator: prepare invoked for an already-accepted turn")
)

// Run is an opened run: its bound paths, the state/registry/artifact stores under the
// shared run lock, and the RNG the precompute mints identities from.
type Run struct {
	loc      attach.RunLocation
	state    *state.Store
	registry *state.RegistryStore
	store    *transport.ArtifactStore
	rng      io.Reader

	mintMu sync.Mutex // serializes RNG-backed minting across concurrent precomputes

	mu     sync.RWMutex // RLocked for a submit's lifetime; Locked by Close
	closed bool
}

// submitHooks are per-call test barriers carried on the submit context; nil in
// production. afterFacts fires inside precompute once fact preparation is done (or
// skipped) and before minting; afterPrecompute fires after precompute and before
// transport acquires the run lock. Both run while the submit holds its read lock.
type submitHooks struct {
	afterFacts      func()
	afterPrecompute func()
}

type hooksKey struct{}

func withHooks(ctx context.Context, h *submitHooks) context.Context {
	return context.WithValue(ctx, hooksKey{}, h)
}

func hooksFrom(ctx context.Context) *submitHooks {
	h, _ := ctx.Value(hooksKey{}).(*submitHooks)
	return h
}

// OpenRun binds runID to its canonical run paths (through the active-pointer/catalog/
// bootstrap-journal authority), verifies the pairing is complete, and opens the
// artifact store. A partial failure releases everything it acquired.
func OpenRun(repoDir, runID string, rng io.Reader) (*Run, error) {
	if rng == nil {
		return nil, fmt.Errorf("coordinator: OpenRun requires an RNG")
	}
	loc, err := attach.ResolveRun(repoDir, runID)
	if err != nil {
		return nil, err
	}
	// Fail fast if the run is not a completed pairing (the per-submit journal seam
	// re-checks under the submit guard).
	g, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, genstore.ErrBusy
	}
	class, cerr := attach.ClassifyPairJournal(g, loc)
	rerr := g.Release()
	if cerr != nil || rerr != nil {
		return nil, errors.Join(cerr, rerr)
	}
	if class != attach.JournalTerminal {
		return nil, fmt.Errorf("%w: pair journal class %d", ErrNotReady, class)
	}

	store, err := transport.NewArtifactStore(loc.ArtifactsDir)
	if err != nil {
		return nil, err
	}
	return &Run{
		loc:      loc,
		state:    state.Open(loc.StateDir, loc.RunLock),
		registry: state.OpenRegistry(loc.RegistryDir, loc.RunLock),
		store:    store,
		rng:      rng,
	}, nil
}

// Submit drives one accepted submit. It precomputes the Prepare OFF the run guard
// (fact I/O + minting against an optimistic snapshot), then hands transport a closure
// whose guarded work is pure. It holds a read lock for the submit's whole lifetime so
// Close cannot release the artifact store under an in-flight Get/Put.
func (rn *Run) Submit(ctx context.Context, sessionID string, raw []byte) (transport.SubmitResult, error) {
	rn.mu.RLock()
	defer rn.mu.RUnlock()
	if rn.closed {
		return transport.SubmitResult{}, ErrClosed
	}

	prepare, err := rn.precompute(ctx, raw)
	if err != nil {
		return transport.SubmitResult{}, err
	}
	if h := hooksFrom(ctx); h != nil && h.afterPrecompute != nil {
		h.afterPrecompute()
	}
	return transport.Submit(ctx, transport.SubmitDeps{
		Store:    rn.state,
		Registry: rn.registry,
		Journal:  pairJournalReader{loc: rn.loc},
		Sink:     rn.store,
		Prepare:  prepare,
	}, sessionID, raw)
}

// Close releases the artifact store. It waits for in-flight submits (the write lock
// blocks until every read lock is released), so it never races an artifact Get/Put.
func (rn *Run) Close() error {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if rn.closed {
		return nil
	}
	rn.closed = true
	return rn.store.Close()
}

// RunLock is the run's mutation lock (exposed for tests that acquire it directly).
func (rn *Run) RunLock() string { return rn.loc.RunLock }

// --- the journal seam ---

// pairJournalReader adapts the pair-attach journal classification to transport's
// JournalReader, so a submit never authorizes through a mid-attach registry.
type pairJournalReader struct {
	loc attach.RunLocation
}

func (r pairJournalReader) LockPath() string { return r.loc.RunLock }

// Head classifies the pair journal under the held submit guard. A Run is opened only
// after the pairing completed, so ONLY a still-Terminal journal may proceed: a
// NonTerminal maps to recovery, and an Absent/unknown/error (a completed journal that
// vanished or corrupted after OpenRun) fails closed to recovery-required rather than
// transport's permissive Absent-proceed.
func (r pairJournalReader) Head(g *genstore.Guard, runID string) (transport.JournalHead, error) {
	if runID != r.loc.RunID {
		return transport.JournalUnknown, fmt.Errorf("coordinator: submit run id %q is not the opened run %q", runID, r.loc.RunID)
	}
	class, err := attach.ClassifyPairJournal(g, r.loc)
	if err != nil {
		return transport.JournalUnknown, err
	}
	switch class {
	case attach.JournalTerminal:
		return transport.JournalTerminal, nil
	case attach.JournalNonTerminal:
		return transport.JournalNonterminal, nil
	default: // JournalAbsent or an unknown value: the completed pairing is gone.
		return transport.JournalUnknown, fmt.Errorf("coordinator: pair journal for %s is no longer a completed pairing (class %d)", runID, class)
	}
}

// --- precompute: build the Prepare off the run guard ---

// failPrepare returns a Prepare that always fails; transport uses it only when it
// would not otherwise call Prepare (a replay or a missing run), so its error is a
// defensive backstop, never the surfaced result.
func failPrepare(err error) transport.Prepare {
	return func(state.RunState, transport.PreparedSubmit) (transport.PreparedTransition, error) {
		return transport.PreparedTransition{}, err
	}
}

// precompute reads the optimistic state and, for a NEW turn, reconstructs the facts
// and pre-mints both candidate identities, capturing them in a closure whose guarded
// work does no I/O and no RNG. For an already-accepted turn it returns a fail-Prepare
// (transport resolves the replay before Prepare, independent of facts/RNG).
func (rn *Run) precompute(ctx context.Context, raw []byte) (transport.Prepare, error) {
	n, err := transport.Normalize(raw)
	if err != nil {
		return nil, err
	}
	rs, ok, err := rn.state.Load()
	if err != nil {
		return nil, err
	}
	if !ok {
		return failPrepare(transport.ErrNoRun), nil // transport rejects at load, before Prepare
	}
	if _, seen := rs.AcceptedTurns[n.TurnID]; seen {
		return failPrepare(ErrReplayInPrepare), nil // replay: no facts, no RNG
	}

	// New turn: reconstruct facts (only where the phase needs them) and pre-mint both
	// candidates against the optimistic snapshot.
	phaseNeedsFacts := rs.Phase == state.PhasePlanCritique || rs.Phase == state.PhasePlanRevise
	var facts engine.ProjectionFacts
	var refs engine.CandidateRefs
	if phaseNeedsFacts {
		facts, refs, err = rn.loadFacts(rs)
		if err != nil {
			return nil, err
		}
	}
	if h := hooksFrom(ctx); h != nil && h.afterFacts != nil {
		h.afterFacts()
	}
	turnCand, gateCand, err := rn.mintPair(takenSet(rs))
	if err != nil {
		return nil, err
	}

	prepare := func(snapshot state.RunState, prepared transport.PreparedSubmit) (transport.PreparedTransition, error) {
		// Re-validate the captured refs against the LOCKED snapshot before use.
		if phaseNeedsFacts {
			if derr := compareRefs(refs, snapshot); derr != nil {
				return transport.PreparedTransition{}, derr
			}
		}
		ev, perr := engine.Project(snapshot, []byte(prepared.CanonicalJSON), facts)
		if perr != nil {
			return transport.PreparedTransition{}, perr
		}
		dec, perr := engine.Evaluate(snapshot, ev, engine.RuntimeFacts{})
		if perr != nil {
			return transport.PreparedTransition{}, perr
		}
		idKind, perr := engine.RequiredID(dec)
		if perr != nil {
			return transport.PreparedTransition{}, perr
		}
		var ids engine.Ids
		var issuedTurn, issuedGate string
		switch idKind {
		case engine.IDAssignment:
			ids.AssignmentTurnID, issuedTurn = turnCand, turnCand
		case engine.IDGate:
			ids.GateID, issuedGate = gateCand, gateCand
		default:
			return transport.PreparedTransition{}, fmt.Errorf("coordinator: unsupported id kind %d", idKind)
		}
		submitted := ev.Source
		apply := func(gen uint64, next *state.RunState) error {
			return engine.Apply(dec, submitted, ids, gen, next)
		}
		return transport.NewPreparedTransition(issuedTurn, issuedGate, apply), nil
	}
	return prepare, nil
}

// mintPair issues BOTH candidate identities once (adding the assignment candidate to
// the taken set before minting the gate candidate), serialized so concurrent
// precomputes cannot interleave reads of the shared RNG. Only the one the chosen edge
// needs is persisted; the other is discarded.
func (rn *Run) mintPair(taken map[string]bool) (turn, gate string, err error) {
	rn.mintMu.Lock()
	defer rn.mintMu.Unlock()
	turn, err = state.MintTurnID(rn.rng, taken)
	if err != nil {
		return "", "", err
	}
	taken[turn] = true
	gate, err = state.MintGateID(rn.rng, taken)
	if err != nil {
		return "", "", err
	}
	return turn, gate, nil
}

// loadFacts reconstructs the candidate facts from the real ArtifactStore, bounding
// the work BEFORE loading: it collects the durable planning-turn refs, rejects a
// count over the artifact bound, then loads each exact (turn,digest) in receipt order
// while enforcing the cumulative byte cap, folds via the engine, and cross-checks
// every reconstructed ref against the durable state.
func (rn *Run) loadFacts(snapshot state.RunState) (engine.ProjectionFacts, engine.CandidateRefs, error) {
	type planRef struct {
		turnID, digest string
		phase          state.Phase
		receipt        uint64
	}
	var refs []planRef
	for tid, acc := range snapshot.AcceptedTurns {
		if isPlanningPhase(acc.Phase) {
			refs = append(refs, planRef{tid, acc.ArtifactDigest, acc.Phase, acc.Receipt.Revision})
		}
	}
	if len(refs) > engine.MaxPlanningArtifacts {
		return engine.ProjectionFacts{}, engine.CandidateRefs{}, fmt.Errorf("%w: %d planning artifacts", engine.ErrHistoryTooLarge, len(refs))
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].receipt < refs[j].receipt })

	arts := make([]engine.AcceptedArtifact, 0, len(refs))
	total := 0
	for _, r := range refs {
		canonical, gerr := rn.store.Get(r.turnID, r.digest)
		if gerr != nil {
			return engine.ProjectionFacts{}, engine.CandidateRefs{}, fmt.Errorf("%w: turn %s: %v", ErrEvidence, r.turnID, gerr)
		}
		if len(canonical) > engine.MaxMaterializeBytes || total > engine.MaxMaterializeBytes-len(canonical) {
			return engine.ProjectionFacts{}, engine.CandidateRefs{}, fmt.Errorf("%w: over %d bytes", engine.ErrHistoryTooLarge, engine.MaxMaterializeBytes)
		}
		total += len(canonical)
		arts = append(arts, engine.AcceptedArtifact{
			TurnID: r.turnID, Digest: r.digest, Phase: r.phase, ReceiptRevision: r.receipt, Canonical: canonical,
		})
	}

	facts, materRefs, err := engine.MaterializeCandidate(arts)
	if err != nil {
		return engine.ProjectionFacts{}, engine.CandidateRefs{}, err
	}
	if err := compareRefs(materRefs, snapshot); err != nil {
		return engine.ProjectionFacts{}, engine.CandidateRefs{}, err
	}
	return facts, materRefs, nil
}

// compareRefs asserts every reconstructed ref equals the durable state exactly: plan
// source/digest/step-count, check keys and digest, the expected phase, and the pending
// findings (source + keys, nil included).
func compareRefs(refs engine.CandidateRefs, s state.RunState) error {
	if s.CandidatePlan == nil || s.CandidateChecks == nil {
		return fmt.Errorf("%w: no candidate in the durable state", ErrFactDrift)
	}
	if refs.Source != s.CandidatePlan.Source {
		return fmt.Errorf("%w: plan source", ErrFactDrift)
	}
	if refs.PlanDigest != s.CandidatePlan.Digest {
		return fmt.Errorf("%w: plan digest", ErrFactDrift)
	}
	if refs.StepCount != s.CandidatePlan.StepCount {
		return fmt.Errorf("%w: plan step count", ErrFactDrift)
	}
	if refs.CheckDigest != s.CandidateChecks.Digest || !reflect.DeepEqual(refs.CheckKeys, s.CandidateChecks.Keys) {
		return fmt.Errorf("%w: check set", ErrFactDrift)
	}
	if refs.ExpectedPhase != s.Phase {
		return fmt.Errorf("%w: expected phase %s != durable %s", ErrFactDrift, refs.ExpectedPhase, s.Phase)
	}
	return comparePending(refs.PendingFindings, s.PendingFindings)
}

func comparePending(got, want *state.FindingObligations) error {
	if (got == nil) != (want == nil) {
		return fmt.Errorf("%w: pending-findings presence", ErrFactDrift)
	}
	if got == nil {
		return nil
	}
	if got.Source != want.Source || !reflect.DeepEqual(got.Keys, want.Keys) {
		return fmt.Errorf("%w: pending findings", ErrFactDrift)
	}
	return nil
}

// takenSet is the optimistic set of identities a fresh mint must avoid: every accepted
// turn plus the outstanding assignment, gate, and first-turn ids.
func takenSet(s state.RunState) map[string]bool {
	taken := make(map[string]bool, len(s.AcceptedTurns)+3)
	for tid := range s.AcceptedTurns {
		taken[tid] = true
	}
	if s.Assignment != nil {
		taken[s.Assignment.ID] = true
	}
	if s.Gate != nil {
		taken[s.Gate.ID] = true
	}
	if s.FirstTurn != nil {
		taken[s.FirstTurn.ID] = true
	}
	return taken
}

func isPlanningPhase(p state.Phase) bool {
	return p == state.PhasePlanDraft || p == state.PhasePlanCritique || p == state.PhasePlanRevise
}
