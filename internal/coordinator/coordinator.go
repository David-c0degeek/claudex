// Package coordinator wires the pure phase engine to the durable stores behind one
// production entrypoint. OpenRun binds a run to its canonical paths, and Run.Submit
// drives an accepted submit through the locked transport with an engine-backed
// Prepare that is PRECOMPUTED off the run guard: the fact reconstruction (artifact
// I/O) and identity minting (RNG) happen against an optimistic snapshot before
// transport acquires the lock, so the guarded critical section is only the pure
// engine work (re-validate refs, Project, Evaluate, choose the pre-minted id, Apply).
//
// Scope: this build serves the full agent-submit phase graph (PLAN_DRAFT..FIX plus the
// VERIFY verification, projected against the frozen task snapshot) and the
// coordinator-authored TESTS outcome (Run.SubmitTestOutcome drives the ownerless TESTS
// pass/fail into VERIFY or FIX). The ownerless VERIFY is activated by attach.ReplaceAttach
// (which stands alone). It does NOT yet run the tests itself (the TESTS runner seam is a
// later slice), resolve human gates, or expose a CLI; an unsupported edge fails closed
// before any effect is published. Every mutating entrypoint gates on the aggregate journal
// reader (a complete pairing, no pending replacement) before Registry authority.
package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"sync"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/config"
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
	return openRun(repoDir, runID, rng, defaultRecoverConfirm)
}

// recoverConfirmFn re-confirms a reopened run's whole durability chain under the held run
// guard. It is a per-call parameter (not a mutable global) so a test can inject a failing
// confirmer without behavior bleed across concurrent opens.
type recoverConfirmFn func(g *genstore.Guard, loc attach.RunLocation, st *state.Store, reg *state.RegistryStore) error

// defaultRecoverConfirm re-confirms BOTH journal authorities AND both stores on reopen. A
// prior mutation may have committed VISIBLY but left its directory entry durability-
// unconfirmed after a halted process — including a TERMINAL journal head, which the
// classifiers read with no barrier (Latest only), which is exactly why txn.Recover confirms
// even a terminal head. Re-confirm the whole chain under the SAME guard before serving; a
// persistent failure is recovery-required (halt), never a silent trust of a visible-but-
// unconfirmed record.
func defaultRecoverConfirm(g *genstore.Guard, loc attach.RunLocation, st *state.Store, reg *state.RegistryStore) error {
	if err := attach.ConfirmPairJournal(g, loc); err != nil {
		return err
	}
	if err := attach.ConfirmReplaceJournal(g, loc); err != nil {
		return err
	}
	if err := st.ConfirmDurable(g); err != nil {
		return err
	}
	return reg.ConfirmDurable(g)
}

