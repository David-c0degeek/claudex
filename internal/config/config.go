// Package config parses, defaults, and validates a run's two explicit inputs
// (D016): the versioned task contract (what the run must achieve) and the run
// policy (the mechanical test command, observable caps, timeouts, and base/repo
// policy). Both are resolved by the first attach, hashed, and frozen into run
// state so later edits cannot change a live run.
//
// This package owns parsing and validation only. Persisting the frozen policy
// and the task-contract hash is the state layer's job.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Current supported schema versions. A file may omit the version (0), which is
// treated as the current version; any other value fails closed.
const (
	TaskContractVersion = 1
	RunPolicyVersion    = 1
)

// TaskContract is the run's goal and acceptance definition (the harvested
// TASK_CONTRACT shape).
type TaskContract struct {
	SchemaVersion      int      `json:"schema_version"`
	Goal               string   `json:"goal"`
	CurrentBehavior    string   `json:"current_behavior"`
	DesiredBehavior    string   `json:"desired_behavior"`
	Scope              string   `json:"scope"`
	NonGoals           []string `json:"non_goals"`
	Constraints        []string `json:"constraints"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	RequiredTests      []string `json:"required_tests"`
	RelevantFiles      []string `json:"relevant_files"`
	OpenQuestions      []string `json:"open_questions"`
}

// Caps are the observable-only limits the coordinator can enforce in attach
// (turn/fix counts, artifact bytes, wall time, per-turn timeout). Token/cost
// caps are deliberately absent — they are not observable in attach (D007).
type Caps struct {
	MaxPlanRounds       int   `json:"max_plan_rounds"`
	MaxCheckpointRounds int   `json:"max_checkpoint_rounds"`
	MaxTestRounds       int   `json:"max_test_rounds"`
	MaxVerifyRounds     int   `json:"max_verify_rounds"`
	MaxArtifactBytes    int64 `json:"max_artifact_bytes"`
	MaxWallSeconds      int64 `json:"max_wall_seconds"`
	AgentTimeoutSeconds int64 `json:"agent_timeout_seconds"`
}

// RunPolicy is the frozen operational envelope for a run.
type RunPolicy struct {
	SchemaVersion int    `json:"schema_version"`
	TestCommand   string `json:"test_command"` // optional; empty means no mechanical gate
	BaseBranch    string `json:"base_branch"`
	Caps          Caps   `json:"caps"`
}

// DefaultRunPolicy returns the baseline policy that unset fields fall back to.
func DefaultRunPolicy() RunPolicy {
	return RunPolicy{
		SchemaVersion: RunPolicyVersion,
		BaseBranch:    "main",
		Caps: Caps{
			MaxPlanRounds:       1,
			MaxCheckpointRounds: 3,
			MaxTestRounds:       2,
			MaxVerifyRounds:     2,
			MaxArtifactBytes:    262144,
			MaxWallSeconds:      10800,
			AgentTimeoutSeconds: 3600,
		},
	}
}

// ParseTaskContract decodes and validates a task-contract file. Unknown fields
// are rejected so a typo fails closed rather than being silently ignored.
func ParseTaskContract(data []byte) (TaskContract, error) {
	var tc TaskContract
	if err := strictUnmarshal(data, &tc); err != nil {
		return TaskContract{}, fmt.Errorf("task contract: %w", err)
	}
	if tc.SchemaVersion == 0 {
		tc.SchemaVersion = TaskContractVersion
	}
	if tc.SchemaVersion != TaskContractVersion {
		return TaskContract{}, fmt.Errorf("task contract: unsupported schema_version %d (want %d)", tc.SchemaVersion, TaskContractVersion)
	}
	if strings.TrimSpace(tc.Goal) == "" {
		return TaskContract{}, fmt.Errorf("task contract: goal is required")
	}
	if strings.TrimSpace(tc.DesiredBehavior) == "" {
		return TaskContract{}, fmt.Errorf("task contract: desired_behavior is required")
	}
	if len(nonEmpty(tc.AcceptanceCriteria)) == 0 {
		return TaskContract{}, fmt.Errorf("task contract: at least one acceptance_criteria is required")
	}
	return tc, nil
}

// ParseRunPolicy decodes a run-policy file, fills unset fields from the
// defaults, and validates the result. A nil/empty input yields the defaults.
func ParseRunPolicy(data []byte) (RunPolicy, error) {
	rp := DefaultRunPolicy()
	if len(bytes.TrimSpace(data)) != 0 {
		// Decode overrides onto the defaults so omitted fields keep their default.
		if err := strictUnmarshal(data, &rp); err != nil {
			return RunPolicy{}, fmt.Errorf("run policy: %w", err)
		}
	}
	if rp.SchemaVersion == 0 {
		rp.SchemaVersion = RunPolicyVersion
	}
	if rp.SchemaVersion != RunPolicyVersion {
		return RunPolicy{}, fmt.Errorf("run policy: unsupported schema_version %d (want %d)", rp.SchemaVersion, RunPolicyVersion)
	}
	if err := rp.validate(); err != nil {
		return RunPolicy{}, fmt.Errorf("run policy: %w", err)
	}
	return rp, nil
}

func (rp RunPolicy) validate() error {
	if strings.TrimSpace(rp.BaseBranch) == "" {
		return fmt.Errorf("base_branch is required")
	}
	positives := []struct {
		name string
		v    int64
	}{
		{"max_plan_rounds", int64(rp.Caps.MaxPlanRounds)},
		{"max_checkpoint_rounds", int64(rp.Caps.MaxCheckpointRounds)},
		{"max_test_rounds", int64(rp.Caps.MaxTestRounds)},
		{"max_verify_rounds", int64(rp.Caps.MaxVerifyRounds)},
		{"max_artifact_bytes", rp.Caps.MaxArtifactBytes},
		{"max_wall_seconds", rp.Caps.MaxWallSeconds},
		{"agent_timeout_seconds", rp.Caps.AgentTimeoutSeconds},
	}
	for _, p := range positives {
		if p.v <= 0 {
			return fmt.Errorf("%s must be positive, got %d", p.name, p.v)
		}
	}
	return nil
}

// Hash returns the hex-encoded SHA-256 of the exact input bytes, used to record
// which task contract / policy a run was frozen against.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// strictUnmarshal rejects unknown fields.
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("unexpected trailing content after JSON value")
	}
	return nil
}

func nonEmpty(ss []string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
