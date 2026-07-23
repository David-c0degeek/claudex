package attach

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// ConfirmPairJournal re-confirms the pair-attach journal store's durability under a held run
// guard. A reopen must re-confirm a visible-but-durability-unconfirmed TERMINAL head before
// its classification authorizes serving the run — ClassifyPairJournal reads the head with no
// barrier (Latest only), whereas txn.Recover always confirms even a terminal head. A journal
// store that does not exist (a lead-only run) is a no-op.
func ConfirmPairJournal(g *genstore.Guard, loc RunLocation) error {
	return confirmJournalStore(g, loc.AttachDir, loc.RunLock)
}

// ConfirmReplaceJournal re-confirms the session-replacement journal store's durability under a
// held run guard (see ConfirmPairJournal); a run that never had a replacement is a no-op.
func ConfirmReplaceJournal(g *genstore.Guard, loc RunLocation) error {
	return confirmJournalStore(g, loc.ReplaceDir, loc.RunLock)
}

// ConfirmCommitTxnJournal re-confirms the implementation-submit git-commit transaction journal
// store's durability under a held run guard (see ConfirmPairJournal); a run that has accepted no
// git commit is a no-op.
func ConfirmCommitTxnJournal(g *genstore.Guard, loc RunLocation) error {
	return confirmJournalStore(g, loc.CommitTxnDir, loc.RunLock)
}

// CommitTxnJournalDir is the per-run git-commit transaction journal directory (a helper so the
// coordinator opens exactly this store, never joining paths of its own).
func CommitTxnJournalDir(loc RunLocation) string { return loc.CommitTxnDir }

func confirmJournalStore(g *genstore.Guard, dir, lock string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil // never recorded: nothing to confirm
	}
	return txn.Open(dir, lock).ConfirmDurable(g)
}

// RunLocation is the canonical, attach-derived location of a bound run. Every path is
// derived here from the repository layout so a consumer (the coordinator) never joins
// run paths of its own — it opens exactly these stores under RunLock.
type RunLocation struct {
	RunID        string
	RelDir       string // repo-relative run directory
	RunDir       string // absolute run directory
	RunLock      string // the per-run mutation lock (the two-tier protocol's inner lock)
	StateDir     string // the RunState genstore directory
	RegistryDir  string // the Registry genstore directory
	AttachDir    string // the pair-attach transaction journal directory
	ReplaceDir   string // the dedicated session-replacement transaction journal directory
	CommitTxnDir string // the dedicated implementation-submit git-commit transaction journal directory
	ArtifactsDir string // the content-addressed submit-artifact store directory
	SessionDir   string // the role-addressed session inbox store dir (.claudex/session; SessionStore roots here)
	MailboxDir   string // the dir holding the rebuildable .claudex/mailbox.md transcript mirror (MailboxStore roots here)
}

// ResolveRun binds runID to the repository's single active bootstrap allocation and
// returns the canonical run paths. It reuses the active-pointer -> catalog ->
// bootstrap-journal authority (the same bindRunToBootstrap a join uses), so a forged
// active pointer cannot route to an unallocated directory, and it runs the
// legacy-refusal guard first so a pre-pivot run directory is never resolved. It is
// read-only and lock-free: the per-run lock need not exist yet.
func ResolveRun(repoDir, runID string) (RunLocation, error) {
	if !state.IsRunID(runID) {
		return RunLocation{}, fmt.Errorf("%w: run id is not canonical", ErrJoinUnauthorized)
	}
	if err := legacyRepoRefusal(repoDir); err != nil {
		return RunLocation{}, err
	}
	lay := layoutFor(repoDir)
	cur, ok, err := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if err != nil {
		return RunLocation{}, err
	}
	if !ok {
		return RunLocation{}, fmt.Errorf("%w: no active run", ErrJoinUnauthorized)
	}
	if err := bindRunToBootstrap(lay, cur, runID); err != nil {
		return RunLocation{}, err
	}
	return runLocationFor(lay, runID), nil
}