func openRun(repoDir, runID string, rng io.Reader, confirm recoverConfirmFn) (*Run, error) {
	if rng == nil {
		return nil, fmt.Errorf("coordinator: OpenRun requires an RNG")
	}
	loc, err := attach.ResolveRun(repoDir, runID)
	if err != nil {
		return nil, err
	}
	st := state.Open(loc.StateDir, loc.RunLock)
	reg := state.OpenRegistry(loc.RegistryDir, loc.RunLock)

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
	var confErr error
	if cerr == nil && rperr == nil {
		confErr = confirm(g, loc, st, reg)
	}
	rerr := g.Release()
	if cerr != nil || rperr != nil || confErr != nil || rerr != nil {
		return nil, errors.Join(cerr, rperr, confErr, rerr)
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
		state:    st,
		registry: reg,
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

// SubmitTestOutcome authors the coordinator-owned TESTS pass/fail through the locked
// transport primitive. It is ownerless — no agent turn, no artifact, no accepted turn: the
// coordinator's TESTS runner (wired in 4d) supplies the outcome, the evidence digest, and
// the expectedRevision the outcome was derived against. The candidate identities are
// pre-minted OFF the guard (the FIX/gate id, selected under the guard by the pure engine
// adapter — never minted under the guard), and transport enforces the run identity bind
// (the aggregate journal reader), the stale-round guard (expectedRevision), context
// cancellation, and the exact TESTS-outcome shape. A pass enters ownerless VERIFY one
// generation past the pair; a fail routes to the lead's FIX or the test-budget gate.
//
// evidenceDigest is the sha256 of the TESTS run's evidence; 4d will bind it to a real
// evidence artifact. transport validates it as the ownerless empty-turn source shape.
func (rn *Run) SubmitTestOutcome(ctx context.Context, pass bool, evidenceDigest string, expectedRevision uint64) (transport.TestOutcomeResult, error) {
	rn.mu.RLock()
	defer rn.mu.RUnlock()
	if rn.closed {
		return transport.TestOutcomeResult{}, ErrClosed
	}
	// Validate context and cheap inputs BEFORE precompute, so an RNG/load error can never mask
	// the exact rejection the primitive returns (context cancellation, ErrBadEvidence), and a
	// doomed request reads no RNG. The primitive re-checks all of these authoritatively under
	// the run guard.
	if err := ctx.Err(); err != nil {
		return transport.TestOutcomeResult{}, err
	}
	if !state.IsHex64(evidenceDigest) || expectedRevision == 0 {
		return transport.TestOutcomeResult{}, transport.ErrBadEvidence
	}
	prepare, err := rn.precomputeTestOutcome(pass, expectedRevision)
	if err != nil {
		return transport.TestOutcomeResult{}, err
	}
	return transport.SubmitTestOutcome(ctx, transport.TestOutcomeDeps{
		Store:    rn.state,
		Registry: rn.registry,
		Journal:  runJournalReader{loc: rn.loc},
		Prepare:  prepare,
	}, expectedRevision, evidenceDigest)
}

// TestRunner executes a run's mechanical test gate and reports the outcome plus the evidence
// its digest binds to. It is an INJECTED SEAM: the coordinator never execs a command itself —
// production supplies a confined command executor (a later, security-reviewed slice), and
// tests supply a deterministic fake. The evidence bytes are whatever the runner captured
// (e.g. test output); the coordinator digests them for the outcome's ownerless source.
type TestRunner interface {
	RunTests(ctx context.Context, command string) (pass bool, evidence []byte, err error)
}

// RunTests runs the coordinator-owned TESTS gate for a run at ownerless TESTS and authors its
// outcome. Off the run guard it runs the gate — a DISABLED gate is a pass with no mechanical
// evidence (the pairing relies on the checkpoint reviews' tests_adequate), an ENABLED gate
// defers to the injected runner — digests the evidence into the ownerless source, and applies
// the outcome via SubmitTestOutcome (which re-validates the phase/revision and gates on the
// aggregate journal reader under the guard). The optimistic phase read only avoids running the
// gate for a run that plainly is not at TESTS; the authoritative check is SubmitTestOutcome's.
func (rn *Run) RunTests(ctx context.Context, runner TestRunner) (transport.TestOutcomeResult, error) {
	rs, ok, err := rn.state.Load()
	if err != nil {
		return transport.TestOutcomeResult{}, err
	}
	if !ok {
		return transport.TestOutcomeResult{}, transport.ErrNoRun
	}
	if rs.Phase != state.PhaseTests {
		return transport.TestOutcomeResult{}, fmt.Errorf("%w: run is in %s", transport.ErrNotTestsPhase, rs.Phase)
	}
	pass, evidence, err := runTestGate(ctx, rs.EffectivePolicy.TestGate, runner)
	if err != nil {
		return transport.TestOutcomeResult{}, err
	}
	sum := sha256.Sum256(evidence)
	return rn.SubmitTestOutcome(ctx, pass, hex.EncodeToString(sum[:]), rs.Revision)
}

// runTestGate produces the (pass, evidence) for a run's test gate. A disabled gate needs no
// runner; an enabled gate requires one.
func runTestGate(ctx context.Context, gate config.TestGate, runner TestRunner) (bool, []byte, error) {
	if gate.Disabled {
		return true, []byte("test-gate-disabled"), nil
	}
	if runner == nil {
		return false, nil, fmt.Errorf("coordinator: an enabled test gate requires a runner")
	}
	return runner.RunTests(ctx, gate.Command)
}

// precomputeTestOutcome returns a TestPrepare whose guarded work is pure (evaluate the
// ownerless outcome against the LOCKED snapshot, choose the required id, bind the apply). A
// PASS enters ownerless VERIFY (IDNone): it mints NOTHING and reads no RNG. A FAIL may issue
// an identity (the lead's FIX turn or the test-budget gate), so it pre-mints both candidates
// OFF the guard (mirroring the agent-submit precompute) — but ONLY when the optimistic
// snapshot is a live TESTS phase at the expected revision, the exact precondition under which
// the primitive calls this prepare. A doomed request (wrong phase, stale round) thus reads no
// RNG; the primitive re-validates under the guard, and a round that advances between here and
// the lock is stale-rejected BEFORE prepare runs (so a pre-minted candidate is never bound to
// the wrong round). No I/O, no minting, no ledger mutation under the guard.
func (rn *Run) precomputeTestOutcome(pass bool, expectedRevision uint64) (transport.TestPrepare, error) {
	var turnCand, gateCand string
	if !pass {
		rs, ok, err := rn.state.Load()
		if err != nil {
			return nil, err
		}
		if ok && rs.Phase == state.PhaseTests && rs.Revision == expectedRevision {
			if turnCand, gateCand, err = rn.mintPair(takenSet(rs)); err != nil {
				return nil, err
			}
		}
	}
	prepare := func(snapshot state.RunState, prepared transport.PreparedTestOutcome) (transport.PreparedTransition, error) {
		ev := engine.Event{Kind: engine.EvTestsOutcome, Source: prepared.Source, Pass: pass}
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
			if turnCand == "" { // the optimistic gate must have pre-minted for a live fail
				return transport.PreparedTransition{}, fmt.Errorf("coordinator: a fail outcome reached prepare without a pre-minted turn")
			}
			ids.AssignmentTurnID, issuedTurn = turnCand, turnCand
		case engine.IDGate:
			if gateCand == "" {
				return transport.PreparedTransition{}, fmt.Errorf("coordinator: a gate outcome reached prepare without a pre-minted gate")
			}
			ids.GateID, issuedGate = gateCand, gateCand
		case engine.IDNone:
			// Ownerless TESTS pass -> VERIFY: no identity, and no candidate was minted.
		default:
			return transport.PreparedTransition{}, fmt.Errorf("coordinator: unknown id kind %d", idKind)
		}
		apply := func(gen uint64, next *state.RunState) error {
			return engine.Apply(dec, prepared.Source, ids, gen, next)
		}
		return transport.NewPreparedTransition(issuedTurn, issuedGate, apply), nil
	}
	return prepare, nil
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
	// candidates against the optimistic snapshot. A plan-review submit needs the candidate
	// planning refs; a VERIFY submit needs the frozen task-contract snapshot (Project
	// re-verifies its digest under the guard).
	phaseNeedsFacts := rs.Phase == state.PhasePlanCritique || rs.Phase == state.PhasePlanRevise
	var facts engine.ProjectionFacts
	var refs engine.CandidateRefs
	if phaseNeedsFacts {
		facts, refs, err = rn.loadFacts(rs)
		if err != nil {
			return nil, err
		}
	}
	if rs.Phase == state.PhaseVerify {
		if facts.Task, err = rn.loadTaskFacts(rs); err != nil {
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

// loadTaskFacts reconstructs the frozen task-contract snapshot a VERIFY submit is projected
// against, reading it from the run directory confined via os.Root so a forged relative path
// cannot escape the run. Project re-verifies the snapshot digest under the guard, so this
// loader only asserts presence and bounds the size (never trusts the bytes on its own).
func (rn *Run) loadTaskFacts(snapshot state.RunState) (engine.TaskFacts, error) {
	if snapshot.TaskSnapshot.RelPath == "" {
		return engine.TaskFacts{}, fmt.Errorf("%w: run has no task snapshot", ErrEvidence)
	}
	root, err := os.OpenRoot(rn.loc.RunDir)
	if err != nil {
		return engine.TaskFacts{}, err
	}
	defer root.Close()
	f, err := root.Open(snapshot.TaskSnapshot.RelPath)
	if err != nil {
		return engine.TaskFacts{}, fmt.Errorf("%w: task snapshot: %v", ErrEvidence, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(engine.MaxMaterializeBytes)+1))
	if err != nil {
		return engine.TaskFacts{}, fmt.Errorf("%w: task snapshot: %v", ErrEvidence, err)
	}
	if len(data) > engine.MaxMaterializeBytes {
		return engine.TaskFacts{}, fmt.Errorf("%w: task snapshot over %d bytes", engine.ErrHistoryTooLarge, engine.MaxMaterializeBytes)
	}
	return engine.TaskFacts{SnapshotBytes: string(data)}, nil
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
