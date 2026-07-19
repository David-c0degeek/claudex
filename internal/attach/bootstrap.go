package attach

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/fsclass"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/legacy"
	"github.com/David-c0degeek/claudex/internal/redact"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// ErrRunExists means a run is already allocated for this repository, so a first
// attach cannot bootstrap another; the caller should join or reattach instead.
var ErrRunExists = errors.New("attach: a run is already allocated for this repository")

// ErrUnsupportedFS means the run location is on a known-unsupported filesystem, or
// an unknown one without the acknowledge policy.
var ErrUnsupportedFS = errors.New("attach: unsupported filesystem for a run")

// BaseResolver resolves the policy base branch to an EXACT commit OID during
// read-only preparation, so every Apply works from a frozen OID and never a
// moving branch. The context governs cancellation of the underlying git command:
// a cancelled resolution fails read-only, before any journal record exists.
type BaseResolver interface {
	ResolveBase(ctx context.Context, repoDir, baseBranch string) (commit string, err error)
}

// WorktreeProvisioner creates and verifies the run's isolated worktree at the
// frozen run-relative locator + branch from the intent's exact base commit. It is
// an idempotent Observe/Apply/Confirm participant driven inside the bootstrap
// journal, so a cancelled Apply (via the context) halts with no recorded progress
// and the next mutator recovers it forward. ConfirmWorktree re-confirms the
// provisioned identity AND its OS durability under the held guard before the
// journal records the step — Observe==Applied proves identity, not durability.
type WorktreeProvisioner interface {
	ObserveWorktree(ctx context.Context, repoDir string, in BootstrapIntent) (txn.StepStatus, error)
	ApplyWorktree(ctx context.Context, repoDir string, in BootstrapIntent) error
	ConfirmWorktree(ctx context.Context, repoDir string, in BootstrapIntent) error
}

// Classifier classifies the durability-support class of the run location. The
// default wraps fsclass.Classify; tests inject a fake to exercise the refusal
// paths deterministically.
type Classifier interface {
	Classify(path string) (fsclass.Result, error)
}

type realClassifier struct{}

func (realClassifier) Classify(path string) (fsclass.Result, error) { return fsclass.Classify(path) }

// UnsupportedFSError is the structured filesystem refusal the CLI messages from:
// the class, the reason, and whether an acknowledge policy would have allowed it.
// It unwraps to the single ErrUnsupportedFS sentinel.
type UnsupportedFSError struct {
	Class       string
	Reason      string
	AckRequired bool
}

func (e *UnsupportedFSError) Error() string {
	if e.AckRequired {
		return fmt.Sprintf("attach: %s filesystem needs unknown_fs_policy=acknowledge (%s)", e.Class, e.Reason)
	}
	return fmt.Sprintf("attach: %s filesystem is unsupported for a run (%s)", e.Class, e.Reason)
}

func (e *UnsupportedFSError) Unwrap() error { return ErrUnsupportedFS }

// FirstAttachRequest is a first-attach invocation. The caller supplies the exact
// task-contract and effective-policy source bytes; the effective policy is
// DERIVED from PolicyCanonical (so the persisted snapshot and the run's policy
// can never disagree), and the two are checked together via ValidateEffective.
// OperationID is a caller-stable idempotency key so a lost response after a
// completed bootstrap returns the same run rather than forcing a new one. Clock,
// RNG, base resolver, worktree provisioner, and filesystem classifier are
// injected.
type FirstAttachRequest struct {
	RepoDir         string
	Agent           state.Agent
	OperationID     string
	TaskCanonical   []byte
	PolicyCanonical []byte
	CreatedUnix     int64
	RNG             io.Reader
	Base            BaseResolver
	Worktree        WorktreeProvisioner
	Classifier      Classifier
}

// FirstAttachResult is the outcome the initiator receives: the run and its lead
// session, plus the argv the pair uses to join (structured, so quoting is never
// authoritative).
type FirstAttachResult struct {
	RunID     string
	SessionID string
	Role      state.SlotRole
	Agent     state.Agent
	JoinArgv  []string
}

// layout resolves the repo-scoped paths a bootstrap uses. The lock path is never
// persisted; it only serializes writers.
type layout struct {
	repoDir          string
	repoLock         string
	catalogDir       string
	currentRunDir    string
	bootstrapJournal string
}

