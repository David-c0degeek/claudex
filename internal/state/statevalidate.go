package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/David-c0degeek/claudex/internal/redact"
)

var knownLifecycles = map[Lifecycle]bool{
	LifecycleRunning: true, LifecyclePaused: true, LifecyclePausedBudget: true,
	LifecycleRateLimited: true, LifecycleCancelled: true, LifecycleFailedRetryable: true,
	LifecycleFailedTerminal: true, LifecycleCompleted: true,
}

var knownPhases = map[Phase]bool{
	PhaseInit: true, PhasePlanDraft: true, PhasePlanCritique: true, PhasePlanRevise: true,
	PhaseImplementStep: true, PhaseCheckpoint: true, PhaseFix: true, PhaseTests: true,
	PhaseVerify: true, PhaseAwaitGuidance: true, PhaseDone: true,
}

var knownFSClasses = map[string]bool{
	"supported-local": true, "known-unsupported": true, "unknown": true,
}

// strictDecodeRunState decodes exactly one JSON value with no unknown fields and
// no trailing content.
func strictDecodeRunState(data []byte) (RunState, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rs RunState
	if err := dec.Decode(&rs); err != nil {
		return RunState{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return RunState{}, fmt.Errorf("unexpected trailing content")
	}
	return rs, nil
}

// validate enforces the invariants that hold for every persisted run state.
func validate(rs *RunState) error {
	if rs.SchemaVersion != RunStateVersion {
		return fmt.Errorf("schema_version %d != %d", rs.SchemaVersion, RunStateVersion)
	}
	if !validRunID(rs.RunID) {
		return fmt.Errorf("invalid run_id %q", rs.RunID)
	}
	if rs.Revision == 0 {
		return fmt.Errorf("revision must be > 0")
	}
	if !knownLifecycles[rs.Lifecycle] {
		return fmt.Errorf("unknown lifecycle %q", rs.Lifecycle)
	}
	if !knownPhases[rs.Phase] {
		return fmt.Errorf("unknown phase %q", rs.Phase)
	}
	if rs.CreatedUnix <= 0 || rs.StartedUnix < 0 || rs.DeadlineUnix < 0 {
		return fmt.Errorf("invalid timestamps")
	}
	if err := validateSnapshot("task_snapshot", rs.TaskSnapshot); err != nil {
		return err
	}
	if err := validateSnapshot("policy_snapshot", rs.PolicySnapshot); err != nil {
		return err
	}
	if err := rs.EffectivePolicy.Validate(); err != nil {
		return fmt.Errorf("effective_policy: %w", err)
	}
	if !knownFSClasses[rs.FS.Class] {
		return fmt.Errorf("unknown fs class %q", rs.FS.Class)
	}
	if strings.TrimSpace(rs.Base) == "" {
		return fmt.Errorf("base is required")
	}
	if strings.TrimSpace(rs.BaseCommit) == "" {
		return fmt.Errorf("base_commit is required")
	}
	if err := validateCounters(rs.Counters); err != nil {
		return err
	}
	if err := validateAcceptedTurns(rs); err != nil {
		return err
	}
	if err := validateRef("assignment", rs.Assignment, rs.Revision); err != nil {
		return err
	}
	if err := validateRef("gate", rs.Gate, rs.Revision); err != nil {
		return err
	}
	if err := validateProjection("recovery", rs.Recovery, rs.Revision); err != nil {
		return err
	}
	if err := validateProjection("failure", rs.Failure, rs.Revision); err != nil {
		return err
	}
	return nil
}

// validateInit adds the requirements specific to the first generation.
func validateInit(rs *RunState) error {
	if rs.Revision != 1 {
		return fmt.Errorf("initial state must be revision 1, got %d", rs.Revision)
	}
	if rs.Lifecycle != LifecycleRunning {
		return fmt.Errorf("initial lifecycle must be running, got %q", rs.Lifecycle)
	}
	return nil
}

// validateTransition enforces immutability, monotonicity, and append-only rules.
func validateTransition(old, next *RunState) error {
	// Bootstrap/input fields are immutable after generation 1.
	if old.RunID != next.RunID {
		return fmt.Errorf("run_id is immutable")
	}
	if old.CreatedUnix != next.CreatedUnix {
		return fmt.Errorf("created_unix is immutable")
	}
	if old.TaskSnapshot != next.TaskSnapshot || old.PolicySnapshot != next.PolicySnapshot {
		return fmt.Errorf("input snapshots are immutable")
	}
	if !reflect.DeepEqual(old.EffectivePolicy, next.EffectivePolicy) {
		return fmt.Errorf("effective_policy is immutable")
	}
	if old.FS != next.FS {
		return fmt.Errorf("filesystem decision is immutable")
	}
	if old.Base != next.Base || old.BaseCommit != next.BaseCommit {
		return fmt.Errorf("base identity is immutable")
	}
	// Started/deadline are write-once.
	if err := writeOnce("started_unix", old.StartedUnix, next.StartedUnix); err != nil {
		return err
	}
	if err := writeOnce("deadline_unix", old.DeadlineUnix, next.DeadlineUnix); err != nil {
		return err
	}
	// Counters are non-decreasing.
	if next.Counters.PlanRevisions < old.Counters.PlanRevisions ||
		next.Counters.TestFixes < old.Counters.TestFixes ||
		next.Counters.VerifyFixes < old.Counters.VerifyFixes {
		return fmt.Errorf("counters must not decrease")
	}
	if len(next.Counters.StepFixes) < len(old.Counters.StepFixes) {
		return fmt.Errorf("step fixes must not shrink")
	}
	for i := range old.Counters.StepFixes {
		if next.Counters.StepFixes[i] < old.Counters.StepFixes[i] {
			return fmt.Errorf("step fix %d must not decrease", i)
		}
	}
	// Accepted turns are append-only and existing entries are frozen.
	for k, v := range old.AcceptedTurns {
		nv, ok := next.AcceptedTurns[k]
		if !ok || nv != v {
			return fmt.Errorf("accepted turn %q is immutable", k)
		}
	}
	// A newly-set or replaced ref must bind to the resulting revision.
	if err := refBindsToRevision("assignment", old.Assignment, next.Assignment, next.Revision); err != nil {
		return err
	}
	if err := refBindsToRevision("gate", old.Gate, next.Gate, next.Revision); err != nil {
		return err
	}
	return nil
}

func writeOnce(field string, old, next int64) error {
	if old != 0 && next != old {
		return fmt.Errorf("%s is write-once", field)
	}
	return nil
}

func refBindsToRevision(field string, old, next *Ref, rev uint64) error {
	if next == nil {
		return nil // cleared or never set
	}
	if old != nil && *old == *next {
		return nil // unchanged
	}
	if next.IssuedRevision != rev {
		return fmt.Errorf("%s was (re)issued but bound to revision %d, not the resulting %d", field, next.IssuedRevision, rev)
	}
	return nil
}

func validateSnapshot(field string, sr SnapshotRef) error {
	if !isLocalRelPath(sr.RelPath) {
		return fmt.Errorf("%s.rel_path %q is not a canonical local path", field, sr.RelPath)
	}
	if !isHex64(sr.Digest) {
		return fmt.Errorf("%s.digest is not a 64-char lower-hex sha256", field)
	}
	return nil
}

func validateCounters(c Counters) error {
	if c.PlanRevisions < 0 || c.TestFixes < 0 || c.VerifyFixes < 0 {
		return fmt.Errorf("counters must be non-negative")
	}
	for i, v := range c.StepFixes {
		if v < 0 {
			return fmt.Errorf("step fix %d is negative", i)
		}
	}
	return nil
}

func validateAcceptedTurns(rs *RunState) error {
	for k, v := range rs.AcceptedTurns {
		if v.Receipt.TurnID != k {
			return fmt.Errorf("accepted turn key %q != receipt turn_id %q", k, v.Receipt.TurnID)
		}
		if v.ArtifactDigest != v.Receipt.ArtifactDigest {
			return fmt.Errorf("accepted turn %q artifact digests disagree", k)
		}
		if !isHex64(v.ArtifactDigest) {
			return fmt.Errorf("accepted turn %q digest is not a 64-char lower-hex sha256", k)
		}
		if v.Receipt.Revision == 0 || v.Receipt.Revision > rs.Revision {
			return fmt.Errorf("accepted turn %q receipt revision %d out of range (1..%d)", k, v.Receipt.Revision, rs.Revision)
		}
	}
	return nil
}

func validateRef(field string, r *Ref, rev uint64) error {
	if r == nil {
		return nil
	}
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("%s id is required when present", field)
	}
	if r.IssuedRevision == 0 || r.IssuedRevision > rev {
		return fmt.Errorf("%s issued_revision %d out of range (1..%d)", field, r.IssuedRevision, rev)
	}
	return nil
}

