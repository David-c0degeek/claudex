package coordinator

import (
	"errors"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// ErrReadRecoveryRequired means a lock-free read observed a nonterminal aggregate transaction
// (a pair/replacement/commit-txn journal mid-flight, or a commit-txn journal that vanished after
// accepted git evidence). The run is mid-transaction, so a coherent read cannot be served — the
// caller reports/fails recovery-required rather than returning a torn cross-store view.
var ErrReadRecoveryRequired = errors.New("coordinator: run requires recovery before a read")

// ErrRunGone means the requested run is no longer the repository's active run (a switch happened
// during resolution) — an operational condition, not a torn read.
var ErrRunGone = errors.New("coordinator: requested run is not the active run")

// readCoherenceMaxAttempts bounds the bracket read-retry so a pathologically busy run cannot
// spin forever; each retry is cheap (a few lock-free store loads).
const readCoherenceMaxAttempts = 64

// journalSnap is a comparable lock-free snapshot of one transaction journal head.
type journalSnap struct {
	present  bool
	rev      uint64
	terminal bool
}

func headSnap(dir, lock string) (journalSnap, error) {
	rec, ok, err := txn.Open(dir, lock).Latest()
	if err != nil {
		return journalSnap{}, err
	}
	if !ok {
		return journalSnap{}, nil
	}
	return journalSnap{present: true, rev: rec.Revision, terminal: rec.Terminal()}, nil
}

// coherenceSnap is a comparable snapshot of the aggregate authorities read lock-free: the
// active-run pointer identity, the State and Registry generations, and all three journal heads.
// Two equal snaps taken around a read prove the read was not torn by a concurrent mutation.
type coherenceSnap struct {
	activeRunID string
	activeOK    bool
	stateRev    uint64
	stateOK     bool
	regRev      uint64
	regOK       bool
	pair        journalSnap
	repl        journalSnap
	ctxn        journalSnap
}

// readSnap captures the aggregate-authority snapshot lock-free from the resolved run location.
func readSnap(repoDir string, loc attach.RunLocation) (coherenceSnap, state.RunState, error) {
	var s coherenceSnap
	active, aok, err := attach.ActiveRunPointer(repoDir)
	if err != nil {
		return s, state.RunState{}, err
	}
	s.activeRunID, s.activeOK = active, aok

	if s.pair, err = headSnap(loc.AttachDir, loc.RunLock); err != nil {
		return s, state.RunState{}, err
	}
	if s.repl, err = headSnap(loc.ReplaceDir, loc.RunLock); err != nil {
		return s, state.RunState{}, err
	}
	if s.ctxn, err = headSnap(loc.CommitTxnDir, loc.RunLock); err != nil {
		return s, state.RunState{}, err
	}

	rs, sok, err := state.Open(loc.StateDir, loc.RunLock).Load()
	if err != nil {
		return s, state.RunState{}, err
	}
	s.stateOK, s.stateRev = sok, rs.Revision

	reg, gok, err := state.OpenRegistry(loc.RegistryDir, loc.RunLock).Load()
	if err != nil {
		return s, state.RunState{}, err
	}
	s.regOK, s.regRev = gok, reg.Revision
	return s, rs, nil
}

// recoveryRequired reports whether the snapshot shows an aggregate transaction that forbids a
// coherent read: any journal present-but-nonterminal (a pending pair/replacement/commit-txn), or
// a commit-txn journal that VANISHED after the run accepted git evidence (corruption).
func (s coherenceSnap) recoveryRequired(rs state.RunState) bool {
	if s.pair.present && !s.pair.terminal {
		return true
	}
	if s.repl.present && !s.repl.terminal {
		return true
	}
	if s.ctxn.present && !s.ctxn.terminal {
		return true
	}
	if !s.ctxn.present && s.stateOK {
		if _, has := state.LatestGitCommit(rs); has {
			return true
		}
	}
	return false
}

// readCoherent runs the bracketed lock-free read: it resolves the run (validating the active
// bootstrap binding), then, retrying on any observed movement, captures a snapshot, executes the
// caller's read against the SAME live stores, and re-captures — accepting the read only when the
// two snapshots are identical (so no concurrent mutation tore the cross-store view). A nonterminal
// aggregate transaction fails recovery-required; a run switch fails run-gone. `wait` holds no
// lock and `status` is lock-free — the bracket is the only mechanism.
func readCoherent[T any](repoDir, runID string, read func(loc attach.RunLocation) (T, error)) (T, error) {
	var zero T
	loc, err := attach.ResolveRun(repoDir, runID)
	if err != nil {
		return zero, err
	}
	for attempt := 0; attempt < readCoherenceMaxAttempts; attempt++ {
		s1, rs1, err := readSnap(repoDir, loc)
		if err != nil {
			return zero, err
		}
		if !s1.activeOK || s1.activeRunID != runID {
			return zero, ErrRunGone
		}
		if !s1.stateOK {
			return zero, transport.ErrNoRun
		}
		if s1.recoveryRequired(rs1) {
			return zero, ErrReadRecoveryRequired
		}
		out, rerr := read(loc)
		if rerr != nil {
			return zero, rerr
		}
		s2, _, err := readSnap(repoDir, loc)
		if err != nil {
			return zero, err
		}
		if s1 == s2 {
			return out, nil
		}
		// The stores moved during the read; retry with a fresh bracket.
	}
	return zero, fmt.Errorf("coordinator: read did not reach a coherent snapshot after %d attempts", readCoherenceMaxAttempts)
}

// Status returns the run's status projection over a coherent, aggregate-gated snapshot. It is
// lock-free (the bracket, never a guard) and BYO/protocol-only: the honesty labels are backed by
// the durable registration read inside the bracket.
func Status(repoDir, runID string) (transport.StatusReport, error) {
	return readCoherent(repoDir, runID, func(loc attach.RunLocation) (transport.StatusReport, error) {
		store := state.Open(loc.StateDir, loc.RunLock)
		return transport.Status(store, byoHonesty(loc))
	})
}

// byoHonesty is the production BYO/protocol-only honesty source: the tier is backed by the
// durable registration (the registry must load and belong to the run), and the capabilities are
// the fixed protocol-only set (repo-read-only unavailable under BYO; verify-fresh-session enforced
// by the durable session-generation gate). It never claims managed.
func byoHonesty(loc attach.RunLocation) transport.HonestySource {
	return func(in transport.StatusInput) (transport.HonestyLabels, error) {
		reg, ok, err := state.OpenRegistry(loc.RegistryDir, loc.RunLock).Load()
		if err != nil {
			return transport.HonestyLabels{}, err
		}
		if !ok || reg.RunID != in.RunID {
			return transport.HonestyLabels{}, fmt.Errorf("coordinator: no durable registration for the run")
		}
		return transport.HonestyLabels{
			Tier:          transport.TierProtocolOnly,
			TierMechanism: "durable-byo-registration",
			Capabilities: []transport.Capability{
				{Name: "repo-read-only", Status: "unavailable", Mechanism: "byo-attach"},
				{Name: "verify-fresh-session", Status: "enforced", Mechanism: "fresh-session-declared"},
			},
		}, nil
	}
}
