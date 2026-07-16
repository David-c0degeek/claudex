package attach

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// ReattachStatus is the typed outcome of a reattach.
type ReattachStatus string

const (
	// ReattachCurrent: the presented session is the live session for its slot.
	ReattachCurrent ReattachStatus = "current"
	// ReattachReplaced: the session was superseded; CurrentGeneration is the live
	// generation for remediation, but no replacement credential is returned.
	ReattachReplaced ReattachStatus = "replaced"
	// ReattachUnknown: the session was never registered for this run.
	ReattachUnknown ReattachStatus = "unknown"
	// ReattachMismatch: the session is current but the agent/role disagree.
	ReattachMismatch ReattachStatus = "agent-role-mismatch"
	// ReattachStale: the run is not the active run, or the snapshot changed under
	// the read (retry).
	ReattachStale ReattachStatus = "stale"
	// ReattachRecoveryRequired: a run attach transaction is pending; the run is not
	// yet coherently attached, and reattach (read-only) will not drive it.
	ReattachRecoveryRequired ReattachStatus = "recovery-required"
)

// ReattachRequest presents an existing session's exact identity.
type ReattachRequest struct {
	RepoDir   string
	RunID     string
	SessionID string
	Agent     state.Agent
	Role      state.SlotRole
}

// ReattachResult is the typed outcome. SessionID is echoed only on Current.
type ReattachResult struct {
	Status            ReattachStatus
	RunID             string
	SessionID         string
	Role              state.SlotRole
	Agent             state.Agent
	CurrentGeneration uint64
}

func (req ReattachRequest) validate() error {
	if req.RepoDir == "" || !state.IsRunID(req.RunID) {
		return fmt.Errorf("attach: repo dir and a canonical run id are required")
	}
	if !state.IsSessionID(req.SessionID) {
		return fmt.Errorf("attach: a minted session id is required")
	}
	if req.Agent != state.AgentClaude && req.Agent != state.AgentCodex {
		return fmt.Errorf("attach: agent must be claude or codex")
	}
	if req.Role != state.SlotLead && req.Role != state.SlotPair {
		return fmt.Errorf("attach: role must be lead or pair")
	}
	return nil
}

// Reattach resolves an existing session against the run — genuinely READ-ONLY and
// LOCK-FREE (no store append, journal recovery, id mint, or directory/lock-file
// creation), because lead reattach can precede the run lock's very existence.
//
// It uses a bracketed stable-snapshot protocol: authorize the exact active run
// (CurrentRun bound to its bootstrap allocation), snapshot the Registry, then
// re-read CurrentRun and require the identical generation — an unchanged
// CurrentRun brackets the Registry read, and a run switch changes its revision or
// identity and is reported stale. A pending run attach journal is an activation
// barrier: the run is not yet coherently attached, so reattach reports
// recovery-required rather than driving it.
func Reattach(req ReattachRequest) (ReattachResult, error) {
	if err := req.validate(); err != nil {
		return ReattachResult{}, err
	}
	lay := layoutFor(req.RepoDir)
	runDir := lay.runDir(state.RunDirRelFor(req.RunID))

	// Authorize the exact active run (lock-free; the same exact bootstrap binding
	// join uses). A non-active/unbound run is stale, not an error.
	cur1, err := authorizeJoin(lay, req.RunID)
	if err != nil {
		if errors.Is(err, ErrJoinUnauthorized) {
			return ReattachResult{Status: ReattachStale, RunID: req.RunID}, nil
		}
		return ReattachResult{}, err
	}

	// A pending attach journal means the pair transaction has not completed — a pair
	// slot may be visible after registry-pair-fill but before PLAN_DRAFT/journal
	// completion. Reattach stays read-only and reports recovery-required (the simple
	// documented rule: any pending run attach journal blocks reattach).
	if pending, perr := attachJournalPending(lay, runDir); perr != nil {
		return ReattachResult{}, perr
	} else if pending {
		return ReattachResult{Status: ReattachRecoveryRequired, RunID: req.RunID}, nil
	}

	// Registry snapshot (one immutable generation — a coherent point-in-time read).
	reg, ok, err := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load()
	if err != nil {
		return ReattachResult{}, err
	}
	if !ok || reg.RunID != req.RunID {
		return ReattachResult{Status: ReattachStale, RunID: req.RunID}, nil
	}

	// Re-read CurrentRun; an unchanged generation brackets the Registry read.
	cur2, ok, err := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if err != nil {
		return ReattachResult{}, err
	}
	if !ok || cur2 != cur1 {
		return ReattachResult{Status: ReattachStale, RunID: req.RunID}, nil
	}

	// Resolve the presented session against the bracketed Registry snapshot.
	r := reg.Resolve(req.SessionID)
	switch r.Status {
	case state.RegUnknown:
		return ReattachResult{Status: ReattachUnknown, RunID: req.RunID}, nil
	case state.RegReplaced:
		// No credential; only the current generation for remediation.
		return ReattachResult{Status: ReattachReplaced, RunID: req.RunID, Role: r.Role, Agent: r.Agent, CurrentGeneration: r.CurrentGeneration}, nil
	default: // RegCurrent
		if r.Role != req.Role || r.Agent != req.Agent {
			return ReattachResult{Status: ReattachMismatch, RunID: req.RunID}, nil
		}
		return ReattachResult{Status: ReattachCurrent, RunID: req.RunID, SessionID: req.SessionID, Role: r.Role, Agent: r.Agent, CurrentGeneration: r.CurrentGeneration}, nil
	}
}

// attachJournalPending reports whether the run's attach journal holds a
// non-terminal transaction (lock-free; a missing journal is not pending).
func attachJournalPending(lay layout, runDir string) (bool, error) {
	rec, ok, err := txn.Open(lay.attachJournalDir(runDir), runLock(runDir)).Latest()
	if err != nil {
		return false, err
	}
	return ok && !rec.Terminal(), nil
}
