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
	RNG                io.Reader
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
// (no lock-free TOCTOU), then under the run guard requires a consistent attach shape
// (a pristine pre-pair lead-only run, or a completed pairing with landed effects) and
// the exact filled slot at expected_generation, and appends ONE new session to the
// Registry — the sole durable effect, leaving RunState untouched. Identities are
// pre-minted off the guards. Both guards are released exactly once, reverse order,
// with joined errors on every path.
func replaceAttach(req ReplaceRequest, seams replaceSeams) (ReplaceResult, error) {
	if err := req.validate(); err != nil {
		return ReplaceResult{}, err
	}
	lay := layoutFor(req.RepoDir)
	loc := runLocationFor(lay, req.RunID)
	registry := state.OpenRegistry(loc.RegistryDir, loc.RunLock)

	// Pre-mint OFF the guards from an optimistic complete taken-session set.
	newSess, err := mintReplacementSession(registry, req.RNG)
	if err != nil {
		return ReplaceResult{}, err
	}

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
	// Recheck the pre-minted id against the LOCKED registry before the mutation.
	if !state.IsSessionID(newSess) || reg.Resolve(newSess).Status != state.RegUnknown {
		return ReplaceResult{}, errors.Join(fmt.Errorf("attach: minted session id is not usable under the lock"), releaseAll())
	}

	// The shape is stable and the run guard is held; the mutation is a per-run store
	// effect, so the repo guard is no longer needed.
	if err := releaseRepo(); err != nil {
		return ReplaceResult{}, errors.Join(err, releaseRun())
	}

	newGen := currentGen + 1
	committed, merr := seams.mutate(registry, runGuard, reg.Revision, func(nextRev uint64, next *state.Registry) error {
		target := regSlot(*next, req.Role)
		target.Sessions = append(target.Sessions, state.SessionRecord{
			SessionID: newSess, Generation: newGen, IssuedRegistryRevision: nextRev,
		})
		target.CurrentSessionID = newSess
		return nil
	})
	candidate := ReplaceResult{RunID: req.RunID, SessionID: newSess, Role: req.Role, Agent: req.Agent, Generation: newGen}
	if merr != nil {
		if errors.Is(merr, genstore.ErrAmbiguous) {
			// The append MAY have committed; retain the identity for recovery and do NOT
			// present it as current.
			return candidate, errors.Join(fmt.Errorf("%w: %w", ErrReplaceOutcomeUnknown, merr), releaseRun())
		}
		// Proven uncommitted: nothing durable.
		return ReplaceResult{}, errors.Join(merr, releaseRun())
	}

	// Proven committed: the new session is durable. A run-guard release error is a warning.
	if relErr := releaseRun(); relErr != nil {
		candidate.CommitWarning = &genstore.PostCommitError{Generation: committed.Revision, Err: relErr}
	}
	return candidate, nil
}

// mintReplacementSession pre-mints a fresh session id off the guards, avoiding every
// id in the optimistic registry (the Registry's own Resolve is the authority). The
// under-guard recheck re-validates it against the locked registry before the mutation.
func mintReplacementSession(registry *state.RegistryStore, rng io.Reader) (string, error) {
	reg, ok, err := registry.Load()
	if err != nil {
		return "", err
	}
	taken := func(id string) bool { return ok && reg.Resolve(id).Status != state.RegUnknown }
	return state.MintSessionID(rng, taken)
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
