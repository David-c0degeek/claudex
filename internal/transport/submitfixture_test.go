package transport

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

// Transition is the legacy value-only transition shape; the test shim adapts it to
// a Prepare. The production Submit takes Prepare directly.
type Transition = func(PreparedSubmit, uint64, *state.RunState) error

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

// adaptTransition wraps a legacy Transition as a Prepare that declares no issued
// identities; requireLiveOwner remains the authority on the resulting shape, and the
// new tests exercise the issued-id recheck explicitly.
func adaptTransition(adv Transition) Prepare {
	return func(_ state.RunState, p PreparedSubmit) (PreparedTransition, error) {
		return NewPreparedTransition("", "", func(gen uint64, next *state.RunState) error { return adv(p, gen, next) }), nil
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