func layoutFor(repoDir string) layout {
	base := filepath.Join(repoDir, ".claudex")
	return layout{
		repoDir:          repoDir,
		repoLock:         filepath.Join(base, "repo.lock"),
		catalogDir:       filepath.Join(base, "catalog"),
		currentRunDir:    filepath.Join(base, "active-run"),
		bootstrapJournal: filepath.Join(base, "bootstrap"),
	}
}

func (l layout) runDir(relDir string) string {
	return filepath.Join(l.repoDir, filepath.FromSlash(relDir))
}

// FirstAttach bootstraps a run under the repository allocation lock. It first
// recovers any pending bootstrap forward (an idempotent retry returns the same
// identities), then — only when no run exists — prepares a deterministic intent
// read-only and journals the bootstrap to completion.
func FirstAttach(ctx context.Context, req FirstAttachRequest) (FirstAttachResult, error) {
	if err := req.validateMinimal(); err != nil {
		return FirstAttachResult{}, err
	}
	classifier := req.Classifier
	if classifier == nil {
		classifier = realClassifier{}
	}
	lay := layoutFor(req.RepoDir)

	// Read-only legacy refusal BEFORE any mutation.
	if err := legacyRepoRefusal(lay.repoDir); err != nil {
		return FirstAttachResult{}, err
	}

	// Lock-free probe: if neither a pending bootstrap nor an active run exists, this
	// is definitely a new bootstrap, so parse the inputs and classify the filesystem
	// READ-ONLY and refuse an unsupported one BEFORE creating .claudex or the lock.
	journal := txn.Open(lay.bootstrapJournal, lay.repoLock)
	current := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock)
	recoveryPath, err := hasPendingOrActive(journal, current)
	if err != nil {
		return FirstAttachResult{}, err
	}
	if !recoveryPath {
		policy, verr := req.validateForNewBootstrap()
		if verr != nil {
			return FirstAttachResult{}, verr
		}
		res, cerr := classifyRunLocation(classifier, lay)
		if cerr != nil {
			return FirstAttachResult{}, cerr // an inability to classify is fail-closed, cause preserved
		}
		if _, _, _, ferr := decideFS(res, policy); ferr != nil {
			return FirstAttachResult{}, ferr
		}
	}

	if err := os.MkdirAll(filepath.Dir(lay.repoLock), 0o700); err != nil {
		return FirstAttachResult{}, err
	}
	g, ok, err := genstore.Acquire(lay.repoLock)
	if err != nil {
		return FirstAttachResult{}, err
	}
	if !ok {
		return FirstAttachResult{}, genstore.ErrBusy
	}
	defer g.Release()

	seams := seams{base: req.Base, worktree: req.Worktree}

	// Recover a pending bootstrap forward first (needs only the worktree seam, no
	// fresh inputs). The recovered run's identity is returned ONLY to the operation
	// that started it; a different operation completes the recovery but is told the
	// run exists.
	rec, recovered, err := journal.Recover(g, func(in txn.Intent) (txn.Plan, error) {
		return planFor(ctx, lay, seams, g, in)
	})
	if err != nil {
		return FirstAttachResult{}, err
	}
	if recovered {
		bi, derr := decodeIntent(rec.Intent.Payload)
		if derr != nil {
			return FirstAttachResult{}, derr
		}
		if bi.OperationID != req.OperationID {
			return FirstAttachResult{}, fmt.Errorf("%w: %s", ErrRunExists, bi.RunID)
		}
		cur, ok, lerr := current.Load()
		if lerr != nil {
			return FirstAttachResult{}, lerr
		}
		if !ok || !cur.Active {
			return FirstAttachResult{}, fmt.Errorf("%w: %s", ErrRunExists, bi.RunID)
		}
		return sameOpResult(lay, journal, cur)
	}

	// An active run: the same operation returns the incumbent (a combined,
	// identity-bound read verified still current in the Registry — never a
	// stale/replaced session); a different operation is told the run exists —
	// unless the active run has reached an ABSORBING terminal, in which case it is
	// reconciled by a journaled clear inside the new bootstrap (never cleared
	// eagerly, so a failed prepare leaves the pointer untouched).
	cur, hasCur, cerr := current.Load()
	if cerr != nil {
		return FirstAttachResult{}, cerr
	}
	var clearPrior *state.CurrentRun
	if hasCur && cur.Active {
		if cur.OperationID == req.OperationID {
			return sameOpResult(lay, journal, cur)
		}
		terminal, terr := currentRunTerminal(lay, cur)
		if terr != nil {
			return FirstAttachResult{}, terr
		}
		if !terminal {
			return FirstAttachResult{}, fmt.Errorf("%w: %s", ErrRunExists, cur.RunID)
		}
		c := cur
		clearPrior = &c
	}

	// New bootstrap under the guard: full input validation + fs decision (revalidated).
	policy, err := req.validateForNewBootstrap()
	if err != nil {
		return FirstAttachResult{}, err
	}
	intent, err := prepare(ctx, lay, req, policy, classifier, clearPrior)
	if err != nil {
		return FirstAttachResult{}, err
	}
	plan, err := planFor(ctx, lay, seams, g, txn.Intent{
		Version: txn.IntentVersion, Kind: intentKind, TxnID: intent.TxnID,
		ExpectedStateRevision: 0, Payload: mustMarshalIntent(intent),
	})
	if err != nil {
		return FirstAttachResult{}, err
	}
	if _, err := journal.Run(g, plan); err != nil {
		return FirstAttachResult{}, err
	}
	return resultFor(intent), nil
}