func runLocationFor(lay layout, runID string) RunLocation {
	rel := state.RunDirRelFor(runID)
	runDir := lay.runDir(rel)
	base := filepath.Join(lay.repoDir, runtimeDirName)
	return RunLocation{
		RunID:        runID,
		RelDir:       rel,
		RunDir:       runDir,
		RunLock:      runLock(runDir),
		StateDir:     filepath.Join(runDir, "state"),
		RegistryDir:  filepath.Join(runDir, "registry"),
		AttachDir:    lay.attachJournalDir(runDir),
		ReplaceDir:   lay.replaceJournalDir(runDir),
		CommitTxnDir: lay.commitTxnJournalDir(runDir),
		ArtifactsDir: filepath.Join(runDir, "artifacts"),
		// The session inbox and the human-readable mailbox mirror are repo-level per D018
		// (`.claudex/mailbox.md`), attach-derived so no consumer re-joins the private layout.
		SessionDir: filepath.Join(base, "session"),
		MailboxDir: base,
	}
}

// RepoLock is the repository-level allocation lock (`.claudex/repo.lock`) — the guard that
// serializes bootstrap/active-pointer switches. A repo-scoped projection (the shared
// `.claudex/mailbox.md` transcript, which any run may write) must serialize its writers on THIS
// lock, not a per-run lock, so a delayed writer of a superseded run cannot overwrite the current
// active run's projection.
func RepoLock(repoDir string) string { return layoutFor(repoDir).repoLock }

// ActiveRunPointer returns the raw active-run pointer identity (RunID + whether a run is
// currently active), WITHOUT the ResolveRun binding walk. A caller holding RepoLock uses it to
// bind a repo-level projection write to the current active run: the pointer cannot change under
// the held repo lock (bootstrap mutates it only under that lock). (false) means no active run.
func ActiveRunPointer(repoDir string) (runID string, active bool, err error) {
	lay := layoutFor(repoDir)
	cur, ok, err := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if err != nil {
		return "", false, err
	}
	if !ok || !cur.Active {
		return "", false, nil
	}
	return cur.RunID, true, nil
}

// ActiveRunID discovers the repository's single active run id from the active-run pointer, for
// convenience so a CLI need not always pass --run. It is DISCOVERY ONLY: the loaded id is
// validated through ResolveRun's active-pointer -> catalog -> bootstrap-journal binding (never
// trusted as raw pointer authority), and the legacy-refusal guard runs first. (false, nil) means
// no active run; any binding failure is an error, never a silent id.
func ActiveRunID(repoDir string) (string, bool, error) {
	if err := legacyRepoRefusal(repoDir); err != nil {
		return "", false, err
	}
	lay := layoutFor(repoDir)
	cur, ok, err := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	if _, err := ResolveRun(repoDir, cur.RunID); err != nil {
		return "", false, err
	}
	return cur.RunID, true, nil
}

// JournalClass is the classification of a run's pair-attach journal head.
type JournalClass int

const (
	// JournalAbsent means no pair-attach was ever recorded (a lead-only run).
	JournalAbsent JournalClass = iota
	// JournalNonTerminal means a pair-attach is present but not a completed pairing —
	// an in-flight transaction (recovery required) or a cleanly aborted one. A
	// consumer must not open the run for submits on this class.
	JournalNonTerminal
	// JournalTerminal means a complete, identity-bound pair-attach: the run is paired.
	JournalTerminal
)

// ClassifyPairJournal classifies a resolved run's pair-attach journal head under a
// held run guard. It proves g is this journal's live lock (CheckGuard) before reading
// the head, so the classification is not a lock-free race against a concurrent
// transaction. Every present record — pending, aborted, or complete — is first bound
// to its decoded pair-intent payload (version and kind, txn id, expected state
// revision, and run id); only then is a complete record reported Terminal and a
// pending/aborted one NonTerminal. A mis-bound record is an error, never a class.
func ClassifyPairJournal(g *genstore.Guard, loc RunLocation) (JournalClass, error) {
	journal := txn.Open(loc.AttachDir, loc.RunLock)
	if err := journal.CheckGuard(g); err != nil {
		return JournalAbsent, err
	}
	rec, ok, err := journal.Latest()
	if err != nil {
		return JournalAbsent, err
	}
	return classifyPairRecord(rec, ok, loc.RunID)
}

// ClassifyPairRecord is the PURE, lock-free domain classification of a pair-attach journal head
// already read (via txn.Latest) by a caller that cannot hold the run guard — a read-only reader
// bracketing a coherent snapshot. It binds the present head's kind/version/envelope/payload/run
// identity before considering it complete, so an aborted, pending, or misbound head is
// NonTerminal/error (recovery-required), never mistaken for a completed pairing.
func ClassifyPairRecord(rec txn.Record, ok bool, runID string) (JournalClass, error) {
	return classifyPairRecord(rec, ok, runID)
}

