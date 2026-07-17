package attach

import (
	"errors"
	"fmt"
	"io"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

var (
	// ErrReplaceUnauthorized means the run is not the active bootstrap allocation the
	// replacement names (no active run, a run switch, or a forged binding).
	ErrReplaceUnauthorized = errors.New("attach: replacement is not authorized for this run")
	// ErrReplaceSlotEmpty means the named role slot is not filled, so there is no
	// session to supersede.
	ErrReplaceSlotEmpty = errors.New("attach: the role slot is not filled")
	// ErrReplaceAgentMismatch means the request's agent does not match the slot's
	// immutable agent — a mid-run role/agent switch is never a replacement.
	ErrReplaceAgentMismatch = errors.New("attach: replacement agent does not match the slot (no mid-run switch)")
	// ErrReplaceStaleGeneration means expected_generation is not the slot's current
	// generation. It never reveals the current session credential.
	ErrReplaceStaleGeneration = errors.New("attach: expected generation does not match the current slot generation")
	// ErrReplaceRecoveryRequired means the run's attach effects are not a consistent,
	// completed shape (a non-terminal/corrupt/vanished pair journal, or a terminal
	// journal whose Registry/RunState effects are missing), so a replacement must not
	// proceed until the run is recovered.
	ErrReplaceRecoveryRequired = errors.New("attach: run requires recovery before a replacement")
	// ErrReplaceOutcomeUnknown means the Registry append MAY have committed but its
	// outcome could not be reconciled. The returned candidate identity is retained for
	// recovery and MUST NOT be treated as the current session until recovery resolves
	// the Registry.
	ErrReplaceOutcomeUnknown = errors.New("attach: replacement outcome is unknown; recovery required before use")
	// ErrReplaceConflict means the request reuses an operation id that a different
	// replacement (different role/agent/superseded generation) already recorded, so it
	// is not an idempotent retry.
	ErrReplaceConflict = errors.New("attach: operation id names a different replacement")
)

// ReplaceRequest is an explicit same-role session replacement: it supersedes the
// current session of a filled role slot with a new session generation, so a crashed
// or abandoned TUI can be taken over without disturbing the run's phase state. The
// trust boundary is same-user filesystem access (no authentication, no leases).
type ReplaceRequest struct {
	RepoDir            string
	RunID              string
	Role               state.SlotRole
	Agent              state.Agent
	ExpectedGeneration uint64
	// OperationID is a mandatory, caller-stable idempotency key ("op-" + 32 lower-hex).
	// Idempotency is head-scoped: while this operation is the latest journalled
	// replacement (pending or complete), a retry with the SAME id reconciles to the
	// same result without re-minting or double-applying. AFTER a later replacement
	// supersedes it, an old retry is stale, not a historical current result — the
	// journal does not scan history. Preventing global reuse of an id across distinct
	// replacements is the caller's obligation. A different id is a distinct replacement.
	OperationID string
	RNG         io.Reader
}

// ReplaceResult is the outcome: the new session id and its generation. When the error
// is ErrReplaceOutcomeUnknown the identity is a RECOVERY CANDIDATE, not a proven
// current session.
type ReplaceResult struct {
	RunID      string
	SessionID  string
	Role       state.SlotRole
	Agent      state.Agent
	Generation uint64
	// CommitWarning is non-nil ONLY when the append is PROVEN committed but the
	// run-guard release then failed; the new session is durable and authoritative.
	CommitWarning error
}

// replaceSeams are per-call injectable release/mutation points; production uses the
// real guard releases and Registry mutation. Tests inject failing releases and an
// ambiguous mutation without any global state.
type replaceSeams struct {
	releaseRepo func(*genstore.Guard) error
	releaseRun  func(*genstore.Guard) error
	mutate      func(*state.RegistryStore, *genstore.Guard, uint64, func(uint64, *state.Registry) error) (state.Registry, error)
}

func defaultReplaceSeams() replaceSeams {
	return replaceSeams{
		releaseRepo: (*genstore.Guard).Release,
		releaseRun:  (*genstore.Guard).Release,
		mutate:      (*state.RegistryStore).MutateLocked,
	}
}

func (r ReplaceRequest) validate() error {
	if !state.IsRunID(r.RunID) {
		return fmt.Errorf("%w: run id is not canonical", ErrReplaceUnauthorized)
	}
	if r.Role != state.SlotLead && r.Role != state.SlotPair {
		return fmt.Errorf("attach: replacement role must be lead or pair")
	}
	if r.Agent != state.AgentClaude && r.Agent != state.AgentCodex {
		return fmt.Errorf("attach: replacement agent must be claude or codex")
	}
	if r.ExpectedGeneration == 0 {
		return fmt.Errorf("attach: expected_generation must be > 0")
	}
	if !state.IsOperationID(r.OperationID) {
		return fmt.Errorf("attach: replacement requires a minted operation_id")
	}
	if r.RNG == nil {
		return fmt.Errorf("attach: replacement requires an RNG")
	}
	return nil
}

// ReplaceAttach performs an explicit same-role session replacement.
func ReplaceAttach(req ReplaceRequest) (ReplaceResult, error) {
	return replaceAttach(req, defaultReplaceSeams())
}

// replaceAttach authorizes the exact active bootstrap allocation under the repo guard
// (no lock-free TOCTOU), then under the run guard RECOVERS any pending replacement,
// reconciles an idempotent same-operation retry, and otherwise journals a NEW same-role
// session replacement whose sole 4c-1 effect is appending ONE new session to the
// Registry (RunState untouched). The dedicated replacement journal makes the Registry
// append crash-recoverable and backs the mandatory operation id's lost-response
// idempotency. Both guards are released exactly once, reverse order, with joined errors.
func replaceAttach(req ReplaceRequest, seams replaceSeams) (ReplaceResult, error) {
	if err := req.validate(); err != nil {
		return ReplaceResult{}, err
	}
	lay := layoutFor(req.RepoDir)
	loc := runLocationFor(lay, req.RunID)
	registry := state.OpenRegistry(loc.RegistryDir, loc.RunLock)
	journal := txn.Open(loc.ReplaceDir, loc.RunLock)

	repoGuard, ok, err := genstore.Acquire(lay.repoLock)
	if err != nil {
		return ReplaceResult{}, err
	}
	if !ok {
		return ReplaceResult{}, genstore.ErrBusy
	}
	repoDone := false
	releaseRepo := func() error {
		if repoDone {
			return nil
		}
		repoDone = true
		return seams.releaseRepo(repoGuard)
	}

	cur, err := authorizeReplace(lay, req.RunID)
	if err != nil {
		return ReplaceResult{}, errors.Join(err, releaseRepo())
	}

	runGuard, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil {
		return ReplaceResult{}, errors.Join(err, releaseRepo())
	}
	if !ok {
		return ReplaceResult{}, errors.Join(genstore.ErrBusy, releaseRepo())
	}
	runDone := false
	releaseRun := func() error {
		if runDone {
			return nil
		}
		runDone = true
		return seams.releaseRun(runGuard)
	}
	// releaseAll releases in reverse acquisition order (run, then repo).
	releaseAll := func() error { return errors.Join(releaseRun(), releaseRepo()) }

	// 1. Recover any pending replacement FIRST, before authorizing a new one. A pending
	//    operation that cannot be completed (ambiguous/indeterminate) blocks every
	//    request — a different one cannot step around it.
	if _, _, rerr := journal.Recover(runGuard, func(tin txn.Intent) (txn.Plan, error) {
		in, derr := decodeReplaceIntent(tin.Payload)
		if derr != nil {
			return txn.Plan{}, derr
		}
		return replacePlanFor(registry, runGuard, in, seams.mutate)
	}); rerr != nil {
		return ReplaceResult{}, errors.Join(fmt.Errorf("%w: %v", ErrReplaceRecoveryRequired, rerr), releaseAll())
	}

	// 2. Bind and classify the journal head. Every present head — pending or terminal —
	//    must bind to the exact replacement envelope/payload/run/steps, or it is
	//    recovery-required (never history a new operation can silently step over). A
	//    non-terminal head should not survive Recover, and an aborted head is not a
	//    completed replacement; both are recovery-required.
	head, hasHead, herr := journal.Latest()
	if herr != nil {
		return ReplaceResult{}, errors.Join(herr, releaseAll())
	}
	if hasHead {
		hin, berr := bindReplaceHead(head, req.RunID)
		if berr != nil {
			return ReplaceResult{}, errors.Join(fmt.Errorf("%w: %v", ErrReplaceRecoveryRequired, berr), releaseAll())
		}
		if !head.Complete || head.Aborted {
			return ReplaceResult{}, errors.Join(fmt.Errorf("%w: replacement head is not a completed transaction", ErrReplaceRecoveryRequired), releaseAll())
		}
		if hin.OperationID == req.OperationID {
			res, serr := sameReplaceResult(registry, req, hin)
			if serr != nil {
				return ReplaceResult{}, errors.Join(serr, releaseAll())
			}
			// Proven committed (a completed transaction): a release failure is a
			// post-commit warning over the terminal journal revision, not an op error.
			if relErr := releaseAll(); relErr != nil {
				res.CommitWarning = &genstore.PostCommitError{Generation: head.Revision, Err: relErr}
			}
			return res, nil
		}
		// A different, completed operation may be stepped over only once its durable
		// Registry effect is exactly Applied; a rolled-back or missing effect is
		// recovery-required, never silently overwritten.
		switch st, sterr := replaceStepStatus(registry, hin); {
		case sterr != nil:
			return ReplaceResult{}, errors.Join(sterr, releaseAll())
		case st != txn.StatusApplied:
			return ReplaceResult{}, errors.Join(fmt.Errorf("%w: the latest replacement's Registry effect is missing", ErrReplaceRecoveryRequired), releaseAll())
		}
	}

	// 3. A genuinely new replacement. Authorize the replaceable shape and the exact
	//    filled slot at expected_generation before minting or journaling.
	reg, err := verifyReplaceable(loc, registry, runGuard, req.RunID, cur)
	if err != nil {
		return ReplaceResult{}, errors.Join(err, releaseAll())
	}
	slot := regSlot(reg, req.Role)
	if slot == nil {
		return ReplaceResult{}, errors.Join(fmt.Errorf("%w: %s", ErrReplaceSlotEmpty, req.Role), releaseAll())
	}
	if slot.Agent != req.Agent {
		return ReplaceResult{}, errors.Join(fmt.Errorf("%w: slot holds %s", ErrReplaceAgentMismatch, slot.Agent), releaseAll())
	}
	// Generations are consecutive from 1, so the current generation is the session
	// count. A stale expected generation is reported without the current credential.
	currentGen := uint64(len(slot.Sessions))
	if req.ExpectedGeneration != currentGen {
		return ReplaceResult{}, errors.Join(fmt.Errorf("%w: expected %d, current %d", ErrReplaceStaleGeneration, req.ExpectedGeneration, currentGen), releaseAll())
	}

	// The shape is stable and the run guard is held; the replacement is a per-run store
	// effect, so the repo guard is no longer needed.
	if err := releaseRepo(); err != nil {
		return ReplaceResult{}, errors.Join(err, releaseRun())
	}

	in, err := prepareReplace(req, reg, currentGen)
	if err != nil {
		return ReplaceResult{}, errors.Join(err, releaseRun())
	}
	plan, err := replacePlanFor(registry, runGuard, in, seams.mutate)
	if err != nil {
		return ReplaceResult{}, errors.Join(err, releaseRun())
	}

	candidate := replaceCandidate(in)
	final, rerr := journal.Run(runGuard, plan)
	if rerr != nil {
		if errors.Is(rerr, genstore.ErrAmbiguous) {
			// The append MAY have committed; the pending journal retains the frozen
			// candidate for recovery. Do NOT present it as current.
			return candidate, errors.Join(fmt.Errorf("%w: %w", ErrReplaceOutcomeUnknown, rerr), releaseRun())
		}
		// Proven uncommitted, or a step left recoverably pending: nothing authoritative.
		return ReplaceResult{}, errors.Join(rerr, releaseRun())
	}

	// Proven committed: the new session is durable and authoritative.
	if relErr := releaseRun(); relErr != nil {
		candidate.CommitWarning = &genstore.PostCommitError{Generation: final.Revision, Err: relErr}
	}
	return candidate, nil
}

// prepareReplace freezes the replacement intent read-only under the run guard: it
// mints the new session id (avoiding every registered id) and a txn id, and binds the
// superseded/new generations and the Registry revision the append compares against.
func prepareReplace(req ReplaceRequest, reg state.Registry, currentGen uint64) (ReplaceIntent, error) {
	taken := func(id string) bool { return reg.Resolve(id).Status != state.RegUnknown }
	newSess, err := state.MintSessionID(req.RNG, taken)
	if err != nil {
		return ReplaceIntent{}, err
	}
	txnID, err := mintID("rpl-", req.RNG)
	if err != nil {
		return ReplaceIntent{}, err
	}
	in := ReplaceIntent{
		RunID:                    req.RunID,
		TxnID:                    txnID,
		OperationID:              req.OperationID,
		Role:                     req.Role,
		Agent:                    req.Agent,
		SupersededGeneration:     currentGen,
		NewSessionID:             newSess,
		NewGeneration:            currentGen + 1,
		ExpectedRegistryRevision: reg.Revision,
	}
	if err := in.validate(); err != nil {
		return ReplaceIntent{}, err
	}
	return in, nil
}

func replaceCandidate(in ReplaceIntent) ReplaceResult {
	return ReplaceResult{RunID: in.RunID, SessionID: in.NewSessionID, Role: in.Role, Agent: in.Agent, Generation: in.NewGeneration}
}

// sameReplaceResult is the identity-bound idempotent return for a completed
// operation: the request bindings must match the frozen intent (else the operation id
// names a different replacement), and the Registry must still show the new session
// current for the bound role/agent/generation.
func sameReplaceResult(registry *state.RegistryStore, req ReplaceRequest, in ReplaceIntent) (ReplaceResult, error) {
	if req.Role != in.Role || req.Agent != in.Agent || req.ExpectedGeneration != in.SupersededGeneration {
		return ReplaceResult{}, fmt.Errorf("%w: %s", ErrReplaceConflict, in.OperationID)
	}
	reg, ok, err := registry.Load()
	if err != nil {
		return ReplaceResult{}, err
	}
	if !ok || reg.RunID != in.RunID {
		return ReplaceResult{}, fmt.Errorf("%w: registry effect is missing", ErrReplaceRecoveryRequired)
	}
	r := reg.Resolve(in.NewSessionID)
	if r.Status != state.RegCurrent || r.Role != in.Role || r.Agent != in.Agent || r.CurrentGeneration != in.NewGeneration {
		return ReplaceResult{}, fmt.Errorf("%w: the replacement Registry effect is missing", ErrReplaceRecoveryRequired)
	}
	return replaceCandidate(in), nil
}

// authorizeReplace binds the run to the active bootstrap allocation under the repo
// guard (reusing the pair-attach authority) and returns the guarded CurrentRun.
func authorizeReplace(lay layout, runID string) (state.CurrentRun, error) {
	cur, ok, err := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if err != nil {
		return state.CurrentRun{}, err
	}
	if !ok {
		return state.CurrentRun{}, fmt.Errorf("%w: no active run", ErrReplaceUnauthorized)
	}
	if err := bindRunToBootstrap(lay, cur, runID); err != nil {
		return state.CurrentRun{}, fmt.Errorf("%w: %v", ErrReplaceUnauthorized, err)
	}
	return cur, nil
}

// verifyReplaceable enforces the two legal replaceable shapes under the run guard and
// returns the loaded Registry:
//   - a pristine pre-pair lead-only run (JournalAbsent + joinable INIT), where only the
//     lead is fillable — replacing the empty pair falls to the caller's slot check;
//   - a completed pairing (JournalTerminal) whose Registry and RunState effects landed.
//
// Every other state — non-terminal/corrupt journal, a vanished completed journal, or a
// terminal journal with rolled-back effects — is recovery-required.
func verifyReplaceable(loc RunLocation, registry *state.RegistryStore, runGuard *genstore.Guard, runID string, cur state.CurrentRun) (state.Registry, error) {
	class, cerr := ClassifyPairJournal(runGuard, loc)
	if cerr != nil {
		return state.Registry{}, fmt.Errorf("%w: %v", ErrReplaceRecoveryRequired, cerr)
	}
	reg, regOK, rerr := registry.Load()
	if rerr != nil {
		return state.Registry{}, rerr
	}
	// The run binding is already authorized under the repo guard, so a missing or
	// mismatched run Registry is an inconsistent effect, not an authorization failure.
	if !regOK || reg.RunID != runID {
		return state.Registry{}, fmt.Errorf("%w: registry effect is missing or mismatched", ErrReplaceRecoveryRequired)
	}
	rs, rsOK, srerr := state.Open(loc.StateDir, loc.RunLock).Load()
	if srerr != nil {
		return state.Registry{}, srerr
	}

	switch class {
	case JournalTerminal:
		if err := requireCompletedPairEffects(loc, reg, rsOK, rs); err != nil {
			return state.Registry{}, err
		}
		return reg, nil
	case JournalAbsent:
		// A pending pair transaction must be recovered before lead replacement (its
		// frozen lead digest cannot survive an intervening replacement); requireJoinable
		// -InitShape rejects anything but the pristine lead-only INIT, and a filled pair
		// (a vanished completed journal) fails it too.
		if err := requireJoinableInitShape(runID, cur, reg, rsOK, rs); err != nil {
			return state.Registry{}, fmt.Errorf("%w: %v", ErrReplaceRecoveryRequired, err)
		}
		return reg, nil
	default: // JournalNonTerminal or any unknown value
		return state.Registry{}, fmt.Errorf("%w: pair journal class %d", ErrReplaceRecoveryRequired, class)
	}
}

// requireCompletedPairEffects proves a terminal pair journal's durable effects landed:
// the recorded pair session is in the pair history (current or replaced) with the
// bound role/agent, and RunState.FirstTurn proves the PLAN_DRAFT issuance. A terminal
// record whose effects were rolled back is recovery-required.
func requireCompletedPairEffects(loc RunLocation, reg state.Registry, rsOK bool, rs state.RunState) error {
	rec, ok, err := txn.Open(loc.AttachDir, loc.RunLock).Latest()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReplaceRecoveryRequired, err)
	}
	if !ok || !rec.Complete || rec.Aborted {
		return fmt.Errorf("%w: pair journal is not a completed record", ErrReplaceRecoveryRequired)
	}
	in, derr := decodePairIntent(rec.Intent.Payload)
	if derr != nil {
		return fmt.Errorf("%w: %v", ErrReplaceRecoveryRequired, derr)
	}
	pr := reg.Resolve(in.PairSessionID)
	if pr.Status == state.RegUnknown || pr.Role != state.SlotPair || pr.Agent != in.PairAgent {
		return fmt.Errorf("%w: the completed pair Registry effect is missing", ErrReplaceRecoveryRequired)
	}
	if !rsOK {
		return fmt.Errorf("%w: run state is missing", ErrReplaceRecoveryRequired)
	}
	applied, aerr := planDraftApplied(rs, in)
	if aerr != nil {
		return aerr
	}
	if !applied {
		return fmt.Errorf("%w: the completed pair RunState effect is missing", ErrReplaceRecoveryRequired)
	}
	return nil
}

func regSlot(reg state.Registry, role state.SlotRole) *state.RoleSlot {
	if role == state.SlotLead {
		return reg.Lead
	}
	return reg.Pair
}
