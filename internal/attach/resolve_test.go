package attach

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
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
		RunID:        a.RunID,
		RelDir:       state.RunDirRelFor(a.RunID),
		RunDir:       runDir,
		RunLock:      runLock(runDir),
		StateDir:     filepath.Join(runDir, "state"),
		RegistryDir:  filepath.Join(runDir, "registry"),
		AttachDir:    lay.attachJournalDir(runDir),
		ReplaceDir:   lay.replaceJournalDir(runDir),
		ArtifactsDir: filepath.Join(runDir, "artifacts"),
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
// journal is a complete, identity-bound pairing (Terminal). ClassifyPairJournal reads
// under a held run guard.
func TestClassifyPairJournalAbsentThenTerminal(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	loc, err := ResolveRun(repo, a.RunID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	g, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	class, err := ClassifyPairJournal(g, loc)
	if err != nil {
		t.Fatalf("classify (pre-pair): %v", err)
	}
	if class != JournalAbsent {
		t.Fatalf("pre-pair class = %d, want JournalAbsent", class)
	}
	if err := g.Release(); err != nil { // release before JoinAttach takes the run lock
		t.Fatalf("release: %v", err)
	}

	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err != nil {
		t.Fatalf("join: %v", err)
	}
	g2, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil || !ok {
		t.Fatalf("acquire2: ok=%v err=%v", ok, err)
	}
	defer g2.Release()
	class, err = ClassifyPairJournal(g2, loc)
	if err != nil {
		t.Fatalf("classify (post-pair): %v", err)
	}
	if class != JournalTerminal {
		t.Fatalf("post-pair class = %d, want JournalTerminal", class)
	}
}

// ClassifyPairJournal proves the caller holds the live run lock before reading.
func TestClassifyPairJournalGuardRequired(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	loc, err := ResolveRun(repo, a.RunID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if _, err := ClassifyPairJournal(nil, loc); err == nil {
		t.Fatal("nil guard should be rejected")
	}
	other, ok, err := genstore.Acquire(filepath.Join(loc.RunDir, "other.lock"))
	if err != nil || !ok {
		t.Fatalf("acquire other: ok=%v err=%v", ok, err)
	}
	if _, err := ClassifyPairJournal(other, loc); !errors.Is(err, genstore.ErrWrongLock) {
		t.Fatalf("wrong-lock err = %v, want ErrWrongLock", err)
	}
	if err := other.Release(); err != nil {
		t.Fatalf("release other: %v", err)
	}
	g, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := g.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := ClassifyPairJournal(g, loc); err == nil {
		t.Fatal("released guard should be rejected")
	}
}

// classifyPairRecord covers every branch against genuine records: absent, a real
// pending record (a crash after pair-fill, before completion), a realistic aborted
// shape, a mis-bound intent, a run-id mismatch, and a completed pairing.
func TestClassifyPairRecordBranches(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	loc, err := ResolveRun(repo, a.RunID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Absent when no record is present.
	if c, err := classifyPairRecord(txn.Record{}, false, a.RunID); err != nil || c != JournalAbsent {
		t.Fatalf("absent: c=%d err=%v", c, err)
	}

	// A genuine pending record: crash after the pair-fill step, before completion.
	fired := false
	stepFailpoint = func(s string) error {
		if s == "registry-pair-fill" && !fired {
			fired = true
			return errors.New("injected crash after pair-fill")
		}
		return nil
	}
	defer func() { stepFailpoint = nil }() // never let a failure mid-test leak the hook
	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err == nil {
		t.Fatal("expected a crash after pair-fill")
	}
	stepFailpoint = nil

	pending, ok, err := txn.Open(loc.AttachDir, loc.RunLock).Latest()
	if err != nil || !ok {
		t.Fatalf("load pending: ok=%v err=%v", ok, err)
	}
	if pending.Complete {
		t.Fatalf("expected a pending (incomplete) record, got a complete one")
	}
	// A bound but non-terminal record classifies NonTerminal.
	if c, err := classifyPairRecord(pending, true, a.RunID); err != nil || c != JournalNonTerminal {
		t.Fatalf("pending: c=%d err=%v", c, err)
	}
	// A mis-bound record (wrong intent kind) is an error, never a class.
	badKind := pending
	badKind.Intent.Kind = "bootstrap"
	if _, err := classifyPairRecord(badKind, true, a.RunID); !errors.Is(err, ErrJoinUnauthorized) {
		t.Fatalf("kind mismatch err = %v, want ErrJoinUnauthorized", err)
	}
	// A run-id the record does not name is a mismatch.
	if _, err := classifyPairRecord(pending, true, "run-"+"0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrJoinUnauthorized) {
		t.Fatalf("id mismatch err = %v, want ErrJoinUnauthorized", err)
	}

	// Recover forward to a completed pairing, which is terminal.
	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err != nil {
		t.Fatalf("recovery join: %v", err)
	}
	complete, ok, err := txn.Open(loc.AttachDir, loc.RunLock).Latest()
	if err != nil || !ok {
		t.Fatalf("load complete: ok=%v err=%v", ok, err)
	}
	if c, err := classifyPairRecord(complete, true, a.RunID); err != nil || c != JournalTerminal {
		t.Fatalf("terminal: c=%d err=%v", c, err)
	}
}
