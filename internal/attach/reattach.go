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
	// ReattachMismatch: the session is known (current OR replaced) but the presented
	// agent/role disagree with its slot.
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

// reattachReader is the injectable set of lock-free reads reattach brackets, so a
// test can deterministically vary the CurrentRun / attach-journal head across the
// bracketing reads without a mutable global hook. current() is called for the
// leading and trailing CurrentRun snapshots; journalHead() for the leading and
// trailing attach-journal heads; registry() once, between them.
type reattachReader struct {
	current     func() (state.CurrentRun, bool, error)
	journalHead func() (txn.Record, bool, error)
	registry    func() (state.Registry, bool, error)
}

func realReattachReader(lay layout, runDir string) reattachReader {
	return reattachReader{
		current: state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load,
		journalHead: func() (txn.Record, bool, error) {
			return txn.Open(lay.attachJournalDir(runDir), runLock(runDir)).Latest()
		},
		registry: state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load,
	}
}

// Reattach resolves an existing session against the run — genuinely READ-ONLY and
// LOCK-FREE (no store append, journal recovery, id mint, or directory/lock-file
// creation), because lead reattach can precede the run lock's very existence.
func Reattach(req ReattachRequest) (ReattachResult, error) {
	if err := req.validate(); err != nil {
		return ReattachResult{}, err
	}
	lay := layoutFor(req.RepoDir)
	runDir := lay.runDir(state.RunDirRelFor(req.RunID))
	return reattachWith(req, lay, realReattachReader(lay, runDir))
}

// reattachWith runs the bracketed stable-snapshot protocol: read CurrentRun and
// the attach-journal head, snapshot the Registry, then re-read both — requiring an
// UNCHANGED CurrentRun (revision + identity) AND an unchanged journal head to
// bracket the Registry read across BOTH authorities. A run switch or a journal
// head change (even missing->complete) is stale/retry; a nonterminal head is the
// activation barrier (recovery-required); only then is the Registry snapshot
// resolved.
func reattachWith(req ReattachRequest, lay layout, r reattachReader) (ReattachResult, error) {
	cur1, ok, err := r.current()
	if err != nil {
		return ReattachResult{}, err
	}
	if !ok {
		return ReattachResult{Status: ReattachStale, RunID: req.RunID}, nil
	}
	// Authorize cur1 against its exact bootstrap allocation (repo-level, lock-free).
	if berr := bindRunToBootstrap(lay, cur1, req.RunID); berr != nil {
		if errors.Is(berr, ErrJoinUnauthorized) {
			return ReattachResult{Status: ReattachStale, RunID: req.RunID}, nil
		}
		return ReattachResult{}, berr
	}

	j1, j1ok, err := r.journalHead()
	if err != nil {
		return ReattachResult{}, err
	}
	reg, regok, err := r.registry()
	if err != nil {
		return ReattachResult{}, err
	}
	j2, j2ok, err := r.journalHead()
	if err != nil {
		return ReattachResult{}, err
	}
	cur2, cur2ok, err := r.current()
	if err != nil {
		return ReattachResult{}, err
	}

	// A run switch (CurrentRun revision/identity changed) invalidates everything.
	if !cur2ok || cur2 != cur1 {
		return ReattachResult{Status: ReattachStale, RunID: req.RunID}, nil
	}
	// A pending run attach transaction in EITHER bracket is the activation barrier —
	// checked before the changed-head test, so a terminal->pending change reports
	// recovery-required (never current), not merely stale.
	if (j1ok && !j1.Terminal()) || (j2ok && !j2.Terminal()) {
		return ReattachResult{Status: ReattachRecoveryRequired, RunID: req.RunID}, nil
	}
	// A changed (but terminal/missing) journal head — including missing->complete —
	// is stale/retry: the Registry snapshot was not bracketed by a single head.
	if j1ok != j2ok || (j1ok && j1.Revision != j2.Revision) {
		return ReattachResult{Status: ReattachStale, RunID: req.RunID}, nil
	}
	if !regok || reg.RunID != req.RunID {
		return ReattachResult{Status: ReattachStale, RunID: req.RunID}, nil
	}

	// Resolve the presented session against the bracketed Registry snapshot. For any
	// KNOWN session, the presented agent/role must match FIRST; then classify.
	res := reg.Resolve(req.SessionID)
	if res.Status == state.RegUnknown {
		return ReattachResult{Status: ReattachUnknown, RunID: req.RunID}, nil
	}
	if res.Role != req.Role || res.Agent != req.Agent {
		return ReattachResult{Status: ReattachMismatch, RunID: req.RunID}, nil
	}
	if res.Status == state.RegReplaced {
		// No credential; only the current generation for remediation.
		return ReattachResult{Status: ReattachReplaced, RunID: req.RunID, Role: res.Role, Agent: res.Agent, CurrentGeneration: res.CurrentGeneration}, nil
	}
	return ReattachResult{Status: ReattachCurrent, RunID: req.RunID, SessionID: req.SessionID, Role: res.Role, Agent: res.Agent, CurrentGeneration: res.CurrentGeneration}, nil
}