// hasPendingOrActive lock-free reports whether a pending bootstrap or an active
// run exists, so a definite new bootstrap can be refused before any mutation.
func hasPendingOrActive(journal *txn.Journal, current *state.CurrentRunStore) (bool, error) {
	if rec, ok, err := journal.Latest(); err != nil {
		return false, err
	} else if ok && !rec.Terminal() {
		return true, nil
	}
	if cur, ok, err := current.Load(); err != nil {
		return false, err
	} else if ok && cur.Active {
		return true, nil
	}
	return false, nil
}

// classifyRunLocation classifies the filesystem backing the run location and
// PROPAGATES a classifier error (an inability to classify is fail-closed, but it
// is never fabricated into a positive known-unsupported classification).
func classifyRunLocation(classifier Classifier, lay layout) (fsclass.Result, error) {
	return classifier.Classify(filepath.Join(lay.repoDir, ".claudex"))
}

// currentRunTerminal reports whether the active run has reached an ABSORBING
// terminal (safe to reconcile). It is identity-bound: the pointer's locator must
// be the derived one, and the loaded RunState's run id must equal the pointer's,
// so a cross-wired or resurrectable run is never treated as clearable.
func currentRunTerminal(lay layout, cur state.CurrentRun) (bool, error) {
	if cur.RelDir != state.RunDirRelFor(cur.RunID) {
		return false, nil
	}
	rs, ok, err := state.Open(filepath.Join(lay.runDir(cur.RelDir), "state"), lay.repoLock).Load()
	if err != nil {
		return false, err
	}
	if !ok || rs.RunID != cur.RunID {
		return false, nil
	}
	return state.IsAbsorbingLifecycle(rs.Lifecycle), nil
}

// sameOpResult is the single combined-read for an idempotent same-operation
// return. It binds the terminal bootstrap journal, the active-run pointer, and the
// Registry to ONE run identity before returning any credential, and fails closed
// (ErrRunExists) on any mismatch or a replaced session — so cross-wired stores can
// never hand back the wrong run's session.
func sameOpResult(lay layout, journal *txn.Journal, cur state.CurrentRun) (FirstAttachResult, error) {
	rec, ok, err := journal.Latest()
	if err != nil {
		return FirstAttachResult{}, err
	}
	// A COMPLETED (never aborted) bootstrap with a fully-bound envelope — an aborted
	// transaction must never yield a session even if other stores were cross-wired.
	if !ok || !rec.Complete || rec.Aborted ||
		rec.Intent.Version != txn.IntentVersion || rec.Intent.Kind != intentKind || rec.Intent.ExpectedStateRevision != 0 {
		return FirstAttachResult{}, fmt.Errorf("%w: %s", ErrRunExists, cur.RunID)
	}
	bi, err := decodeIntent(rec.Intent.Payload)
	if err != nil {
		return FirstAttachResult{}, err
	}
	// Envelope <-> payload, and journal <-> active-run pointer identity.
	if rec.TxnID() != bi.TxnID || bi.RunID != cur.RunID || bi.RelDir != cur.RelDir || bi.OperationID != cur.OperationID {
		return FirstAttachResult{}, fmt.Errorf("%w: %s", ErrRunExists, cur.RunID)
	}
	// Journal <-> Registry identity/role/agent, and the session is still current.
	reg, ok, err := state.OpenRegistry(filepath.Join(lay.runDir(bi.RelDir), "registry"), lay.repoLock).Load()
	if err != nil {
		return FirstAttachResult{}, err
	}
	if !ok || reg.RunID != bi.RunID {
		return FirstAttachResult{}, fmt.Errorf("%w: %s", ErrRunExists, cur.RunID)
	}
	r := reg.Resolve(bi.SessionID)
	if r.Status != state.RegCurrent || r.Role != state.SlotLead || r.Agent != bi.Agent {
		return FirstAttachResult{}, fmt.Errorf("%w: %s", ErrRunExists, cur.RunID)
	}
	return resultFor(bi), nil
}

