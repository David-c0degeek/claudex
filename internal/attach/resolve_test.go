package attach

import (
	"errors"
	"path/filepath"
	"strings"
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
		CommitTxnDir: lay.commitTxnJournalDir(runDir),
		ArtifactsDir: filepath.Join(runDir, "artifacts"),
		SessionDir:   filepath.Join(repo, ".claudex", "session"),
		MailboxDir:   filepath.Join(repo, ".claudex"),
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

// ClassifyReplaceJournal reports Absent before any replacement, NonTerminal for a pending
// (ambiguous) one, and Terminal after it is recovered forward to completion — so a submit
// never authorizes off the Registry while a replacement is mid-supersession.
func TestClassifyReplaceJournalLifecycle(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	loc, err := ResolveRun(repo, a.RunID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	classify := func(t *testing.T) ReplaceJournalClass {
		t.Helper()
		g, ok, err := genstore.Acquire(loc.RunLock)
		if err != nil || !ok {
			t.Fatalf("acquire: ok=%v err=%v", ok, err)
		}
		defer g.Release()
		c, cerr := ClassifyReplaceJournal(g, loc)
		if cerr != nil {
			t.Fatalf("classify: %v", cerr)
		}
		return c
	}

	// Absent before any replacement (the replace journal dir does not yet exist).
	if c := classify(t); c != ReplaceJournalAbsent {
		t.Fatalf("pre-replace class = %d, want ReplaceJournalAbsent", c)
	}

	// A genuine pending replacement: an ambiguous Registry mutate leaves the journal
	// prepared-but-not-completed.
	req := replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)
	amb := defaultReplaceSeams()
	amb.mutate = ambiguousMutate()
	if _, err := replaceAttach(req, amb); !errors.Is(err, ErrReplaceOutcomeUnknown) {
		t.Fatalf("ambiguous replace err = %v, want ErrReplaceOutcomeUnknown", err)
	}
	if c := classify(t); c != ReplaceJournalNonTerminal {
		t.Fatalf("pending class = %d, want ReplaceJournalNonTerminal", c)
	}

	// A same-operation retry recovers it forward to a completed (terminal) replacement; an
	// errReader RNG proves the retry never re-mints.
	retry := req
	retry.RNG = errReader{}
	if _, err := ReplaceAttach(retry); err != nil {
		t.Fatalf("recovery retry: %v", err)
	}
	if c := classify(t); c != ReplaceJournalTerminal {
		t.Fatalf("terminal class = %d, want ReplaceJournalTerminal", c)
	}
}

// classifyReplaceRecord binds before the terminality branch: a mis-bound head (wrong kind
// or a run-id it does not name) is an error, never a clean class.
func TestClassifyReplaceRecordMisbound(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	in := sampleReplaceIntent(runID, 3)
	good := txn.Record{
		Intent: txn.Intent{
			Version: txn.IntentVersion, Kind: replaceIntentKind, TxnID: in.TxnID,
			Payload: mustMarshalReplace(in),
		},
		StepIDs:  []string{stepRegistryReplace},
		Complete: true,
	}
	// A bound, complete head is Terminal.
	if c, err := classifyReplaceRecord(good, true, runID); err != nil || c != ReplaceJournalTerminal {
		t.Fatalf("bound terminal: c=%d err=%v", c, err)
	}
	// Not present: Absent.
	if c, err := classifyReplaceRecord(txn.Record{}, false, runID); err != nil || c != ReplaceJournalAbsent {
		t.Fatalf("absent: c=%d err=%v", c, err)
	}
	// Wrong intent kind: an error.
	badKind := good
	badKind.Intent.Kind = "bootstrap"
	if _, err := classifyReplaceRecord(badKind, true, runID); err == nil {
		t.Fatal("wrong kind should be an error")
	}
	// A run-id the record does not name: an error.
	if _, err := classifyReplaceRecord(good, true, "run-"+strings.Repeat("b", 32)); err == nil {
		t.Fatal("run-id mismatch should be an error")
	}
}
