// Package coordinator wires the pure phase engine to the durable stores behind one
// production entrypoint. OpenRun binds a run to its canonical paths, and Run.Submit
// drives an accepted submit through the locked transport with an engine-backed
// Prepare: it loads the immutable planning history from the real ArtifactStore,
// reconstructs the candidate facts, cross-checks them against the locked state, then
// projects/evaluates the transition and mints the one identity the chosen edge issues.
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
	"path/filepath"
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
	// ErrReplayInPrepare means Prepare was invoked for an already-accepted turn, which
	// the locked transport resolves before Prepare — a defensive invariant.
	ErrReplayInPrepare = errors.New("coordinator: prepare invoked for an already-accepted turn")
)

// Run is an opened run: its bound paths, the state/registry/artifact stores under the
// shared run lock, and the RNG the Prepare adapter mints identities from.
type Run struct {
	loc      attach.RunLocation
	state    *state.Store
	registry *state.RegistryStore
	store    *transport.ArtifactStore
	rng      io.Reader

	mintMu sync.Mutex // serializes RNG-backed minting across concurrent submits

	mu     sync.RWMutex // RLocked for a submit's lifetime; Locked by Close
	closed bool
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
	if cerr != nil {
		return nil, cerr
	}
	if rerr != nil {
		return nil, rerr
	}
	if class != attach.JournalTerminal {
		return nil, fmt.Errorf("%w: pair journal class %d", ErrNotReady, class)
	}

	store, err := transport.NewArtifactStore(filepath.Join(loc.RunDir, "artifacts"))
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

// Submit drives one accepted submit through the locked transport. It holds a read
// lock for the submit's whole lifetime so Close cannot release the artifact store
// under an in-flight Get/Put.
func (rn *Run) Submit(ctx context.Context, sessionID string, raw []byte) (transport.SubmitResult, error) {
	rn.mu.RLock()
	defer rn.mu.RUnlock()
	if rn.closed {
		return transport.SubmitResult{}, ErrClosed
	}
	return transport.Submit(ctx, transport.SubmitDeps{
		Store:    rn.state,
		Registry: rn.registry,
		Journal:  pairJournalReader{loc: rn.loc},
		Sink:     rn.store,
		Prepare:  rn.prepare,
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
	case attach.JournalAbsent:
		return transport.JournalAbsent, nil
	default:
		return transport.JournalUnknown, fmt.Errorf("coordinator: unknown pair journal class %d", class)
	}
}

// --- the engine-backed Prepare ---

// prepare is the transport Prepare seam: from a deep clone of the locked state and
// the validated submit it reconstructs the candidate facts (for the plan-negotiation
// phases), projects and evaluates the transition, mints the one identity the chosen
// edge issues, and returns an honest PreparedTransition whose apply calls engine.Apply
// with exactly that decision and identity. Any unsupported edge, semantic rejection,
// or fact drift returns an error, so transport rejects before publishing the artifact.
func (rn *Run) prepare(snapshot state.RunState, prepared transport.PreparedSubmit) (transport.PreparedTransition, error) {
	// Defensive: the locked transport resolves an already-accepted turn before Prepare.
	if _, seen := snapshot.AcceptedTurns[prepared.TurnID]; seen {
		return transport.PreparedTransition{}, ErrReplayInPrepare
	}

	// Facts are reconstructed only for the plan-negotiation phases that need them;
	// every other new-turn phase projects against empty facts.
	var facts engine.ProjectionFacts
	if snapshot.Phase == state.PhasePlanCritique || snapshot.Phase == state.PhasePlanRevise {
		f, err := rn.loadFacts(snapshot)
		if err != nil {
			return transport.PreparedTransition{}, err
		}
		facts = f
	}

	canonical := []byte(prepared.CanonicalJSON)
	ev, err := engine.Project(snapshot, canonical, facts)
	if err != nil {
		return transport.PreparedTransition{}, err
	}
	dec, err := engine.Evaluate(snapshot, ev)
	if err != nil {
		return transport.PreparedTransition{}, err
	}
	idKind, err := engine.RequiredID(dec)
	if err != nil {
		return transport.PreparedTransition{}, err
	}

	issuedTurn, issuedGate, err := rn.mint(idKind, takenSet(snapshot))
	if err != nil {
		return transport.PreparedTransition{}, err
	}
	ids := engine.Ids{AssignmentTurnID: issuedTurn, GateID: issuedGate}
	submitted := ev.Source
	apply := func(gen uint64, next *state.RunState) error {
		return engine.Apply(dec, submitted, ids, gen, next)
	}
	return transport.NewPreparedTransition(issuedTurn, issuedGate, apply), nil
}

// mint issues the single identity the chosen edge requires, serialized so concurrent
// submits cannot interleave reads of the shared RNG.
func (rn *Run) mint(kind engine.IDKind, taken map[string]bool) (turn, gate string, err error) {
	rn.mintMu.Lock()
	defer rn.mintMu.Unlock()
	switch kind {
	case engine.IDAssignment:
		id, e := state.MintTurnID(rn.rng, taken)
		return id, "", e
	case engine.IDGate:
		id, e := state.MintGateID(rn.rng, taken)
		return "", id, e
	default:
		return "", "", fmt.Errorf("coordinator: unsupported id kind %d", kind)
	}
}

// loadFacts reconstructs the candidate facts from the real ArtifactStore: it filters
// the accepted planning turns, loads each exact (turn,digest) artifact, folds them in
// receipt order via the engine, then cross-checks EVERY reconstructed ref against the
// durable state, so a state that drifted from its immutable history is rejected.
func (rn *Run) loadFacts(snapshot state.RunState) (engine.ProjectionFacts, error) {
	var arts []engine.AcceptedArtifact
	for tid, acc := range snapshot.AcceptedTurns {
		if !isPlanningPhase(acc.Phase) {
			continue
		}
		canonical, err := rn.store.Get(tid, acc.ArtifactDigest)
		if err != nil {
			return engine.ProjectionFacts{}, fmt.Errorf("coordinator: load artifact for turn %s: %w", tid, err)
		}
		arts = append(arts, engine.AcceptedArtifact{
			TurnID:          tid,
			Digest:          acc.ArtifactDigest,
			Phase:           acc.Phase,
			ReceiptRevision: acc.Receipt.Revision,
			Canonical:       canonical,
		})
	}
	sort.Slice(arts, func(i, j int) bool { return arts[i].ReceiptRevision < arts[j].ReceiptRevision })

	facts, refs, err := engine.MaterializeCandidate(arts)
	if err != nil {
		return engine.ProjectionFacts{}, err
	}
	if err := compareRefs(refs, snapshot); err != nil {
		return engine.ProjectionFacts{}, err
	}
	return facts, nil
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
