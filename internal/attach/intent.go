// Package attach bootstraps a run and registers the lead and pair terminals.
//
// First attach is a repo-scoped, journaled transaction: a fully deterministic
// BootstrapIntent is prepared READ-ONLY before the journal (every target identity
// — run/session/txn ids, snapshot paths+digests, resolved base commit, worktree
// locator, catalog ref — is fixed up front), then realized by idempotent
// Observe/Apply participants over that intent. No participant mints identity
// during Apply, so a crash between an applied effect and recorded progress is
// repaired forward: recovery reconstructs the identical plan from the intent and
// re-drives. The task and effective-policy snapshots are ordinary atomic input
// persistence (done here); only base-commit resolution and worktree creation are
// git seams that subject 04 fills behind a frozen interface.
package attach

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
)

// intentKind is the txn kind for a first-attach bootstrap.
const intentKind = "bootstrap"

// maxSnapshotBytes bounds an embedded input snapshot so both snapshots plus the
// rest of the intent stay under the journal's payload limit. It equals config's
// documented source bound.
const maxSnapshotBytes = config.MaxContractBytes

// BootstrapIntent is the deterministic, bounded target identity of a first
// attach, prepared read-only before the journal and immutable for the
// transaction's life. It carries every value the participants need, so no step
// resolves fresh identity after a mutation and recovery is exact.
type BootstrapIntent struct {
	RunID       string      `json:"run_id"`
	TxnID       string      `json:"txn_id"`
	OperationID string      `json:"operation_id"` // caller-stable idempotency key
	SessionID   string      `json:"session_id"`   // the lead's minted session
	Agent       state.Agent `json:"agent"`        // the lead agent
	CreatedUnix int64       `json:"created_unix"`

	RelDir string `json:"rel_dir"` // run directory, relative to the repo root

	// Input snapshots: the exact canonical bytes and their run-relative target
	// paths/digests. Apply writes the bytes and verifies the digest; Observe
	// confirms the target file holds exactly these bytes.
	TaskRelPath   string `json:"task_rel_path"`
	TaskDigest    string `json:"task_digest"`
	TaskCanonical []byte `json:"task_canonical"`

	PolicyRelPath   string `json:"policy_rel_path"`
	PolicyDigest    string `json:"policy_digest"`
	PolicyCanonical []byte `json:"policy_canonical"`

	// The effective policy, carried so state-init is deterministic without
	// re-deriving it; its canonical serialization is PolicyCanonical.
	EffectivePolicy config.RunPolicy `json:"effective_policy"`

	// Frozen workspace identity (resolved read-only during preparation). The
	// worktree is addressed by a run-relative locator + a run branch, never an
	// ephemeral absolute string; the base commit is the exact resolved OID.
	Base            string `json:"base"`
	BaseCommit      string `json:"base_commit"`
	WorktreeRelPath string `json:"worktree_rel_path"`
	RunBranch       string `json:"run_branch"`

	// Filesystem classification frozen at bootstrap.
	FSClass  string `json:"fs_class"`
	FSReason string `json:"fs_reason"`
	FSAck    bool   `json:"fs_acknowledged"`

	// Catalog allocation target: the expected catalog revision and the immutable
	// run ref.
	CatalogExpectedRevision uint64 `json:"catalog_expected_revision"`

	// Prior run to reconcile (clear) inside THIS bootstrap's journal, so a
	// terminal predecessor is cleared atomically-with-recovery only after this
	// bootstrap is prepared — a failed prepare never mutates the pointer. Empty
	// when there is no prior active run.
	ClearPriorRunID    string `json:"clear_prior_run_id"`
	ClearPriorRevision uint64 `json:"clear_prior_revision"`
}

// runRef is the immutable catalog allocation this intent commits.
func (in BootstrapIntent) runRef() state.RunRef {
	return state.RunRef{
		RunID:       in.RunID,
		RelDir:      in.RelDir,
		Base:        in.Base,
		BaseCommit:  in.BaseCommit,
		CreatedUnix: in.CreatedUnix,
	}
}

