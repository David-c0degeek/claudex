// Package coordinator wires the pure phase engine to the durable stores behind one
// production entrypoint. OpenRun binds a run to its canonical paths, and Run.Submit
// drives an accepted submit through the locked transport with an engine-backed
// Prepare that is PRECOMPUTED off the run guard: the fact reconstruction (artifact
// I/O) and identity minting (RNG) happen against an optimistic snapshot before
// transport acquires the lock, so the guarded critical section is only the pure
// engine work (re-validate refs, Project, Evaluate, choose the pre-minted id, Apply).
//
// Scope: this build serves the PLAN_DRAFT..FIX agent-submit subset plus the
// coordinator-authored TESTS outcome (Run.SubmitTestOutcome drives the ownerless
// TESTS pass/fail into VERIFY or FIX). It does NOT implement the VERIFY submit, gate
// resolution, session replacement (attach.ReplaceAttach stands alone), or any CLI; an
// unsupported edge fails closed before any effect is published. Every mutating entrypoint
// gates on the aggregate journal reader (a complete pairing, no pending replacement)
// before Registry authority.
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
	// ErrNotTestsPhase means SubmitTestOutcome was called for a run that is not a live
	// TESTS phase, so the coordinator-authored outcome cannot be applied.
	ErrNotTestsPhase = errors.New("coordinator: run is not awaiting a TESTS outcome")
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
	repl, rperr := attach.ClassifyReplaceJournal(g, loc)
	rerr := g.Release()
	if cerr != nil || rperr != nil || rerr != nil {
		return nil, errors.Join(cerr, rperr, rerr)
	}
	if class != attach.JournalTerminal {
		return nil, fmt.Errorf("%w: pair journal class %d", ErrNotReady, class)
	}
	// A pending session replacement means the run is mid-supersession: fail fast here (the
	// per-submit journal seam re-checks under the submit guard) rather than open for submits.
	// Total switch: Absent/Terminal open, NonTerminal is pending, any unknown class fails closed.
	switch repl {
	case attach.ReplaceJournalAbsent, attach.ReplaceJournalTerminal:
		// No pending replacement — open the run.
	case attach.ReplaceJournalNonTerminal:
		return nil, fmt.Errorf("%w: session replacement pending", ErrNotReady)
	default:
		return nil, fmt.Errorf("%w: unknown replace journal class %d", ErrNotReady, repl)
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
		Journal:  runJournalReader{loc: rn.loc},
		Sink:     rn.store,
		Prepare:  prepare,
	}, sessionID, raw)
}

// SubmitTestOutcome authors the coordinator-owned TESTS pass/fail under the run guard. It
// is ownerless — no agent turn, no artifact, and no minted identity: the coordinator's
// TESTS runner (wired in 4d) supplies the outcome and the evidence digest. On a pass the
// run enters ownerless VERIFY with a fresh threshold one generation past the current pair
// session; a fail routes to the lead's FIX or the test-budget gate (the pure engine
// decides). It fails closed unless the pairing is complete, no replacement is pending, and
// the run is a live TESTS phase.
//
// evidenceDigest is the sha256 of the TESTS run's evidence; 4d will bind it to a real
// evidence artifact. Here it is required to be a well-formed ownerless source (hex64), the
// exact shape validateApply demands, but is not yet checked against a stored artifact.
func (rn *Run) SubmitTestOutcome(ctx context.Context, pass bool, evidenceDigest string) (state.RunState, error) {
	rn.mu.RLock()
	defer rn.mu.RUnlock()
	if rn.closed {
		return state.RunState{}, ErrClosed
	}
	if !state.IsHex64(evidenceDigest) {
		return state.RunState{}, fmt.Errorf("%w: evidence digest is not a sha256", ErrNotTestsPhase)
	}

	g, ok, err := genstore.Acquire(rn.loc.RunLock)
	if err != nil {
		return state.RunState{}, err
	}
	if !ok {
		return state.RunState{}, genstore.ErrBusy
	}
	rs, merr := rn.authorTestOutcome(g, pass, evidenceDigest)
	if rerr := g.Release(); merr == nil && rerr != nil {
		// The append committed but the lock release failed: report a committed error so a
		// caller never retries a done transition (mirrors state.Mutate / genstore.Append).
		return rs, &genstore.PostCommitError{Generation: rs.Revision, Err: rerr}
	}
	return rs, merr
}