func validateProjection(field string, p *Projection, rev uint64) error {
	if p == nil {
		return nil
	}
	if p.AtRevision > rev {
		return fmt.Errorf("%s at_revision %d > state revision %d", field, p.AtRevision, rev)
	}
	return nil
}

// redactAndGuard redacts documented free-text fields and rejects a secret in any
// executable/control field (never silently rewriting identity/control values).
func redactAndGuard(rs *RunState) error {
	rs.FS.Reason = redact.Text(rs.FS.Reason)
	if rs.Recovery != nil {
		rs.Recovery.Reason = redact.Text(rs.Recovery.Reason)
	}
	if rs.Failure != nil {
		rs.Failure.Reason = redact.Text(rs.Failure.Reason)
	}

	control := map[string]string{
		"run_id":                             rs.RunID,
		"base":                               rs.Base,
		"base_commit":                        rs.BaseCommit,
		"phase":                              string(rs.Phase),
		"lifecycle":                          string(rs.Lifecycle),
		"task_snapshot.rel_path":             rs.TaskSnapshot.RelPath,
		"task_snapshot.digest":               rs.TaskSnapshot.Digest,
		"policy_snapshot.rel_path":           rs.PolicySnapshot.RelPath,
		"policy_snapshot.digest":             rs.PolicySnapshot.Digest,
		"fs.class":                           rs.FS.Class,
		"effective_policy.test_gate.command": rs.EffectivePolicy.TestGate.Command,
		"effective_policy.base_branch":       rs.EffectivePolicy.BaseBranch,
		"pending_txn_id":                     rs.PendingTxnID,
	}
	if rs.Assignment != nil {
		control["assignment.id"] = rs.Assignment.ID
	}
	if rs.Gate != nil {
		control["gate.id"] = rs.Gate.ID
	}
	if rs.Recovery != nil {
		control["recovery.code"] = rs.Recovery.Code
		control["recovery.next_action"] = rs.Recovery.NextAction
	}
	if rs.Failure != nil {
		control["failure.code"] = rs.Failure.Code
		control["failure.next_action"] = rs.Failure.NextAction
	}
	for k, v := range rs.AcceptedTurns {
		control["accepted_turns."+k+".digest"] = v.ArtifactDigest
	}
	for field, v := range control {
		if redact.Text(v) != v {
			return fmt.Errorf("state: a secret was detected in control field %s; use environment-based credentials, not run state", field)
		}
	}
	return nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isLocalRelPath(p string) bool {
	if p == "" || p == "." {
		return false
	}
	return filepath.IsLocal(p) && filepath.ToSlash(filepath.Clean(p)) == filepath.ToSlash(p)
}

func validRunID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
