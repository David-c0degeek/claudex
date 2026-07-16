package attach

import (
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
// moving branch. Subject 04 supplies the real git implementation.
type BaseResolver interface {
	ResolveBase(repoDir, baseBranch string) (commit string, err error)
}

// WorktreeProvisioner creates and verifies the run's isolated worktree at the
// frozen run-relative locator + branch from the intent's exact base commit. It is
// an idempotent Observe/Apply participant; subject 04 fills the real git.
type WorktreeProvisioner interface {
	ObserveWorktree(repoDir string, in BootstrapIntent) (txn.StepStatus, error)
	ApplyWorktree(repoDir string, in BootstrapIntent) error
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
func FirstAttach(req FirstAttachRequest) (FirstAttachResult, error) {
	policy, err := req.validate()
	if err != nil {
		return FirstAttachResult{}, err
	}
	classifier := req.Classifier
	if classifier == nil {
		classifier = realClassifier{}
	}
	lay := layoutFor(req.RepoDir)

	// Read-only repo preflight BEFORE creating .claudex or taking the lock: refuse
	// a pre-pivot legacy run and an unsupported filesystem before any side effect.
	if err := repoPreflight(lay, classifier, policy); err != nil {
		return FirstAttachResult{}, err
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
	journal := txn.Open(lay.bootstrapJournal, lay.repoLock)

	// Recover a pending bootstrap forward first: a crash between an applied effect
	// and recorded progress is repaired, and the same session id is returned.
	rec, recovered, err := journal.Recover(g, func(in txn.Intent) (txn.Plan, error) {
		return planFor(lay, seams, g, in)
	})
	if err != nil {
		return FirstAttachResult{}, err
	}
	if recovered {
		bi, derr := decodeIntent(rec.Intent.Payload)
		if derr != nil {
			return FirstAttachResult{}, derr
		}
		return resultFor(bi), nil
	}

	// A current active run means join/reattach, not a second bootstrap — except a
	// lost response after a completed bootstrap: the same operation id returns the
	// incumbent lead session (never a forced replacement).
	cur, ok, cerr := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if cerr != nil {
		return FirstAttachResult{}, cerr
	}
	if ok && cur.Active {
		if cur.OperationID == req.OperationID {
			return incumbentResult(cur), nil
		}
		return FirstAttachResult{}, fmt.Errorf("%w: %s", ErrRunExists, cur.RunID)
	}

	intent, err := prepare(lay, req, policy, classifier)
	if err != nil {
		return FirstAttachResult{}, err
	}
	plan, err := planFor(lay, seams, g, txn.Intent{
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

// repoPreflight runs the read-only refusals before any mutation: a pre-pivot
// legacy run named by .claudex/current, and an unsupported filesystem.
func repoPreflight(lay layout, classifier Classifier, policy config.RunPolicy) error {
	if err := legacyRepoRefusal(lay.repoDir); err != nil {
		return err
	}
	res, err := classifier.Classify(filepath.Join(lay.repoDir, ".claudex"))
	if err != nil {
		return err
	}
	_, _, _, err = decideFS(res, policy)
	return err
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
	switch res.Class {
	case fsclass.SupportedLocal:
		return "supported-local", nonEmpty(res.Reason, "local fixed drive"), false, nil
	case fsclass.Unknown:
		if pol.UnknownFSPolicy != config.UnknownFSAcknowledge {
			return "", "", false, &UnsupportedFSError{Class: "unknown", Reason: nonEmpty(res.Reason, "unclassified filesystem"), AckRequired: true}
		}
		return "unknown", nonEmpty(res.Reason, "unknown filesystem, acknowledged"), true, nil
	default:
		return "", "", false, &UnsupportedFSError{Class: res.Class.String(), Reason: nonEmpty(res.Reason, "network/remote filesystem"), AckRequired: false}
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

func incumbentResult(cur state.CurrentRun) FirstAttachResult {
	return FirstAttachResult{
		RunID:     cur.RunID,
		SessionID: cur.LeadSessionID,
		Role:      state.SlotLead,
		Agent:     cur.LeadAgent,
		JoinArgv:  joinArgv(cur.RunID, cur.LeadAgent),
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

func (req FirstAttachRequest) validate() (config.RunPolicy, error) {
	if req.RepoDir == "" {
		return config.RunPolicy{}, fmt.Errorf("attach: repo dir is required")
	}
	if req.Agent != state.AgentClaude && req.Agent != state.AgentCodex {
		return config.RunPolicy{}, fmt.Errorf("attach: agent must be claude or codex")
	}
	if !state.IsRunID(req.OperationID) {
		return config.RunPolicy{}, fmt.Errorf("attach: a canonical operation_id is required")
	}
	if req.CreatedUnix <= 0 {
		return config.RunPolicy{}, fmt.Errorf("attach: created_unix must be positive")
	}
	if req.RNG == nil {
		return config.RunPolicy{}, fmt.Errorf("attach: an RNG is required")
	}
	if req.Base == nil || req.Worktree == nil {
		return config.RunPolicy{}, fmt.Errorf("attach: base resolver and worktree provisioner are required")
	}
	// Parse the exact source bytes and validate the task and policy together, so a
	// run is never bootstrapped against an invalid contract, and the effective
	// policy is DERIVED from the snapshot bytes (never a separate claim).
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
func prepare(lay layout, req FirstAttachRequest, policy config.RunPolicy, classifier Classifier) (BootstrapIntent, error) {
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

	baseCommit, err := req.Base.ResolveBase(lay.repoDir, policy.BaseBranch)
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