// marshal encodes the intent as the txn payload.
func (in BootstrapIntent) marshal() (json.RawMessage, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// decodeIntent strictly decodes and validates a bootstrap intent payload.
func decodeIntent(payload json.RawMessage) (BootstrapIntent, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var in BootstrapIntent
	if err := dec.Decode(&in); err != nil {
		return BootstrapIntent{}, fmt.Errorf("attach: decode intent: %w", err)
	}
	if err := in.validate(); err != nil {
		return BootstrapIntent{}, err
	}
	return in, nil
}

// validate enforces the intent's internal coherence AND the exact derived layout,
// so a malformed or forged journal payload can never drive a bootstrap that
// writes outside the run's own directory. Every path is required to be exactly
// the value derived from the run id — not merely "some local path".
func (in BootstrapIntent) validate() error {
	if !state.IsRunID(in.RunID) {
		return fmt.Errorf("attach: intent run_id is not canonical")
	}
	if !state.IsRunID(in.TxnID) {
		return fmt.Errorf("attach: intent txn_id is not canonical")
	}
	if !state.IsOperationID(in.OperationID) {
		return fmt.Errorf("attach: intent operation_id is not a minted operation id")
	}
	if !state.IsSessionID(in.SessionID) {
		return fmt.Errorf("attach: intent session_id is not a canonical minted id")
	}
	if in.Agent != state.AgentClaude && in.Agent != state.AgentCodex {
		return fmt.Errorf("attach: intent agent is unknown")
	}
	if in.CreatedUnix <= 0 {
		return fmt.Errorf("attach: intent created_unix must be positive")
	}
	// Derived layout: the run directory, snapshot paths, worktree, and branch are
	// EXACTLY the values derived from the run id, so a forged intent cannot target
	// another location in the repo.
	if in.RelDir != wantRelDir(in.RunID) {
		return fmt.Errorf("attach: intent rel_dir is not the derived run directory")
	}
	if in.TaskRelPath != "inputs/task.json" || in.PolicyRelPath != "inputs/policy.json" {
		return fmt.Errorf("attach: intent snapshot paths are not the derived paths")
	}
	if in.WorktreeRelPath != in.RelDir+"/worktree" {
		return fmt.Errorf("attach: intent worktree_rel_path is not under the run directory")
	}
	if in.RunBranch != wantRunBranch(in.RunID) {
		return fmt.Errorf("attach: intent run_branch is not the derived branch")
	}
	if err := validateSnapshotField("task", in.TaskDigest, in.TaskCanonical); err != nil {
		return err
	}
	if err := validateSnapshotField("policy", in.PolicyDigest, in.PolicyCanonical); err != nil {
		return err
	}
	// The effective policy is DERIVED from the exact policy snapshot bytes, so the
	// two can never disagree; the task snapshot parses and is coherent with it.
	policy, err := config.ParseRunPolicy(in.PolicyCanonical)
	if err != nil {
		return fmt.Errorf("attach: intent policy bytes: %w", err)
	}
	if !reflect.DeepEqual(policy, in.EffectivePolicy) {
		return fmt.Errorf("attach: intent effective_policy is not the parsed policy snapshot")
	}
	task, err := config.ParseTaskContract(in.TaskCanonical)
	if err != nil {
		return fmt.Errorf("attach: intent task bytes: %w", err)
	}
	if err := config.ValidateEffective(task, policy); err != nil {
		return fmt.Errorf("attach: intent task/policy: %w", err)
	}
	if in.Base != policy.BaseBranch {
		return fmt.Errorf("attach: intent base must equal the policy base branch")
	}
	if !isGitOID(in.BaseCommit) {
		return fmt.Errorf("attach: intent base_commit is not a git object id")
	}
	// Filesystem coherence: supported-local is never acknowledged; unknown is only
	// permitted with the acknowledge policy AND the ack flag set.
	if in.FSReason == "" {
		return fmt.Errorf("attach: intent fs reason is required")
	}
	switch in.FSClass {
	case "supported-local":
		if in.FSAck {
			return fmt.Errorf("attach: supported-local must not be acknowledged")
		}
	case "unknown":
		if !in.FSAck || policy.UnknownFSPolicy != config.UnknownFSAcknowledge {
			return fmt.Errorf("attach: unknown filesystem requires the acknowledge policy and ack flag")
		}
	default:
		return fmt.Errorf("attach: intent fs class is not runnable")
	}
	// A prior run to clear must be a real run id at a positive revision, and must
	// not be this run.
	if in.ClearPriorRunID != "" {
		if !state.IsRunID(in.ClearPriorRunID) || in.ClearPriorRevision == 0 || in.ClearPriorRunID == in.RunID {
			return fmt.Errorf("attach: intent clear_prior is not a valid distinct prior run")
		}
	} else if in.ClearPriorRevision != 0 {
		return fmt.Errorf("attach: intent clear_prior revision without a run id")
	}
	return nil
}

func wantRelDir(runID string) string    { return ".claudex/runs/" + runID }
func wantRunBranch(runID string) string { return "claudex/" + runID }

func validateSnapshotField(name, digest string, canonical []byte) error {
	if !state.IsHex64(digest) {
		return fmt.Errorf("attach: intent %s snapshot digest is not a sha256", name)
	}
	if len(canonical) == 0 || len(canonical) > maxSnapshotBytes {
		return fmt.Errorf("attach: intent %s snapshot bytes out of range", name)
	}
	if config.Hash(canonical) != digest {
		return fmt.Errorf("attach: intent %s snapshot digest disagrees with its bytes", name)
	}
	return nil
}

func isGitOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
