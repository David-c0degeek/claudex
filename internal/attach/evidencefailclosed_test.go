package attach

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// Pin B2: producing the review packet is a DETERMINISTIC authoring step — a selector absent from the
// source tree, a bounds breach, an unreadable frozen snapshot — and it happens during the locked
// authorized preparation, BEFORE the transaction opens. A failure must therefore refuse the attach
// outright, leaving the Registry, the attach journal, and RunState exactly as they were: a failed
// packet can never strand a half-issued turn that no evidence backs.
func TestPairAttachEvidenceFailureLeavesNothingDurable(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))

	registry := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir))
	runState := state.Open(filepath.Join(runDir, "state"), runLock(runDir))
	regBefore, _, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	stateBefore, _, err := runState.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	journalHeadBefore := attachJournalHead(t, lay, runDir)

	boom := errors.New("selector absent from the source tree")
	req := joinRequest(repo, a.RunID, opID("b"), 0x10)
	req.Evidence = &stubIssuer{err: boom}
	if _, jerr := JoinAttach(req); !errors.Is(jerr, boom) {
		t.Fatalf("join err = %v, want the packet failure", jerr)
	}

	regAfter, _, err := registry.Load()
	if err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if regAfter.Revision != regBefore.Revision || regAfter.Pair != nil {
		t.Fatalf("registry moved: revision %d -> %d, pair %+v", regBefore.Revision, regAfter.Revision, regAfter.Pair)
	}
	stateAfter, _, err := runState.Load()
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if stateAfter.Revision != stateBefore.Revision || stateAfter.Phase != state.PhaseInit ||
		stateAfter.Assignment != nil || stateAfter.Evidence != nil {
		t.Fatalf("run state moved: %+v", stateAfter)
	}
	if got := attachJournalHead(t, lay, runDir); got != journalHeadBefore {
		t.Fatalf("attach journal head = %q, want the unchanged %q", got, journalHeadBefore)
	}

	// The run is still joinable: a retry with a WORKING issuer succeeds, proving the failure left no
	// poisoned partial state behind.
	if _, jerr := JoinAttach(joinRequest(repo, a.RunID, opID("c"), 0x11)); jerr != nil {
		t.Fatalf("join after a packet failure: %v", jerr)
	}
}

// attachJournalHead summarizes the pair journal's head so an unchanged journal is observable.
func attachJournalHead(t *testing.T, lay layout, runDir string) string {
	t.Helper()
	rec, present, err := txn.Open(lay.attachJournalDir(runDir), runLock(runDir)).Latest()
	if err != nil {
		t.Fatalf("read attach journal: %v", err)
	}
	if !present {
		return "absent"
	}
	return string(rec.Intent.Kind) + "/" + rec.Intent.TxnID
}
