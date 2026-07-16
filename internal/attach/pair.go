package attach

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// ErrJoinUnauthorized means the run to join is not the exact active, fully
// bootstrapped run under the repository authority.
var ErrJoinUnauthorized = errors.New("attach: run is not joinable")

// ErrPairFilled means the pair slot is already registered by a different
// operation (reattach with the session id, or this is a role conflict).
var ErrPairFilled = errors.New("attach: pair role is already registered")

// JoinAttachRequest is a second (pair) attach to an existing run. Agent must be
// complementary to the lead and Role must be pair. OperationID is the caller-stable
// idempotency key. Clock and RNG are injected.
type JoinAttachRequest struct {
	RepoDir     string
	RunID       string
	OperationID string
	Agent       state.Agent
	Role        state.SlotRole
	Now         int64
	RNG         io.Reader
}

// JoinAttachResult is what the pair receives: its session and the lead's first
// (PLAN_DRAFT) turn that this attach issued.
type JoinAttachResult struct {
	RunID       string
	SessionID   string
	Role        state.SlotRole
	Agent       state.Agent
	FirstTurnID string
}

func (l layout) attachJournalDir(runDir string) string { return filepath.Join(runDir, "attach") }

func (req JoinAttachRequest) validate() error {
	if req.RepoDir == "" || !state.IsRunID(req.RunID) {
		return fmt.Errorf("attach: repo dir and a canonical run id are required")
	}
	if !state.IsOperationID(req.OperationID) {
		return fmt.Errorf("attach: a minted operation_id is required")
	}
	if req.Agent != state.AgentClaude && req.Agent != state.AgentCodex {
		return fmt.Errorf("attach: agent must be claude or codex")
	}
	if req.Role != state.SlotPair {
		return fmt.Errorf("attach: join is for the pair role")
	}
	if req.Now <= 0 || req.RNG == nil {
		return fmt.Errorf("attach: a clock and RNG are required")
	}
	return nil
}

// JoinAttach fills the pair slot and advances INIT -> PLAN_DRAFT, issuing the
// lead's first turn — one run-guarded transaction. It closes the repo->run
// authorization race: it validates the exact active run under the repo guard,
// takes the run guard in the fixed repo->run order, then releases the repo guard.
func JoinAttach(req JoinAttachRequest) (JoinAttachResult, error) {
	if err := req.validate(); err != nil {
		return JoinAttachResult{}, err
	}
	lay := layoutFor(req.RepoDir)
	runDir := lay.runDir(state.RunDirRelFor(req.RunID))

	repoGuard, ok, err := genstore.Acquire(lay.repoLock)
	if err != nil {
		return JoinAttachResult{}, err
	}
	if !ok {
		return JoinAttachResult{}, genstore.ErrBusy
	}
	if err := authorizeJoin(lay, req.RunID); err != nil {
		repoGuard.Release()
		return JoinAttachResult{}, err
	}
	runGuard, ok, err := genstore.Acquire(runLock(runDir))
	if err != nil {
		repoGuard.Release()
		return JoinAttachResult{}, err
	}
	if !ok {
		repoGuard.Release()
		return JoinAttachResult{}, genstore.ErrBusy
	}
	if rerr := repoGuard.Release(); rerr != nil {
		runGuard.Release()
		return JoinAttachResult{}, rerr
	}
	defer runGuard.Release()

	journal := txn.Open(lay.attachJournalDir(runDir), runLock(runDir))

	// Recover the run-scoped attach journal BEFORE classifying the request.
	rec, recovered, err := journal.Recover(runGuard, func(in txn.Intent) (txn.Plan, error) {
		return pairPlanFor(lay, runGuard, in)
	})
	if err != nil {
		return JoinAttachResult{}, err
	}
	if recovered {
		pin, derr := decodePairIntent(rec.Intent.Payload)
		if derr != nil {
			return JoinAttachResult{}, derr
		}
		if pin.OperationID != req.OperationID {
			return JoinAttachResult{}, fmt.Errorf("%w: %s", ErrPairFilled, pin.RunID)
		}
		return samePairResult(lay, journal, req.RunID, req.OperationID)
	}

	registry := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir))
	reg, ok, err := registry.Load()
	if err != nil {
		return JoinAttachResult{}, err
	}
	if !ok || reg.RunID != req.RunID || reg.Lead == nil {
		return JoinAttachResult{}, fmt.Errorf("%w: registry", ErrJoinUnauthorized)
	}
	if reg.Pair != nil {
		// Already filled: only the exact completed operation retry returns the pair
		// session; anything else is a conflict (reattach must present its session id).
		return samePairResult(lay, journal, req.RunID, req.OperationID)
	}

	runState := state.Open(filepath.Join(runDir, "state"), runLock(runDir))
	rs, ok, err := runState.Load()
	if err != nil {
		return JoinAttachResult{}, err
	}
	if !ok || rs.RunID != req.RunID || rs.Phase != state.PhaseInit {
		return JoinAttachResult{}, fmt.Errorf("%w: state not awaiting a pair", ErrJoinUnauthorized)
	}
	if req.Agent != complementaryAgent(reg.Lead.Agent) {
		return JoinAttachResult{}, fmt.Errorf("%w: pair agent must be complementary to the lead", ErrPairFilled)
	}

	intent, err := preparePair(req, reg, rs)
	if err != nil {
		return JoinAttachResult{}, err
	}
	plan, err := pairPlanFor(lay, runGuard, txn.Intent{
		Version: txn.IntentVersion, Kind: pairIntentKind, TxnID: intent.TxnID,
		ExpectedStateRevision: intent.ExpectedStateRevision, Payload: mustMarshalPair(intent),
	})
	if err != nil {
		return JoinAttachResult{}, err
	}
	if _, err := journal.Run(runGuard, plan); err != nil {
		return JoinAttachResult{}, err
	}
	return pairResult(intent), nil
}

