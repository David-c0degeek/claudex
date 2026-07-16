package attach

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// realRun bootstraps a run, fills the pair, and returns the live CurrentRun,
// Registry, and terminal attach-journal head for reader-injection tests.
func realRun(t *testing.T) (repo string, a FirstAttachResult, pair JoinAttachResult, cur state.CurrentRun, reg state.Registry, jrec txn.Record) {
	t.Helper()
	repo = t.TempDir()
	a = bootstrapRun(t, repo)
	var err error
	pair, err = JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	cur, _, _ = state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	reg, _, _ = state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load()
	jrec, _, _ = txn.Open(lay.attachJournalDir(runDir), runLock(runDir)).Latest()
	return
}

// CurrentRun switching between the bracketing reads is stale.
func TestReattachCurrentSwitchIsStale(t *testing.T) {
	repo, a, pair, cur, reg, jrec := realRun(t)
	lay := layoutFor(repo)
	switched := cur
	switched.Revision = cur.Revision + 99 // a different generation
	calls := 0
	reader := reattachReader{
		current: func() (state.CurrentRun, bool, error) {
			calls++
			if calls == 1 {
				return cur, true, nil
			}
			return switched, true, nil
		},
		journalHead: func() (txn.Record, bool, error) { return jrec, true, nil },
		registry:    func() (state.Registry, bool, error) { return reg, true, nil },
	}
	res, err := reattachWith(reattachReq(repo, a.RunID, pair.SessionID, state.AgentCodex, state.SlotPair), lay, reader)
	if err != nil || res.Status != ReattachStale {
		t.Fatalf("current-switch reattach = %+v err=%v, want stale", res, err)
	}
}

// A journal head becoming pending (terminal->pending) while a pair slot is visible
// is recovery-required, never current.
func TestReattachJournalTurnsPendingIsRecovery(t *testing.T) {
	repo, a, pair, cur, reg, jrec := realRun(t)
	lay := layoutFor(repo)
	calls := 0
	reader := reattachReader{
		current:  func() (state.CurrentRun, bool, error) { return cur, true, nil },
		registry: func() (state.Registry, bool, error) { return reg, true, nil },
		journalHead: func() (txn.Record, bool, error) {
			calls++
			if calls == 1 {
				return jrec, true, nil // terminal
			}
			return txn.Record{}, true, nil // a new pending transaction
		},
	}
	res, err := reattachWith(reattachReq(repo, a.RunID, pair.SessionID, state.AgentCodex, state.SlotPair), lay, reader)
	if err != nil || res.Status != ReattachRecoveryRequired {
		t.Fatalf("journal-turns-pending reattach = %+v err=%v, want recovery-required", res, err)
	}
}

// A journal head appearing (missing->complete) between reads is stale/retry.
func TestReattachJournalAppearsIsStale(t *testing.T) {
	repo, a, pair, cur, reg, jrec := realRun(t)
	lay := layoutFor(repo)
	calls := 0
	reader := reattachReader{
		current:  func() (state.CurrentRun, bool, error) { return cur, true, nil },
		registry: func() (state.Registry, bool, error) { return reg, true, nil },
		journalHead: func() (txn.Record, bool, error) {
			calls++
			if calls == 1 {
				return txn.Record{}, false, nil // missing
			}
			return jrec, true, nil // now complete (terminal)
		},
	}
	res, err := reattachWith(reattachReq(repo, a.RunID, pair.SessionID, state.AgentCodex, state.SlotPair), lay, reader)
	if err != nil || res.Status != ReattachStale {
		t.Fatalf("journal-appears reattach = %+v err=%v, want stale", res, err)
	}
}

// A replaced session presented with the wrong agent/role is a mismatch, not a
// valid replaced identity.
func TestReattachReplacedWrongRoleIsMismatch(t *testing.T) {
	repo, a, pair, _, _, _ := realRun(t)
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	regStore := state.OpenRegistry(filepath.Join(runDir, "registry"), filepath.Join(runDir, "run.lock"))
	reg, _, _ := regStore.Load()
	newSess := "sess-" + strings.Repeat("e", 32)
	if _, err := regStore.Mutate(reg.Revision, func(gen uint64, next *state.Registry) error {
		next.Pair.Sessions = append(next.Pair.Sessions, state.SessionRecord{SessionID: newSess, Generation: 2, IssuedRegistryRevision: gen})
		next.Pair.CurrentSessionID = newSess
		return nil
	}); err != nil {
		t.Fatalf("replace pair: %v", err)
	}
	// The old (replaced) pair session, presented as the LEAD role.
	res, err := Reattach(reattachReq(repo, a.RunID, pair.SessionID, state.AgentCodex, state.SlotLead))
	if err != nil || res.Status != ReattachMismatch {
		t.Fatalf("replaced-wrong-role reattach = %+v err=%v, want mismatch", res, err)
	}
}
