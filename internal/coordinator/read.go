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

// journalSnap is a comparable lock-free snapshot of one transaction journal head, DOMAIN-
// classified (not generic txn terminality): `recover` is true when the head forbids a coherent
// read — a present-but-nonterminal head (which the attach classifiers deliberately include an
// ABORTED head in), or a misbound/wrong-kind/wrong-identity head (a classification error). `rev`
// is retained so the bracket detects any head change between reads.
type journalSnap struct {
	present bool
	rev     uint64
	recover bool
}

// headSnap reads a journal head lock-free and DOMAIN-classifies it via the supplied pure
// classifier, so an aborted or misbound head is recovery-required (never accepted as complete). A
// genstore read error is a real I/O error; a domain classification error folds into `recover`.
func headSnap(dir, lock string, classify func(rec txn.Record, ok bool) bool) (journalSnap, error) {
	rec, ok, err := txn.Open(dir, lock).Latest()
	if err != nil {
		return journalSnap{}, err
	}
	rev := uint64(0)
	if ok {
		rev = rec.Revision
	}
	return journalSnap{present: ok, rev: rev, recover: classify(rec, ok)}, nil
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
	// identityCorrupt is set when a PRESENT State or Registry names a run other than loc.RunID —
	// a cross-wired store that coherently passes the generation bracket but must still fail closed.
	identityCorrupt bool
	pair            journalSnap
	repl            journalSnap
	ctxn            journalSnap
}

// readSnap captures the aggregate-authority snapshot lock-free from the resolved run location.
func readSnap(repoDir string, loc attach.RunLocation) (coherenceSnap, state.RunState, error) {
	var s coherenceSnap
	active, aok, err := attach.ActiveRunPointer(repoDir)
	if err != nil {
		return s, state.RunState{}, err
	}
	s.activeRunID, s.activeOK = active, aok

	runID := loc.RunID
	if s.pair, err = headSnap(loc.AttachDir, loc.RunLock, func(rec txn.Record, ok bool) bool {
		c, cerr := attach.ClassifyPairRecord(rec, ok, runID)
		return cerr != nil || c == attach.JournalNonTerminal
	}); err != nil {
		return s, state.RunState{}, err
	}
	if s.repl, err = headSnap(loc.ReplaceDir, loc.RunLock, func(rec txn.Record, ok bool) bool {
		c, cerr := attach.ClassifyReplaceRecord(rec, ok, runID)
		return cerr != nil || c == attach.ReplaceJournalNonTerminal
	}); err != nil {
		return s, state.RunState{}, err
	}
	if s.ctxn, err = headSnap(loc.CommitTxnDir, loc.RunLock, func(rec txn.Record, ok bool) bool {
		c, cerr := attach.ClassifyCommitTxnRecord(rec, ok, runID)
		return cerr != nil || c == attach.CommitTxnJournalNonTerminal
	}); err != nil {
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

	// Central cross-store identity bind: state.Registry's documented invariant (Registry.RunID ==
	// RunState.RunID) PLUS the resolved location — every present State and Registry must name
	// loc.RunID, and therefore each other. Binding it here (not independently in each consumer)
	// means a coherently cross-wired store fails closed as recovery/corruption for status AND wait.
	s.identityCorrupt = (sok && rs.RunID != loc.RunID) || (gok && reg.RunID != loc.RunID)
	return s, rs, nil
}

// recoveryRequired reports whether the snapshot shows an aggregate transaction that forbids a
// coherent read: any journal DOMAIN-classified recovery-required (aborted/pending/misbound), or a
// commit-txn journal that VANISHED after the run accepted git evidence (corruption).
func (s coherenceSnap) recoveryRequired(rs state.RunState) bool {
	if s.identityCorrupt {
		return true
	}
	if s.pair.recover || s.repl.recover || s.ctxn.recover {
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
// readBetweenSnapsHook is a test-only seam fired between the read and the second snapshot, so a
// test can deterministically force a moving snapshot (persistent churn). Nil in production.
var readBetweenSnapsHook func()

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
		if readBetweenSnapsHook != nil {
			readBetweenSnapsHook()
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
	// Persistent churn: a run mutating faster than the bracket can read is itself recovery-
	// required (D021 pins "retry, else recovery-required"), so callers keep the typed
	// classification rather than a generic error.
	return zero, fmt.Errorf("%w: no coherent snapshot after %d bracket attempts (persistent churn)", ErrReadRecoveryRequired, readCoherenceMaxAttempts)
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