// authorizeJoin confirms, under the repo guard, that runID is the EXACT active run:
// the active-run pointer names it, the catalog holds its ref, and its bootstrap
// journal is COMPLETE — catalog presence alone never authorizes a join.
func authorizeJoin(lay layout, runID string) error {
	cur, ok, err := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if err != nil {
		return err
	}
	if !ok || !cur.Active || cur.RunID != runID {
		return fmt.Errorf("%w: not the active run", ErrJoinUnauthorized)
	}
	cat, ok, err := state.OpenCatalog(lay.catalogDir, lay.repoLock).Load()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no catalog", ErrJoinUnauthorized)
	}
	if _, found := cat.Lookup(runID); !found {
		return fmt.Errorf("%w: not in catalog", ErrJoinUnauthorized)
	}
	rec, ok, err := txn.Open(lay.bootstrapJournal, lay.repoLock).Latest()
	if err != nil {
		return err
	}
	if !ok || !rec.Complete || rec.Aborted || rec.Intent.Kind != intentKind {
		return fmt.Errorf("%w: bootstrap not complete", ErrJoinUnauthorized)
	}
	bi, err := decodeIntent(rec.Intent.Payload)
	if err != nil {
		return err
	}
	if bi.RunID != runID {
		return fmt.Errorf("%w: bootstrap run mismatch", ErrJoinUnauthorized)
	}
	return nil
}

// preparePair freezes the pair-attach intent read-only under the run guard.
func preparePair(req JoinAttachRequest, reg state.Registry, rs state.RunState) (PairAttachIntent, error) {
	taken := func(id string) bool { return reg.Resolve(id).Status != state.RegUnknown }
	pairSession, err := state.MintSessionID(req.RNG, taken)
	if err != nil {
		return PairAttachIntent{}, err
	}
	txnID, err := mintID("pair-", req.RNG)
	if err != nil {
		return PairAttachIntent{}, err
	}
	firstTurn, err := mintID("turn-", req.RNG)
	if err != nil {
		return PairAttachIntent{}, err
	}
	in := PairAttachIntent{
		RunID:                    req.RunID,
		TxnID:                    txnID,
		OperationID:              req.OperationID,
		PairSessionID:            pairSession,
		PairAgent:                req.Agent,
		FirstTurnID:              firstTurn,
		ExpectedRegistryRevision: reg.Revision,
		ExpectedStateRevision:    rs.Revision,
		StartedUnix:              req.Now,
		DeadlineUnix:             req.Now + rs.EffectivePolicy.Limits.MaxWallSeconds,
	}
	if err := in.validate(); err != nil {
		return PairAttachIntent{}, err
	}
	return in, nil
}

func pairResult(in PairAttachIntent) JoinAttachResult {
	return JoinAttachResult{
		RunID:       in.RunID,
		SessionID:   in.PairSessionID,
		Role:        state.SlotPair,
		Agent:       in.PairAgent,
		FirstTurnID: in.FirstTurnID,
	}
}