func classifyPairRecord(rec txn.Record, ok bool, runID string) (JournalClass, error) {
	if !ok {
		return JournalAbsent, nil
	}
	// Bind the intent BEFORE the terminality branch: a pending pair-attach still has a
	// valid intent envelope+payload, so a corrupt or mis-bound record must fail here
	// whether or not it is complete.
	if rec.Intent.Version != txn.IntentVersion || rec.Intent.Kind != pairIntentKind {
		return JournalAbsent, fmt.Errorf("%w: pair journal kind/version mismatch", ErrJoinUnauthorized)
	}
	in, err := decodePairIntent(rec.Intent.Payload)
	if err != nil {
		return JournalAbsent, err
	}
	if rec.TxnID() != in.TxnID || rec.Intent.ExpectedStateRevision != in.ExpectedStateRevision || in.RunID != runID {
		return JournalAbsent, fmt.Errorf("%w: pair journal identity mismatch", ErrJoinUnauthorized)
	}
	if rec.Complete && !rec.Aborted {
		return JournalTerminal, nil
	}
	// A cleanly aborted pairing or an in-flight transaction: bound, but not a
	// completed pairing the consumer may open.
	return JournalNonTerminal, nil
}

// ReplaceJournalClass is the classification of a run's session-replacement journal head.
type ReplaceJournalClass int

const (
	// ReplaceJournalAbsent means no session replacement was ever recorded for the run.
	ReplaceJournalAbsent ReplaceJournalClass = iota
	// ReplaceJournalNonTerminal means a replacement is present but not a completed
	// transaction — an in-flight one (recovery required) or a cleanly aborted one. A
	// consumer must NOT authorize a run operation off the Registry while a replacement is
	// pending: the supersession it would read against may be mid-flight, so the operation
	// is recovery-required until replaceAttach completes the pending replacement.
	ReplaceJournalNonTerminal
	// ReplaceJournalTerminal means a complete, identity-bound session replacement; the
	// Registry durably reflects the supersession and an ordinary operation may proceed.
	ReplaceJournalTerminal
)

// ClassifyReplaceJournal classifies a resolved run's session-replacement journal head
// under a held run guard. Like ClassifyPairJournal it proves g is this journal's live
// lock (CheckGuard) before reading, so the classification is not a lock-free race against
// a concurrent replaceAttach. Every present record — pending, aborted, or complete — is
// first bound to its exact replacement envelope/payload/run/step-list (bindReplaceHead);
// only then is a complete record reported Terminal and a pending/aborted one NonTerminal.
// A mis-bound record is an error, never a class — it is recovery-required, exactly as
// replaceAttach itself treats a head it cannot bind.
func ClassifyReplaceJournal(g *genstore.Guard, loc RunLocation) (ReplaceJournalClass, error) {
	journal := txn.Open(loc.ReplaceDir, loc.RunLock)
	if err := journal.CheckGuard(g); err != nil {
		return ReplaceJournalAbsent, err
	}
	rec, ok, err := journal.Latest()
	if err != nil {
		return ReplaceJournalAbsent, err
	}
	return classifyReplaceRecord(rec, ok, loc.RunID)
}

// ClassifyReplaceRecord is the PURE, lock-free domain classification of a session-replacement
// journal head (see ClassifyPairRecord): aborted/pending/misbound → NonTerminal/error.
func ClassifyReplaceRecord(rec txn.Record, ok bool, runID string) (ReplaceJournalClass, error) {
	return classifyReplaceRecord(rec, ok, runID)
}

func classifyReplaceRecord(rec txn.Record, ok bool, runID string) (ReplaceJournalClass, error) {
	if !ok {
		return ReplaceJournalAbsent, nil
	}
	// Bind the intent BEFORE the terminality branch: a pending replacement still has a
	// valid intent envelope+payload, so a corrupt or mis-bound record must fail here
	// whether or not it is complete (never silently classified as a clean class).
	if _, err := bindReplaceHead(rec, runID); err != nil {
		return ReplaceJournalAbsent, err
	}
	if rec.Complete && !rec.Aborted {
		return ReplaceJournalTerminal, nil
	}
	return ReplaceJournalNonTerminal, nil
}
