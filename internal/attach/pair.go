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

// pairIntentKind is the txn kind for a pair attach.
const pairIntentKind = "pair-attach"

// ErrJoinUnauthorized means the run to join is not the exact active, fully
// bootstrapped run under the repository authority.
var ErrJoinUnauthorized = errors.New("attach: run is not joinable")

// ErrPairFilled means the pair slot is already registered by a different
// operation (reattach must present its session id), or the request is a role/agent
// conflict.
var ErrPairFilled = errors.New("attach: pair role is already registered")

// PairAttachIntent is the deterministic, bounded target of a pair attach, frozen
// read-only under the run guard. It carries digests of the exact frozen lead slot
// and INIT run-state baseline, so a forged intent or a drifted store fails closed.
type PairAttachIntent struct {
	RunID         string      `json:"run_id"`
	TxnID         string      `json:"txn_id"`
	OperationID   string      `json:"operation_id"`
	PairSessionID string      `json:"pair_session_id"`
	PairAgent     state.Agent `json:"pair_agent"`
	FirstTurnID   string      `json:"first_turn_id"`

	ExpectedRegistryRevision uint64 `json:"expected_registry_revision"`
	ExpectedStateRevision    uint64 `json:"expected_state_revision"`
	StartedUnix              int64  `json:"started_unix"`
	DeadlineUnix             int64  `json:"deadline_unix"`

	LeadDigest      string `json:"lead_digest"`       // digest of the frozen lead slot
	BaseStateDigest string `json:"base_state_digest"` // digest of the frozen INIT run state
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
	if !state.IsHex64(in.LeadDigest) || !state.IsHex64(in.BaseStateDigest) {
		return fmt.Errorf("attach: pair intent baseline digests are not sha256")
	}
	return nil
}

// JoinAttachRequest is a second (pair) attach. Only RepoDir/RunID/OperationID are
// needed for a recovery or idempotent retry; Agent/Role/Now/RNG are required only
// for a NEW pair fill.
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

// validateMinimal is the authority a recovery/idempotent retry needs.
func (req JoinAttachRequest) validateMinimal() error {
	if req.RepoDir == "" || !state.IsRunID(req.RunID) {
		return fmt.Errorf("attach: repo dir and a canonical run id are required")
	}
	if !state.IsOperationID(req.OperationID) {
		return fmt.Errorf("attach: a minted operation_id is required")
	}
	return nil
}

