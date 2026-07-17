package attach

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

func TestResolveRunBindsActiveRun(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)

	loc, err := ResolveRun(repo, a.RunID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	want := RunLocation{
		RunID:       a.RunID,
		RelDir:      state.RunDirRelFor(a.RunID),
		RunDir:      runDir,
		RunLock:     runLock(runDir),
		StateDir:    filepath.Join(runDir, "state"),
		RegistryDir: filepath.Join(runDir, "registry"),
		AttachDir:   lay.attachJournalDir(runDir),
	}
	if loc != want {
		t.Fatalf("location =\n %+v\nwant\n %+v", loc, want)
	}
}

func TestResolveRunRejectsUnknownRun(t *testing.T) {
	repo := t.TempDir()
	bootstrapRun(t, repo) // an unrelated active run exists

	// A syntactically valid but non-active run id must not resolve to a directory.
	other := "run-" + "0123456789abcdef0123456789abcdef"
	if _, err := ResolveRun(repo, other); !errors.Is(err, ErrJoinUnauthorized) {
		t.Fatalf("unknown run err = %v, want ErrJoinUnauthorized", err)
	}
}

func TestResolveRunNoActiveRun(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	// A well-formed run id, but there is no active run at a fresh repo.
	if _, err := ResolveRun(t.TempDir(), a.RunID); !errors.Is(err, ErrJoinUnauthorized) {
		t.Fatalf("no-active err = %v, want ErrJoinUnauthorized", err)
	}
}

func TestResolveRunRejectsNonCanonicalID(t *testing.T) {
	repo := t.TempDir()
	bootstrapRun(t, repo)
	if _, err := ResolveRun(repo, "not-a-run-id"); !errors.Is(err, ErrJoinUnauthorized) {
		t.Fatalf("non-canonical err = %v, want ErrJoinUnauthorized", err)
	}
}

// A lead-only run has no pair-attach journal (Absent); after a pair fills, the
// journal is a complete, identity-bound pairing (Terminal).
func TestClassifyPairJournalAbsentThenTerminal(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	loc, err := ResolveRun(repo, a.RunID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	class, err := ClassifyPairJournal(loc)
	if err != nil {
		t.Fatalf("classify (pre-pair): %v", err)
	}
	if class != JournalAbsent {
		t.Fatalf("pre-pair class = %d, want JournalAbsent", class)
	}

	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err != nil {
		t.Fatalf("join: %v", err)
	}
	class, err = ClassifyPairJournal(loc)
	if err != nil {
		t.Fatalf("classify (post-pair): %v", err)
	}
	if class != JournalTerminal {
		t.Fatalf("post-pair class = %d, want JournalTerminal", class)
	}
}

// classifyPairRecord covers every branch against a real, complete pair-attach record.
func TestClassifyPairRecordBranches(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	loc, err := ResolveRun(repo, a.RunID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err != nil {
		t.Fatalf("join: %v", err)
	}
	rec, ok, err := txn.Open(loc.AttachDir, loc.RunLock).Latest()
	if err != nil || !ok {
		t.Fatalf("load journal: ok=%v err=%v", ok, err)
	}

	// Absent when no record is present.
	if c, err := classifyPairRecord(txn.Record{}, false, a.RunID); err != nil || c != JournalAbsent {
		t.Fatalf("absent: c=%d err=%v", c, err)
	}
	// A bound complete record is terminal.
	if c, err := classifyPairRecord(rec, true, a.RunID); err != nil || c != JournalTerminal {
		t.Fatalf("terminal: c=%d err=%v", c, err)
	}
	// A run-id the record does not name must not be treated as a completed pairing.
	if _, err := classifyPairRecord(rec, true, "run-"+"0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrJoinUnauthorized) {
		t.Fatalf("id mismatch err = %v, want ErrJoinUnauthorized", err)
	}
	// A wrong intent kind fails before the payload is trusted.
	badKind := rec
	badKind.Intent.Kind = "bootstrap"
	if _, err := classifyPairRecord(badKind, true, a.RunID); !errors.Is(err, ErrJoinUnauthorized) {
		t.Fatalf("kind mismatch err = %v, want ErrJoinUnauthorized", err)
	}
	// An in-flight (incomplete) record is non-terminal.
	inflight := rec
	inflight.Complete = false
	if c, err := classifyPairRecord(inflight, true, a.RunID); err != nil || c != JournalNonTerminal {
		t.Fatalf("in-flight: c=%d err=%v", c, err)
	}
	// A cleanly aborted record never paired.
	aborted := rec
	aborted.Aborted = true
	if c, err := classifyPairRecord(aborted, true, a.RunID); err != nil || c != JournalNonTerminal {
		t.Fatalf("aborted: c=%d err=%v", c, err)
	}
}
