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

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
)

// intentKind is the txn kind for a first-attach bootstrap.
const intentKind = "bootstrap"

// maxSnapshotBytes bounds an embedded input snapshot so the whole intent stays
// well under the journal's payload limit.
const maxSnapshotBytes = 16 * 1024

// BootstrapIntent is the deterministic, bounded target identity of a first
// attach, prepared read-only before the journal and immutable for the
// transaction's life. It carries every value the participants need, so no step
// resolves fresh identity after a mutation and recovery is exact.
type BootstrapIntent struct {
	RunID       string      `json:"run_id"`
	TxnID       string      `json:"txn_id"`
	SessionID   string      `json:"session_id"` // the lead's minted session
	Agent       state.Agent `json:"agent"`      // the lead agent
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
	// run ref committed last (the discoverability commit).
	CatalogExpectedRevision uint64 `json:"catalog_expected_revision"`
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

// validate enforces the intent's internal coherence, so a malformed or tampered
// journal payload can never drive a bootstrap.
func (in BootstrapIntent) validate() error {
	if !state.IsRunID(in.RunID) {
		return fmt.Errorf("attach: intent run_id is not canonical")
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
	if !state.IsLocalRelPath(in.RelDir) {
		return fmt.Errorf("attach: intent rel_dir is not a canonical local path")
	}
	if err := validateSnapshotField("task", in.TaskRelPath, in.TaskDigest, in.TaskCanonical); err != nil {
		return err
	}
	if err := validateSnapshotField("policy", in.PolicyRelPath, in.PolicyDigest, in.PolicyCanonical); err != nil {
		return err
	}
	if config.Hash(in.PolicyCanonical) != in.PolicyDigest {
		return fmt.Errorf("attach: intent policy digest disagrees with the policy bytes")
	}
	if config.Hash(in.TaskCanonical) != in.TaskDigest {
		return fmt.Errorf("attach: intent task digest disagrees with the task bytes")
	}
	if err := in.EffectivePolicy.Validate(); err != nil {
		return fmt.Errorf("attach: intent effective_policy: %w", err)
	}
	if in.Base == "" || in.Base != in.EffectivePolicy.BaseBranch {
		return fmt.Errorf("attach: intent base must equal the effective policy base branch")
	}
	if !isGitOID(in.BaseCommit) {
		return fmt.Errorf("attach: intent base_commit is not a git object id")
	}
	if !state.IsLocalRelPath(in.WorktreeRelPath) {
		return fmt.Errorf("attach: intent worktree_rel_path is not a canonical local path")
	}
	if in.RunBranch == "" {
		return fmt.Errorf("attach: intent run_branch is required")
	}
	if !knownFSClass(in.FSClass) || in.FSReason == "" {
		return fmt.Errorf("attach: intent fs classification is incomplete")
	}
	return nil
}

func validateSnapshotField(name, rel, digest string, canonical []byte) error {
	if !state.IsLocalRelPath(rel) {
		return fmt.Errorf("attach: intent %s snapshot path is not a canonical local path", name)
	}
	if !state.IsHex64(digest) {
		return fmt.Errorf("attach: intent %s snapshot digest is not a sha256", name)
	}
	if len(canonical) == 0 || len(canonical) > maxSnapshotBytes {
		return fmt.Errorf("attach: intent %s snapshot bytes out of range", name)
	}
	return nil
}

func knownFSClass(c string) bool {
	switch c {
	case "supported-local", "unknown":
		return true
	}
	return false
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