// authorTestOutcome is SubmitTestOutcome's guarded critical section: the aggregate journal
// gate, the TESTS-phase precondition, the current pair generation, the pure engine
// decision, and the durable state append.
func (rn *Run) authorTestOutcome(g *genstore.Guard, pass bool, evidenceDigest string) (state.RunState, error) {
	// Aggregate journal gate: a complete pairing and no pending replacement, under the SAME
	// guard, before authoring off the RunState/Registry.
	if err := requireRunReady(g, rn.loc); err != nil {
		return state.RunState{}, err
	}
	rs, ok, err := rn.state.Load()
	if err != nil {
		return state.RunState{}, err
	}
	if !ok {
		return state.RunState{}, transport.ErrNoRun
	}
	if rs.Phase != state.PhaseTests {
		return state.RunState{}, fmt.Errorf("%w: run is in %s", ErrNotTestsPhase, rs.Phase)
	}
	reg, ok, err := rn.registry.Load()
	if err != nil {
		return state.RunState{}, err
	}
	if !ok {
		return state.RunState{}, transport.ErrNoRun
	}
	if reg.RunID != rs.RunID {
		return state.RunState{}, fmt.Errorf("%w: registry %q != state %q", transport.ErrRunMismatch, reg.RunID, rs.RunID)
	}
	pairGen, err := currentPairGeneration(reg)
	if err != nil {
		return state.RunState{}, err
	}

	ev := engine.Event{Kind: engine.EvTestsOutcome, Source: state.EventRef{Digest: evidenceDigest}, Pass: pass}
	dec, err := engine.Evaluate(rs, ev, engine.RuntimeFacts{CurrentPairGeneration: pairGen})
	if err != nil {
		return state.RunState{}, err
	}
	// A TESTS pass enters ownerless VERIFY (no identity); a fail issues the lead's FIX
	// assignment or the test-budget gate id. Mint exactly what the route requires.
	ids, err := rn.mintForDecision(dec, rs)
	if err != nil {
		return state.RunState{}, err
	}
	next, merr := rn.state.MutateLocked(g, rs.Revision, func(gen uint64, n *state.RunState) error {
		return engine.Apply(dec, ev.Source, ids, gen, n)
	})
	if merr != nil {
		if !genstore.IsDurabilityUnconfirmed(merr) {
			return state.RunState{}, merr
		}
		// Visible-but-durability-unconfirmed: the state append is valid but not power-safe.
		// Re-confirm the state store durable inline (still under the held guard) before
		// returning success; a persistent failure halts, never a false durable success.
		if cerr := rn.state.ConfirmDurable(g); cerr != nil {
			return state.RunState{}, errors.Join(merr, cerr)
		}
	}
	return next, nil
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

// runJournalReader adapts BOTH the pair-attach journal AND the session-replacement
// journal to transport's JournalReader, so a submit authorizes off the Registry only
// when the pairing is complete AND no replacement is pending. Both journals are
// classified under the SAME held submit guard, before any Registry authority.
type runJournalReader struct {
	loc attach.RunLocation
}

func (r runJournalReader) LockPath() string { return r.loc.RunLock }

// Head classifies the pair and replacement journals under the held submit guard. A Run
// is opened only after the pairing completed, so ONLY a still-Terminal pair journal may
// proceed: a NonTerminal maps to recovery, and an Absent/unknown/error (a completed
// journal that vanished or corrupted after OpenRun) fails closed to recovery-required
// rather than transport's permissive Absent-proceed. A pending replacement (a
// non-terminal or mis-bound replace journal) blocks the submit regardless of the pair
// journal: the Registry it would authorize against may be mid-supersession, so the
// submit is recovery-required until replaceAttach completes the pending replacement.
func (r runJournalReader) Head(g *genstore.Guard, runID string) (transport.JournalHead, error) {
	if runID != r.loc.RunID {
		return transport.JournalUnknown, fmt.Errorf("coordinator: submit run id %q is not the opened run %q", runID, r.loc.RunID)
	}
	pair, err := attach.ClassifyPairJournal(g, r.loc)
	if err != nil {
		return transport.JournalUnknown, err
	}
	repl, err := attach.ClassifyReplaceJournal(g, r.loc)
	if err != nil {
		// A mis-bound replacement head is recovery-required: fail closed rather than
		// authorize off a Registry a replacement may be mid-superseding.
		return transport.JournalUnknown, err
	}
	// Total switch: only Absent/Terminal fall through to the pair-journal decision; a pending
	// replacement blocks, and any unknown class fails closed rather than silently proceeding.
	switch repl {
	case attach.ReplaceJournalAbsent, attach.ReplaceJournalTerminal:
		// No pending replacement — the pair-journal decision below governs.
	case attach.ReplaceJournalNonTerminal:
		return transport.JournalNonterminal, nil
	default:
		return transport.JournalUnknown, fmt.Errorf("coordinator: unknown replace journal class %d", repl)
	}
	switch pair {
	case attach.JournalTerminal:
		return transport.JournalTerminal, nil
	case attach.JournalNonTerminal:
		return transport.JournalNonterminal, nil
	default: // JournalAbsent or an unknown value: the completed pairing is gone.
		return transport.JournalUnknown, fmt.Errorf("coordinator: pair journal for %s is no longer a completed pairing (class %d)", runID, pair)
	}
}

// mintForDecision mints the exact identity the decision's route requires: a turn id for a
// running edge (a TESTS fail routing to the lead's FIX), a gate id for a gate edge (the
// test-budget gate), or none for an ownerless edge (a TESTS pass entering VERIFY). RNG
// access is serialized with concurrent submit precomputes via mintMu.
func (rn *Run) mintForDecision(dec engine.Decision, rs state.RunState) (engine.Ids, error) {
	idKind, err := engine.RequiredID(dec)
	if err != nil {
		return engine.Ids{}, err
	}
	rn.mintMu.Lock()
	defer rn.mintMu.Unlock()
	switch idKind {
	case engine.IDNone:
		return engine.Ids{}, nil
	case engine.IDAssignment:
		turn, err := state.MintTurnID(rn.rng, takenSet(rs))
		if err != nil {
			return engine.Ids{}, err
		}
		return engine.Ids{AssignmentTurnID: turn}, nil
	case engine.IDGate:
		gate, err := state.MintGateID(rn.rng, takenSet(rs))
		if err != nil {
			return engine.Ids{}, err
		}
		return engine.Ids{GateID: gate}, nil
	default:
		return engine.Ids{}, fmt.Errorf("coordinator: unknown id kind %d", idKind)
	}
}

// requireRunReady classifies the pair and replacement journals under the held guard and
// returns transport.ErrRecoveryRequired unless the pairing is complete and no replacement
// is pending. It is the same aggregate gate runJournalReader.Head applies, for the
// coordinator-authored paths (SubmitTestOutcome) that do not flow through transport.
func requireRunReady(g *genstore.Guard, loc attach.RunLocation) error {
	pair, err := attach.ClassifyPairJournal(g, loc)
	if err != nil {
		return fmt.Errorf("%w: %v", transport.ErrRecoveryRequired, err)
	}
	if pair != attach.JournalTerminal {
		return fmt.Errorf("%w: pair journal class %d", transport.ErrRecoveryRequired, pair)
	}
	repl, err := attach.ClassifyReplaceJournal(g, loc)
	if err != nil {
		return fmt.Errorf("%w: %v", transport.ErrRecoveryRequired, err)
	}
	switch repl {
	case attach.ReplaceJournalAbsent, attach.ReplaceJournalTerminal:
		return nil
	case attach.ReplaceJournalNonTerminal:
		return fmt.Errorf("%w: session replacement pending", transport.ErrRecoveryRequired)
	default:
		return fmt.Errorf("%w: unknown replace journal class %d", transport.ErrRecoveryRequired, repl)
	}
}

// currentPairGeneration is the generation of the pair slot's current session, from the
// LOCKED validated Registry — the fresh-session VERIFY threshold is one past it. A
// missing/mismatched pair shape is a run mismatch (never a zero, which would corrupt the
// threshold). Mirrors transport's identical read for the agent-submit path.
func currentPairGeneration(reg state.Registry) (uint64, error) {
	if reg.Pair == nil {
		return 0, fmt.Errorf("%w: the run has no pair slot", transport.ErrRunMismatch)
	}
	r := reg.Resolve(reg.Pair.CurrentSessionID)
	if r.Status != state.RegCurrent || r.Role != state.SlotPair || r.CurrentGeneration == 0 {
		return 0, fmt.Errorf("%w: the pair slot's current session is not resolvable", transport.ErrRunMismatch)
	}
	return r.CurrentGeneration, nil
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
		// The locked current pair generation (from the Registry transport loaded under
		// the run guard) feeds the FIX->VERIFY threshold; it is never a caller claim.
		dec, perr := engine.Evaluate(snapshot, ev, engine.RuntimeFacts{CurrentPairGeneration: prepared.CurrentPairGeneration})
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
		case engine.IDNone:
			// An ownerless terminal-graph edge (CHECKPOINT->TESTS, FIX->TESTS/VERIFY,
			// VERIFY->DONE) issues neither identity; the pre-minted candidates are
			// discarded.
		default:
			return transport.PreparedTransition{}, fmt.Errorf("coordinator: unknown id kind %d", idKind)
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