// legacyRepoRefusal refuses a pre-pivot Python run: the repo-level
// .claudex/current pointer names a run directory whose state.json the legacy
// guard recognizes. Absent pointer is safe.
func legacyRepoRefusal(repoDir string) error {
	data, err := os.ReadFile(filepath.Join(repoDir, ".claudex", "current"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return nil
	}
	if !state.IsRunID(name) {
		return fmt.Errorf("attach: legacy current pointer is not a safe run name")
	}
	return legacy.CheckRunDir(filepath.Join(repoDir, ".claudex", "runs", name))
}

// decideFS maps a filesystem classification to the frozen fs decision, returning
// a typed UnsupportedFSError for a known-unsupported filesystem or an unknown one
// without the acknowledge policy.
func decideFS(res fsclass.Result, pol config.RunPolicy) (class, reason string, ack bool, err error) {
	// The classifier reason is external free text; redact it before it reaches a
	// typed error or the frozen intent (which persists in the journal).
	r := redact.Text(res.Reason)
	switch res.Class {
	case fsclass.SupportedLocal:
		return "supported-local", nonEmpty(r, "local fixed drive"), false, nil
	case fsclass.Unknown:
		if pol.UnknownFSPolicy != config.UnknownFSAcknowledge {
			return "", "", false, &UnsupportedFSError{Class: "unknown", Reason: nonEmpty(r, "unclassified filesystem"), AckRequired: true}
		}
		return "unknown", nonEmpty(r, "unknown filesystem, acknowledged"), true, nil
	default:
		return "", "", false, &UnsupportedFSError{Class: res.Class.String(), Reason: nonEmpty(r, "network/remote filesystem"), AckRequired: false}
	}
}

func resultFor(in BootstrapIntent) FirstAttachResult {
	return FirstAttachResult{
		RunID:     in.RunID,
		SessionID: in.SessionID,
		Role:      state.SlotLead,
		Agent:     in.Agent,
		JoinArgv:  joinArgv(in.RunID, in.Agent),
	}
}

// joinArgv names the EXACT run so the pair joins the right one even once the
// catalog holds history.
func joinArgv(runID string, leadAgent state.Agent) []string {
	return []string{"attach", "--repo", ".", "--run", runID, "--agent", string(complementaryAgent(leadAgent)), "--role", "pair"}
}

func complementaryAgent(a state.Agent) state.Agent {
	if a == state.AgentClaude {
		return state.AgentCodex
	}
	return state.AgentClaude
}

// validateMinimal is the authority a RECOVERY needs: just the repo, the
// operation id, and the worktree participant — never fresh task/policy bytes,
// RNG, or base resolver, so a lost response is recoverable even if the source
// files changed or vanished.
func (req FirstAttachRequest) validateMinimal() error {
	if req.RepoDir == "" {
		return fmt.Errorf("attach: repo dir is required")
	}
	if !state.IsOperationID(req.OperationID) {
		return fmt.Errorf("attach: a minted operation_id is required")
	}
	if req.Worktree == nil {
		return fmt.Errorf("attach: a worktree provisioner is required")
	}
	return nil
}

// validateForNewBootstrap is the full authority a NEW bootstrap needs: it parses
// the exact source bytes and validates the task and policy together (the effective
// policy is DERIVED from the snapshot bytes, never a separate claim).
func (req FirstAttachRequest) validateForNewBootstrap() (config.RunPolicy, error) {
	if req.Agent != state.AgentClaude && req.Agent != state.AgentCodex {
		return config.RunPolicy{}, fmt.Errorf("attach: agent must be claude or codex")
	}
	if req.CreatedUnix <= 0 {
		return config.RunPolicy{}, fmt.Errorf("attach: created_unix must be positive")
	}
	if req.RNG == nil {
		return config.RunPolicy{}, fmt.Errorf("attach: an RNG is required")
	}
	if req.Base == nil {
		return config.RunPolicy{}, fmt.Errorf("attach: a base resolver is required")
	}
	task, err := config.ParseTaskContract(req.TaskCanonical)
	if err != nil {
		return config.RunPolicy{}, fmt.Errorf("attach: %w", err)
	}
	policy, err := config.ParseRunPolicy(req.PolicyCanonical)
	if err != nil {
		return config.RunPolicy{}, fmt.Errorf("attach: %w", err)
	}
	if err := config.ValidateEffective(task, policy); err != nil {
		return config.RunPolicy{}, fmt.Errorf("attach: %w", err)
	}
	return policy, nil
}

// prepare builds the deterministic intent under the repo guard. It REVALIDATES
// the legacy refusal (the read-only preflight ran before the lock), mints every
// identity, classifies the run location, and resolves the base commit from a
// frozen OID — so every downstream Apply and any recovery works from exactly
// these values.
func prepare(ctx context.Context, lay layout, req FirstAttachRequest, policy config.RunPolicy, classifier Classifier, clearPrior *state.CurrentRun) (BootstrapIntent, error) {
	if err := legacyRepoRefusal(lay.repoDir); err != nil {
		return BootstrapIntent{}, err
	}
	runID, err := mintID("run-", req.RNG)
	if err != nil {
		return BootstrapIntent{}, err
	}
	sessionID, err := state.MintSessionID(req.RNG, nil)
	if err != nil {
		return BootstrapIntent{}, err
	}
	txnID, err := mintID("boot-", req.RNG)
	if err != nil {
		return BootstrapIntent{}, err
	}

	relDir := wantRelDir(runID)
	runDir := lay.runDir(relDir)

	if err := legacy.CheckRunDir(runDir); err != nil { // the candidate target
		return BootstrapIntent{}, err
	}
	res, err := classifier.Classify(filepath.Dir(runDir))
	if err != nil {
		return BootstrapIntent{}, err
	}
	fsClass, fsReason, fsAck, err := decideFS(res, policy)
	if err != nil {
		return BootstrapIntent{}, err
	}

	baseCommit, err := req.Base.ResolveBase(ctx, lay.repoDir, policy.BaseBranch)
	if err != nil {
		return BootstrapIntent{}, fmt.Errorf("attach: resolve base: %w", err)
	}

	in := BootstrapIntent{
		RunID:                   runID,
		TxnID:                   txnID,
		OperationID:             req.OperationID,
		SessionID:               sessionID,
		Agent:                   req.Agent,
		CreatedUnix:             req.CreatedUnix,
		RelDir:                  relDir,
		TaskRelPath:             "inputs/task.json",
		TaskDigest:              config.Hash(req.TaskCanonical),
		TaskCanonical:           req.TaskCanonical,
		PolicyRelPath:           "inputs/policy.json",
		PolicyDigest:            config.Hash(req.PolicyCanonical),
		PolicyCanonical:         req.PolicyCanonical,
		EffectivePolicy:         policy,
		Base:                    policy.BaseBranch,
		BaseCommit:              baseCommit,
		WorktreeRelPath:         relDir + "/worktree",
		RunBranch:               wantRunBranch(runID),
		FSClass:                 fsClass,
		FSReason:                fsReason,
		FSAck:                   fsAck,
		CatalogExpectedRevision: 0,
	}
	if clearPrior != nil {
		in.ClearPriorRunID = clearPrior.RunID
		in.ClearPriorRevision = clearPrior.Revision
	}
	if cat, ok, cerr := state.OpenCatalog(lay.catalogDir, lay.repoLock).Load(); cerr != nil {
		return BootstrapIntent{}, cerr
	} else if ok {
		in.CatalogExpectedRevision = cat.Revision
	}
	if err := in.validate(); err != nil {
		return BootstrapIntent{}, err
	}
	return in, nil
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func mintID(prefix string, rng io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rng, b[:]); err != nil {
		return "", fmt.Errorf("attach: mint id: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func mustMarshalIntent(in BootstrapIntent) []byte {
	b, _ := in.marshal()
	return b
}
