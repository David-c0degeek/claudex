package transport

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

// prepareIssuing builds a Prepare that declares the identities it will issue and
// applies the given (deterministic) body exactly once inside the state CAS — never a
// discovery probe.
func prepareIssuing(turnID, gateID string, apply func(gen uint64, next *state.RunState) error) Prepare {
	return func(state.RunState, PreparedSubmit) (PreparedTransition, error) {
		return NewPreparedTransition(turnID, gateID, apply), nil
	}
}

func prepareErr(err error) Prepare {
	return func(state.RunState, PreparedSubmit) (PreparedTransition, error) { return PreparedTransition{}, err }
}

// checkpointApply is the standard running transition body for the CHECKPOINT fixture:
// the review agrees, so the run advances to the next IMPLEMENT_STEP and issues turn-2.
// checkpointPrep declares that turn; countingPrep additionally counts each real apply
// so exact-once is observable.
func checkpointApply(gen uint64, next *state.RunState) error {
	next.Phase = state.PhaseImplementStep
	next.Assignment = &state.Ref{ID: "turn-2", IssuedRevision: gen}
	bindEvidence(next, gen)
	return nil
}

func checkpointPrep() Prepare { return prepareIssuing("turn-2", "", checkpointApply) }

func countingPrep(calls *atomic.Int64) Prepare {
	return prepareIssuing("turn-2", "", func(gen uint64, next *state.RunState) error {
		calls.Add(1)
		return checkpointApply(gen, next)
	})
}

// advancer counts how many times its transition actually applies (never a probe),
// so tests can prove a transition runs exactly once and never on replay/conflict.
type advancer struct{ calls atomic.Int64 }

func (a *advancer) prep() Prepare { return countingPrep(&a.calls) }

var (
	leadSess    = "sess-" + strings.Repeat("a", 32)
	pairSess    = "sess-" + strings.Repeat("b", 32)
	unknownSess = "sess-" + strings.Repeat("f", 32)
)

// canonSess maps a test's convenient session name to a canonical minted id: the
// lead/pair sessions the fixture registry authorizes, or an unknown canonical id.
func canonSess(name string) string {
	switch name {
	case "sess-1":
		return leadSess
	case "sess-2":
		return pairSess
	default:
		return unknownSess
	}
}

// fakeJournal is an injectable attach-journal reader for deterministic tests.
type fakeJournal struct {
	lockPath string
	head     JournalHead
	err      error
}

func (f fakeJournal) LockPath() string                                  { return f.lockPath }
func (f fakeJournal) Head(*genstore.Guard, string) (JournalHead, error) { return f.head, f.err }

func runDir(store *state.Store) string { return filepath.Dir(store.LockPath()) }

func openRunRegistry(store *state.Store) *state.RegistryStore {
	return state.OpenRegistry(filepath.Join(runDir(store), "registry"), store.LockPath())
}

func terminalJournal(store *state.Store) JournalReader {
	return fakeJournal{lockPath: store.LockPath(), head: JournalTerminal}
}

// initRegistry builds a valid two-slot registry (lead=claude, pair=codex) for run-a,
// sharing the store's run lock.
func initRegistry(t *testing.T, store *state.Store) {
	t.Helper()
	reg := openRunRegistry(store)
	r1, err := reg.Mutate(0, func(gen uint64, n *state.Registry) error {
		n.RunID = "run-a"
		n.Lead = &state.RoleSlot{Agent: state.AgentClaude, CurrentSessionID: leadSess, Sessions: []state.SessionRecord{{SessionID: leadSess, Generation: 1, IssuedRegistryRevision: gen}}}
		return nil
	})
	if err != nil {
		t.Fatalf("registry init: %v", err)
	}
	if _, err := reg.Mutate(r1.Revision, func(gen uint64, n *state.Registry) error {
		n.Pair = &state.RoleSlot{Agent: state.AgentCodex, CurrentSessionID: pairSess, Sessions: []state.SessionRecord{{SessionID: pairSess, Generation: 1, IssuedRegistryRevision: gen}}}
		return nil
	}); err != nil {
		t.Fatalf("registry pair: %v", err)
	}
}

// submitDeps builds the standard deps for a run fixture (terminal journal, no
// preflight); tests that need a specific journal/preflight construct deps directly.
func submitDeps(store *state.Store, sink ArtifactSink, prepare Prepare) SubmitDeps {
	return SubmitDeps{Store: store, Registry: openRunRegistry(store), Journal: terminalJournal(store), Sink: sink, Prepare: prepare}
}
