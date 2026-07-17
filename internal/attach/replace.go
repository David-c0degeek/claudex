package attach

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
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
	// ErrReplaceRecoveryRequired means the run's pair journal is not a completed,
	// identity-bound pairing (pending/aborted/vanished/corrupt), so a replacement must
	// not proceed until the run is recovered.
	ErrReplaceRecoveryRequired = errors.New("attach: run requires recovery before a replacement")
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

// ReplaceResult is the outcome: the new session id and its generation.
type ReplaceResult struct {
	RunID      string
	SessionID  string
	Role       state.SlotRole
	Agent      state.Agent
	Generation uint64
	// CommitWarning is non-nil when the Registry append committed but the run-guard
	// release failed; the new session is durable and authoritative regardless.
	CommitWarning error
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

// ReplaceAttach performs an explicit same-role session replacement. It authorizes the
// exact active bootstrap allocation under the repo guard (no lock-free TOCTOU), then
// under the run guard requires a completed pair journal and the exact filled slot at
// expected_generation, and appends ONE new session to the Registry — the sole durable
// effect, leaving RunState untouched. Identities are pre-minted off the guards.
func ReplaceAttach(req ReplaceRequest) (ReplaceResult, error) {
	if err := req.validate(); err != nil {
		return ReplaceResult{}, err
	}
	lay := layoutFor(req.RepoDir)
	runDir := lay.runDir(state.RunDirRelFor(req.RunID))
	registry := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir))

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
	repoReleased := false
	releaseRepo := func() error {
		if repoReleased {
			return nil
		}
		repoReleased = true
		return repoGuard.Release()
	}
	defer releaseRepo()

	if err := authorizeReplace(lay, req.RunID); err != nil {
		return ReplaceResult{}, err
	}

	runGuard, ok, err := genstore.Acquire(runLock(runDir))
	if err != nil {
		return ReplaceResult{}, err
	}
	if !ok {
		return ReplaceResult{}, genstore.ErrBusy
	}
	runReleased := false
	releaseRun := func() error {
		if runReleased {
			return nil
		}
		runReleased = true
		return runGuard.Release()
	}
	defer releaseRun()

	// Require a completed, identity-bound pairing under the live run guard.
	class, cerr := ClassifyPairJournal(runGuard, runLocationFor(lay, req.RunID))
	if cerr != nil {
		return ReplaceResult{}, fmt.Errorf("%w: %v", ErrReplaceRecoveryRequired, cerr)
	}
	if class != JournalTerminal {
		return ReplaceResult{}, fmt.Errorf("%w: pair journal class %d", ErrReplaceRecoveryRequired, class)
	}

	reg, ok, err := registry.Load()
	if err != nil {
		return ReplaceResult{}, err
	}
	if !ok || reg.RunID != req.RunID {
		return ReplaceResult{}, fmt.Errorf("%w: registry does not belong to the run", ErrReplaceUnauthorized)
	}
	slot := regSlot(reg, req.Role)
	if slot == nil {
		return ReplaceResult{}, fmt.Errorf("%w: %s", ErrReplaceSlotEmpty, req.Role)
	}
	if slot.Agent != req.Agent {
		return ReplaceResult{}, fmt.Errorf("%w: slot holds %s", ErrReplaceAgentMismatch, slot.Agent)
	}
	// Generations are consecutive from 1, so the current generation is the session
	// count. A stale expected generation is reported without the current credential.
	currentGen := uint64(len(slot.Sessions))
	if req.ExpectedGeneration != currentGen {
		return ReplaceResult{}, fmt.Errorf("%w: expected %d, current %d", ErrReplaceStaleGeneration, req.ExpectedGeneration, currentGen)
	}
	// Recheck the pre-minted id against the LOCKED registry before the mutation.
	if !state.IsSessionID(newSess) || sessionExists(reg, newSess) {
		return ReplaceResult{}, fmt.Errorf("attach: minted session id is not usable under the lock")
	}

	// The shape is stable and the run guard is held; the mutation is a per-run store
	// effect, so the repo guard is no longer needed.
	if err := releaseRepo(); err != nil {
		return ReplaceResult{}, errors.Join(err, releaseRun())
	}

	newGen := currentGen + 1
	committed, merr := registry.MutateLocked(runGuard, reg.Revision, func(nextRev uint64, next *state.Registry) error {
		target := regSlot(*next, req.Role)
		target.Sessions = append(target.Sessions, state.SessionRecord{
			SessionID: newSess, Generation: newGen, IssuedRegistryRevision: nextRev,
		})
		target.CurrentSessionID = newSess
		return nil
	})
	if merr != nil {
		// Nothing durable; a MutateLocked failure never left a partial write.
		return ReplaceResult{}, errors.Join(merr, releaseRun())
	}

	// Committed: the new session is durable. A run-guard release error is a warning.
	res := ReplaceResult{
		RunID: req.RunID, SessionID: newSess, Role: req.Role, Agent: req.Agent, Generation: newGen,
	}
	if relErr := releaseRun(); relErr != nil {
		res.CommitWarning = &genstore.PostCommitError{Generation: committed.Revision, Err: relErr}
	}
	return res, nil
}

// mintReplacementSession pre-mints a fresh session id off the guards, avoiding every
// id in the optimistic registry (both slot histories). The under-guard recheck
// re-validates it against the locked registry before the mutation.
func mintReplacementSession(registry *state.RegistryStore, rng io.Reader) (string, error) {
	reg, ok, err := registry.Load()
	if err != nil {
		return "", err
	}
	taken := func(id string) bool { return ok && sessionExists(reg, id) }
	return state.MintSessionID(rng, taken)
}

// authorizeReplace binds the run to the active bootstrap allocation under the repo
// guard (reusing the pair-attach authority): the active-run pointer names it, the
// catalog holds its bootstrap ref, and the bootstrap journal is complete and bound.
func authorizeReplace(lay layout, runID string) error {
	cur, ok, err := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no active run", ErrReplaceUnauthorized)
	}
	if err := bindRunToBootstrap(lay, cur, runID); err != nil {
		return fmt.Errorf("%w: %v", ErrReplaceUnauthorized, err)
	}
	return nil
}

func regSlot(reg state.Registry, role state.SlotRole) *state.RoleSlot {
	if role == state.SlotLead {
		return reg.Lead
	}
	return reg.Pair
}

// sessionExists reports whether id appears in either slot's history.
func sessionExists(reg state.Registry, id string) bool {
	for _, slot := range []*state.RoleSlot{reg.Lead, reg.Pair} {
		if slot == nil {
			continue
		}
		for _, s := range slot.Sessions {
			if s.SessionID == id {
				return true
			}
		}
	}
	return false
}
