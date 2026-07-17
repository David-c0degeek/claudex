package attach

import (
	"fmt"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// RunLocation is the canonical, attach-derived location of a bound run. Every path is
// derived here from the repository layout so a consumer (the coordinator) never joins
// run paths of its own — it opens exactly these stores under RunLock.
type RunLocation struct {
	RunID       string
	RelDir      string // repo-relative run directory
	RunDir      string // absolute run directory
	RunLock     string // the per-run mutation lock (the two-tier protocol's inner lock)
	StateDir    string // the RunState genstore directory
	RegistryDir string // the Registry genstore directory
	AttachDir   string // the pair-attach transaction journal directory
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
	return RunLocation{
		RunID:       runID,
		RelDir:      rel,
		RunDir:      runDir,
		RunLock:     runLock(runDir),
		StateDir:    filepath.Join(runDir, "state"),
		RegistryDir: filepath.Join(runDir, "registry"),
		AttachDir:   lay.attachJournalDir(runDir),
	}
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

// ClassifyPairJournal classifies a resolved run's pair-attach journal head. Before
// declaring a complete record terminal it binds the envelope to its decoded
// pair-intent payload — version and kind, txn id, expected state revision, and run id
// — so a corrupt or mis-bound record is never mistaken for a completed pairing. It is
// read-only and lock-free.
func ClassifyPairJournal(loc RunLocation) (JournalClass, error) {
	rec, ok, err := txn.Open(loc.AttachDir, loc.RunLock).Latest()
	if err != nil {
		return JournalAbsent, err
	}
	return classifyPairRecord(rec, ok, loc.RunID)
}

func classifyPairRecord(rec txn.Record, ok bool, runID string) (JournalClass, error) {
	if !ok {
		return JournalAbsent, nil
	}
	if !rec.Terminal() || rec.Aborted {
		// An in-flight transaction needs recovery; an aborted one never paired. Neither
		// is a completed pairing the consumer may open.
		return JournalNonTerminal, nil
	}
	// A complete record: bind its envelope to the decoded payload before trusting it.
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
	return JournalTerminal, nil
}