// samePairResult is the combined, identity-bound idempotent return: it binds the
// COMPLETE pair-attach journal, the Registry pair slot (still current), and the
// run to one identity before returning the pair session — failing closed on any
// mismatch or replacement.
func samePairResult(lay layout, journal *txn.Journal, runID, expectedOp string) (JoinAttachResult, error) {
	rec, ok, err := journal.Latest()
	if err != nil {
		return JoinAttachResult{}, err
	}
	if !ok || !rec.Complete || rec.Aborted ||
		rec.Intent.Version != txn.IntentVersion || rec.Intent.Kind != pairIntentKind {
		return JoinAttachResult{}, fmt.Errorf("%w: %s", ErrPairFilled, runID)
	}
	in, err := decodePairIntent(rec.Intent.Payload)
	if err != nil {
		return JoinAttachResult{}, err
	}
	// Only the exact completed operation may reclaim the pair session.
	if rec.TxnID() != in.TxnID || in.RunID != runID || in.OperationID != expectedOp {
		return JoinAttachResult{}, fmt.Errorf("%w: %s", ErrPairFilled, runID)
	}
	runDir := lay.runDir(state.RunDirRelFor(in.RunID))
	reg, ok, err := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load()
	if err != nil {
		return JoinAttachResult{}, err
	}
	if !ok || reg.RunID != in.RunID {
		return JoinAttachResult{}, fmt.Errorf("%w: %s", ErrPairFilled, runID)
	}
	r := reg.Resolve(in.PairSessionID)
	if r.Status != state.RegCurrent || r.Role != state.SlotPair || r.Agent != in.PairAgent {
		return JoinAttachResult{}, fmt.Errorf("%w: %s", ErrPairFilled, runID)
	}
	return pairResult(in), nil
}

func mustMarshalPair(in PairAttachIntent) []byte {
	b, _ := in.marshal()
	return b
}

// pairIntentKind is the txn kind for a pair attach (registry pair-fill + the
// INIT->PLAN_DRAFT transition that issues the lead's first turn).
const pairIntentKind = "pair-attach"

// PairAttachIntent is the deterministic, bounded target identity of a second
// (pair) attach, frozen read-only under the run guard before the run-scoped
// journal. Every id/revision/timestamp is fixed up front, so the participants
// mint no identity during Apply and recovery is exact.
type PairAttachIntent struct {
	RunID         string      `json:"run_id"`
	TxnID         string      `json:"txn_id"`
	OperationID   string      `json:"operation_id"`    // caller-stable pair op id
	PairSessionID string      `json:"pair_session_id"` // the pair's minted session
	PairAgent     state.Agent `json:"pair_agent"`      // complementary to the lead
	FirstTurnID   string      `json:"first_turn_id"`   // the lead's first (PLAN_DRAFT) turn

	ExpectedRegistryRevision uint64 `json:"expected_registry_revision"`
	ExpectedStateRevision    uint64 `json:"expected_state_revision"`
	StartedUnix              int64  `json:"started_unix"`
	DeadlineUnix             int64  `json:"deadline_unix"`
}

func (in PairAttachIntent) marshal() (json.RawMessage, error) { return json.Marshal(in) }

func decodePairIntent(payload json.RawMessage) (PairAttachIntent, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var in PairAttachIntent
	if err := dec.Decode(&in); err != nil {
		return PairAttachIntent{}, fmt.Errorf("attach: decode pair intent: %w", err)
	}
	if err := in.validate(); err != nil {
		return PairAttachIntent{}, err
	}
	return in, nil
}

func (in PairAttachIntent) validate() error {
	if !state.IsRunID(in.RunID) || !state.IsRunID(in.TxnID) || !state.IsRunID(in.FirstTurnID) {
		return fmt.Errorf("attach: pair intent run/txn/turn id is not canonical")
	}
	if !state.IsOperationID(in.OperationID) {
		return fmt.Errorf("attach: pair intent operation_id is not a minted operation id")
	}
	if !state.IsSessionID(in.PairSessionID) {
		return fmt.Errorf("attach: pair intent session_id is not a canonical minted id")
	}
	if in.PairAgent != state.AgentClaude && in.PairAgent != state.AgentCodex {
		return fmt.Errorf("attach: pair intent agent is unknown")
	}
	if in.ExpectedRegistryRevision == 0 || in.ExpectedStateRevision == 0 {
		return fmt.Errorf("attach: pair intent expected revisions must be positive")
	}
	if in.StartedUnix <= 0 || in.DeadlineUnix <= in.StartedUnix {
		return fmt.Errorf("attach: pair intent times are incoherent")
	}
	return nil
}