// validateForNewPair is the full authority a NEW pair fill needs.
func (req JoinAttachRequest) validateForNewPair() error {
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
// lead's first turn — one run-guarded transaction. It closes the repo->run race:
// it authorizes the exact active run under the repo guard, takes the run guard in
// the fixed repo->run order, and — still holding BOTH — recovers any pending pair
// journal and revalidates the exact-current + complete-INIT shape. Only then does
// it release the repo guard; holding the run guard keeps the lifecycle from going
// terminal, so the active run cannot legitimately switch under it.
func JoinAttach(req JoinAttachRequest) (JoinAttachResult, error) {
	if err := req.validateMinimal(); err != nil {
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
	repoReleased := false
	releaseRepo := func() {
		if !repoReleased {
			repoGuard.Release()
			repoReleased = true
		}
	}
	defer releaseRepo()

	cur, err := authorizeJoin(lay, req.RunID)
	if err != nil {
		return JoinAttachResult{}, err
	}

	runGuard, ok, err := genstore.Acquire(runLock(runDir))
	if err != nil {
		return JoinAttachResult{}, err
	}
	if !ok {
		return JoinAttachResult{}, genstore.ErrBusy
	}
	defer runGuard.Release()

	journal := txn.Open(lay.attachJournalDir(runDir), runLock(runDir))

	// Recover any pending pair journal FIRST, while both guards are held.
	rec, recovered, err := journal.Recover(runGuard, func(in txn.Intent) (txn.Plan, error) {
		return pairPlanFor(lay, runGuard, req.RunID, in)
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
	runState := state.Open(filepath.Join(runDir, "state"), runLock(runDir))
	reg, ok, err := registry.Load()
	if err != nil {
		return JoinAttachResult{}, err
	}
	if !ok {
		return JoinAttachResult{}, fmt.Errorf("%w: registry", ErrJoinUnauthorized)
	}
	if reg.Pair != nil {
		// Already filled: only the exact completed operation retry returns the pair
		// session; anything else is a conflict (reattach must present its session id).
		return samePairResult(lay, journal, req.RunID, req.OperationID)
	}
	rs, ok, err := runState.Load()
	if err != nil {
		return JoinAttachResult{}, err
	}

	// A NEW pair fill requires the complete, stable joinable INIT shape (revalidated
	// under both guards) before we commit to it or release the repo guard.
	if err := requireJoinableInitShape(req.RunID, cur, reg, ok, rs); err != nil {
		return JoinAttachResult{}, err
	}
	if err := req.validateForNewPair(); err != nil {
		return JoinAttachResult{}, err
	}
	if req.Agent != complementaryAgent(reg.Lead.Agent) {
		return JoinAttachResult{}, fmt.Errorf("%w: pair agent must be complementary to the lead", ErrPairFilled)
	}

	// The shape is stable and the run guard is held; drop the repo guard.
	releaseRepo()

	intent, err := preparePair(req, reg, rs)
	if err != nil {
		return JoinAttachResult{}, err
	}
	plan, err := pairPlanFor(lay, runGuard, req.RunID, txn.Intent{
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

// authorizeJoin binds the run to ONE bootstrap allocation under the repo guard:
// the active-run pointer names it exactly, the catalog holds exactly its bootstrap
// ref, and the bootstrap journal is complete with a fully-bound envelope whose
// intent identity equals the pointer. Catalog presence alone is history.
func authorizeJoin(lay layout, runID string) (state.CurrentRun, error) {
	cur, ok, err := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if err != nil {
		return state.CurrentRun{}, err
	}
	if !ok {
		return state.CurrentRun{}, fmt.Errorf("%w: no active run", ErrJoinUnauthorized)
	}
	if err := bindRunToBootstrap(lay, cur, runID); err != nil {
		return state.CurrentRun{}, err
	}
	return cur, nil
}

// bindRunToBootstrap validates that cur is the exact active run bound to ONE
// bootstrap allocation. It reads only the repo-level catalog/bootstrap journal
// (lock-free), so it can authorize a caller-supplied CurrentRun snapshot too.
func bindRunToBootstrap(lay layout, cur state.CurrentRun, runID string) error {
	if !cur.Active || cur.RunID != runID || cur.RelDir != state.RunDirRelFor(runID) {
		return fmt.Errorf("%w: not the active run", ErrJoinUnauthorized)
	}
	rec, ok, err := txn.Open(lay.bootstrapJournal, lay.repoLock).Latest()
	if err != nil {
		return err
	}
	if err != nil {
		return err
	}
	if !ok || !rec.Complete || rec.Aborted ||
		rec.Intent.Version != txn.IntentVersion || rec.Intent.Kind != intentKind || rec.Intent.ExpectedStateRevision != 0 {
		return fmt.Errorf("%w: bootstrap not complete", ErrJoinUnauthorized)
	}
	bi, err := decodeIntent(rec.Intent.Payload)
	if err != nil {
		return err
	}
	if rec.TxnID() != bi.TxnID || bi.RunID != cur.RunID || bi.RelDir != cur.RelDir || bi.OperationID != cur.OperationID {
		return fmt.Errorf("%w: bootstrap identity mismatch", ErrJoinUnauthorized)
	}
	cat, ok, err := state.OpenCatalog(lay.catalogDir, lay.repoLock).Load()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no catalog", ErrJoinUnauthorized)
	}
	ref, found := cat.Lookup(runID)
	if !found || ref != bi.runRef() {
		return fmt.Errorf("%w: catalog ref mismatch", ErrJoinUnauthorized)
	}
	return nil
}

// requireJoinableInitShape enforces the complete, stable initial shape a new pair
// fill needs: the active-run pointer still names this run, the registry is
// lead-only for it, and the run state is a pristine INIT (running, no
// assignment/start/deadline/gate/recovery/failure/pending/accepted).
func requireJoinableInitShape(runID string, cur state.CurrentRun, reg state.Registry, stateOK bool, rs state.RunState) error {
	if !cur.Active || cur.RunID != runID {
		return fmt.Errorf("%w: active run changed", ErrJoinUnauthorized)
	}
	if reg.RunID != runID || reg.Lead == nil || reg.Pair != nil {
		return fmt.Errorf("%w: registry is not lead-only for the run", ErrJoinUnauthorized)
	}
	if !stateOK || rs.RunID != runID {
		return fmt.Errorf("%w: run state missing", ErrJoinUnauthorized)
	}
	if rs.Revision != 1 || rs.Lifecycle != state.LifecycleRunning || rs.Phase != state.PhaseInit ||
		rs.Assignment != nil || rs.Gate != nil || rs.Recovery != nil || rs.Failure != nil ||
		rs.StartedUnix != 0 || rs.DeadlineUnix != 0 || rs.PendingTxnID != "" || len(rs.AcceptedTurns) != 0 {
		return fmt.Errorf("%w: run is not a pristine INIT awaiting a pair", ErrJoinUnauthorized)
	}
	// First attach creates run state at revision 1 with zero budgets; any drift means
	// a stray pre-pair mutation that must not be silently frozen as the baseline.
	c := rs.Counters
	if c.PlanRevisions != 0 || c.TestFixes != 0 || c.VerifyFixes != 0 || len(c.StepFixes) != 0 {
		return fmt.Errorf("%w: run has non-zero budgets before pairing", ErrJoinUnauthorized)
	}
	return nil
}

// preparePair freezes the pair-attach intent read-only under the run guard,
// including digests of the exact lead slot and INIT run-state baseline.
func preparePair(req JoinAttachRequest, reg state.Registry, rs state.RunState) (PairAttachIntent, error) {
	// The clock must not precede the run's creation, or the frozen transaction would
	// be permanently rejected by state validation after Registry already filled.
	if req.Now < rs.CreatedUnix {
		return PairAttachIntent{}, fmt.Errorf("attach: clock precedes the run's creation")
	}
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
	leadDigest, err := canonDigest(reg.Lead)
	if err != nil {
		return PairAttachIntent{}, err
	}
	baseDigest, err := canonDigest(rs)
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
		LeadDigest:               leadDigest,
		BaseStateDigest:          baseDigest,
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
// COMPLETE (never aborted) pair-attach journal envelope + payload, requires the
// operation to match, the Registry pair still current, and — before returning a
// result containing the first turn id — that the run state proves the first turn
// was actually issued/accepted. Any mismatch fails closed.
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
	if rec.TxnID() != in.TxnID || rec.Intent.ExpectedStateRevision != in.ExpectedStateRevision ||
		in.RunID != runID || in.OperationID != expectedOp {
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
	if r := reg.Resolve(in.PairSessionID); r.Status != state.RegCurrent || r.Role != state.SlotPair || r.Agent != in.PairAgent {
		return JoinAttachResult{}, fmt.Errorf("%w: %s", ErrPairFilled, runID)
	}
	rs, ok, err := state.Open(filepath.Join(runDir, "state"), runLock(runDir)).Load()
	if err != nil {
		return JoinAttachResult{}, err
	}
	issued, derr := planDraftApplied(rs, in)
	if derr != nil {
		return JoinAttachResult{}, derr
	}
	if !ok || !issued {
		return JoinAttachResult{}, fmt.Errorf("%w: %s", ErrPairFilled, runID)
	}
	return pairResult(in), nil
}

func mustMarshalPair(in PairAttachIntent) []byte {
	b, _ := in.marshal()
	return b
}
