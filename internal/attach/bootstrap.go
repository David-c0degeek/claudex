package attach

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

// FirstAttachRequest is a first-attach invocation. The caller supplies the
// canonical task bytes and the validated effective policy (its canonical
// serialization must equal PolicyCanonical); clock and RNG are injected.
type FirstAttachRequest struct {
	RepoDir         string
	Agent           state.Agent
	TaskCanonical   []byte
	PolicyCanonical []byte
	EffectivePolicy config.RunPolicy
	CreatedUnix     int64
	RNG             io.Reader
	Base            BaseResolver
	Worktree        WorktreeProvisioner
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
	bootstrapJournal string
}

func layoutFor(repoDir string) layout {
	base := filepath.Join(repoDir, ".claudex")
	return layout{
		repoDir:          repoDir,
		repoLock:         filepath.Join(base, "repo.lock"),
		catalogDir:       filepath.Join(base, "catalog"),
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
	if err := req.validate(); err != nil {
		return FirstAttachResult{}, err
	}
	lay := layoutFor(req.RepoDir)
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

	// No pending bootstrap: a run already allocated means "join/reattach", not a
	// second bootstrap.
	if cat, ok, cerr := state.OpenCatalog(lay.catalogDir, lay.repoLock).Load(); cerr != nil {
		return FirstAttachResult{}, cerr
	} else if ok && len(cat.Runs) > 0 {
		return FirstAttachResult{}, ErrRunExists
	}

	intent, err := prepare(lay, req)
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

func resultFor(in BootstrapIntent) FirstAttachResult {
	return FirstAttachResult{
		RunID:     in.RunID,
		SessionID: in.SessionID,
		Role:      state.SlotLead,
		Agent:     in.Agent,
		JoinArgv:  []string{"attach", "--repo", ".", "--agent", string(complementaryAgent(in.Agent)), "--role", "pair"},
	}
}

func complementaryAgent(a state.Agent) state.Agent {
	if a == state.AgentClaude {
		return state.AgentCodex
	}
	return state.AgentClaude
}

func (req FirstAttachRequest) validate() error {
	if req.RepoDir == "" {
		return fmt.Errorf("attach: repo dir is required")
	}
	if req.Agent != state.AgentClaude && req.Agent != state.AgentCodex {
		return fmt.Errorf("attach: agent must be claude or codex")
	}
	if req.CreatedUnix <= 0 {
		return fmt.Errorf("attach: created_unix must be positive")
	}
	if req.RNG == nil {
		return fmt.Errorf("attach: an RNG is required")
	}
	if req.Base == nil || req.Worktree == nil {
		return fmt.Errorf("attach: base resolver and worktree provisioner are required")
	}
	if config.Hash(req.PolicyCanonical) == "" || len(req.TaskCanonical) == 0 {
		return fmt.Errorf("attach: task and policy bytes are required")
	}
	return nil
}

// prepare builds the deterministic intent read-only: it runs the legacy refusal
// and filesystem classification before any side effect, mints every identity,
// and resolves the base commit from a frozen OID.
func prepare(lay layout, req FirstAttachRequest) (BootstrapIntent, error) {
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

	relDir := ".claudex/runs/" + runID
	runDir := lay.runDir(relDir)

	// Legacy refusal + fs classification BEFORE any mutation.
	if err := legacy.CheckRunDir(runDir); err != nil {
		return BootstrapIntent{}, err
	}
	fsres, err := fsclass.Classify(filepath.Dir(runDir))
	if err != nil {
		return BootstrapIntent{}, err
	}
	fsClass, fsReason, fsAck, err := classifyFS(fsres, req.EffectivePolicy)
	if err != nil {
		return BootstrapIntent{}, err
	}

	baseCommit, err := req.Base.ResolveBase(lay.repoDir, req.EffectivePolicy.BaseBranch)
	if err != nil {
		return BootstrapIntent{}, fmt.Errorf("attach: resolve base: %w", err)
	}

	in := BootstrapIntent{
		RunID:                   runID,
		TxnID:                   txnID,
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
		EffectivePolicy:         req.EffectivePolicy,
		Base:                    req.EffectivePolicy.BaseBranch,
		BaseCommit:              baseCommit,
		WorktreeRelPath:         relDir + "/worktree",
		RunBranch:               "claudex/" + runID,
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

// classifyFS maps a filesystem result to the frozen fs decision, refusing a
// known-unsupported filesystem and requiring the acknowledge policy for unknown.
func classifyFS(res fsclass.Result, pol config.RunPolicy) (class, reason string, ack bool, err error) {
	switch res.Class {
	case fsclass.SupportedLocal:
		return "supported-local", nonEmpty(res.Reason, "local fixed drive"), false, nil
	case fsclass.Unknown:
		if pol.UnknownFSPolicy != config.UnknownFSAcknowledge {
			return "", "", false, fmt.Errorf("%w: unknown filesystem needs unknown_fs_policy=acknowledge", ErrUnsupportedFS)
		}
		return "unknown", nonEmpty(res.Reason, "unknown filesystem, acknowledged"), true, nil
	default:
		return "", "", false, fmt.Errorf("%w: %s", ErrUnsupportedFS, res.Class.String())
	}
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
