// Package config parses, defaults, validates, and resolves a run's two explicit
// inputs (D016): the versioned task contract (what the run must achieve) and the
// run policy (test gate, observable caps, timeouts, evidence bounds, base/repo
// and filesystem policy). The first attach resolves the effective policy, hashes
// the raw sources, and freezes both into run state so later edits cannot change a
// live run.
//
// This package owns parsing, validation, and resolution only. Persisting the
// frozen values and digests is the state layer's job.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Supported schema versions. A supplied document MUST declare its version
// explicitly; a missing or different version fails closed.
const (
	TaskContractVersion = 1
	RunPolicyVersion    = 1
)

// TaskContract is the run's goal and acceptance definition (harvested shape).
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

// UnknownFSPolicy is what to do when the state filesystem classifies as Unknown.
type UnknownFSPolicy string

const (
	UnknownFSRefuse      UnknownFSPolicy = "refuse"      // default: do not run
	UnknownFSAcknowledge UnknownFSPolicy = "acknowledge" // proceed at operator's risk
)

// TestGate is the mechanical test gate. Exactly one of a non-blank Command or
// Disabled=true must hold; an empty command never silently disables the gate.
type TestGate struct {
	Command  string `json:"command"`
	Disabled bool   `json:"disabled"`
}

// Budgets are the number of allowed revise/fix cycles per phase. Zero is valid
// (no cycles allowed); they are not byte/time ceilings.
type Budgets struct {
	PlanRounds       int `json:"plan_rounds"`
	CheckpointRounds int `json:"checkpoint_rounds"`
	TestRounds       int `json:"test_rounds"`
	VerifyRounds     int `json:"verify_rounds"`
}

// Limits are ceilings; each must be positive. Submit-artifact size is separate
// from evidence bounds, and the mechanical-test timeout is distinct from any
// per-turn concept.
type Limits struct {
	MaxArtifactBytesPerSubmit int64 `json:"max_artifact_bytes_per_submit"`
	MaxRunTurns               int   `json:"max_run_turns"`
	MaxWallSeconds            int64 `json:"max_wall_seconds"`
	TestTimeoutSeconds        int64 `json:"test_timeout_seconds"`
	EvidenceMaxTotalBytes     int64 `json:"evidence_max_total_bytes"`
	EvidenceMaxFileBytes      int64 `json:"evidence_max_file_bytes"`
	EvidenceMaxRequests       int   `json:"evidence_max_requests"`
}

// RunPolicy is the operational envelope. There is no agent/turn timeout that
// could expire a turn_id: D004 rejects lease expiry, so idle turns are bounded
// only by the run wall cap, never by silently invalidating an assignment.
type RunPolicy struct {
	SchemaVersion   int             `json:"schema_version"`
	TestGate        TestGate        `json:"test_gate"`
	BaseBranch      string          `json:"base_branch"`
	UnknownFSPolicy UnknownFSPolicy `json:"unknown_fs_policy"`
	Budgets         Budgets         `json:"budgets"`
	Limits          Limits          `json:"limits"`
}

// DefaultRunPolicy returns the override base: safe defaults for every field
// except the test gate, which has no safe universal default and must be set
// explicitly by the supplied policy.
func DefaultRunPolicy() RunPolicy {
	return RunPolicy{
		SchemaVersion:   RunPolicyVersion,
		BaseBranch:      "main",
		UnknownFSPolicy: UnknownFSRefuse,
		Budgets:         Budgets{PlanRounds: 1, CheckpointRounds: 3, TestRounds: 2, VerifyRounds: 2},
		Limits: Limits{
			MaxArtifactBytesPerSubmit: 262144,
			MaxRunTurns:               200,
			MaxWallSeconds:            10800,
			TestTimeoutSeconds:        1800,
			EvidenceMaxTotalBytes:     262144,
			EvidenceMaxFileBytes:      98304,
			EvidenceMaxRequests:       8,
		},
	}
}

// ParseTaskContract decodes and validates a task-contract document.
func ParseTaskContract(data []byte) (TaskContract, error) {
	var tc TaskContract
	if err := strictDecode(data, &tc); err != nil {
		return TaskContract{}, fmt.Errorf("task contract: %w", err)
	}
	if tc.SchemaVersion != TaskContractVersion {
		return TaskContract{}, fmt.Errorf("task contract: schema_version must be %d (got %d)", TaskContractVersion, tc.SchemaVersion)
	}
	scalars := []struct{ name, v string }{
		{"goal", tc.Goal},
		{"current_behavior", tc.CurrentBehavior},
		{"desired_behavior", tc.DesiredBehavior},
		{"scope", tc.Scope},
	}
	for _, s := range scalars {
		if strings.TrimSpace(s.v) == "" {
			return TaskContract{}, fmt.Errorf("task contract: %s is required", s.name)
		}
	}
	if err := noBlankElements("acceptance_criteria", tc.AcceptanceCriteria); err != nil {
		return TaskContract{}, err
	}
	if len(tc.AcceptanceCriteria) == 0 {
		return TaskContract{}, fmt.Errorf("task contract: at least one acceptance_criteria is required")
	}
	if err := noBlankElements("required_tests", tc.RequiredTests); err != nil {
		return TaskContract{}, err
	}
	if err := noBlankElements("relevant_files", tc.RelevantFiles); err != nil {
		return TaskContract{}, err
	}
	return tc, nil
}

// ParseRunPolicy decodes a supplied run-policy document over the defaults so
// omitted fields keep their default, and validates the structural result. A
// supplied document must declare schema_version. Use DefaultRunPolicy for the
// no-file case; do not pass empty data here.
func ParseRunPolicy(data []byte) (RunPolicy, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return RunPolicy{}, fmt.Errorf("run policy: empty document (use DefaultRunPolicy for no-file runs)")
	}
	// Because the document is decoded onto non-zero defaults, an absent
	// schema_version would be masked by the default. Probe its presence with a
	// pointer field, which distinguishes absent (nil) from an explicit value.
	var probe struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return RunPolicy{}, fmt.Errorf("run policy: %w", err)
	}
	if probe.SchemaVersion == nil {
		return RunPolicy{}, fmt.Errorf("run policy: schema_version is required")
	}
	rp := DefaultRunPolicy()
	if err := strictDecode(data, &rp); err != nil {
		return RunPolicy{}, fmt.Errorf("run policy: %w", err)
	}
	if rp.SchemaVersion != RunPolicyVersion {
		return RunPolicy{}, fmt.Errorf("run policy: schema_version must be %d (got %d)", RunPolicyVersion, rp.SchemaVersion)
	}
	if err := rp.validateStructural(); err != nil {
		return RunPolicy{}, fmt.Errorf("run policy: %w", err)
	}
	return rp, nil
}

func (rp RunPolicy) validateStructural() error {
	if strings.TrimSpace(rp.BaseBranch) == "" {
		return fmt.Errorf("base_branch is required")
	}
	switch rp.UnknownFSPolicy {
	case UnknownFSRefuse, UnknownFSAcknowledge:
	default:
		return fmt.Errorf("unknown_fs_policy must be %q or %q, got %q", UnknownFSRefuse, UnknownFSAcknowledge, rp.UnknownFSPolicy)
	}
	// Budgets may be zero but not negative.
	budgets := []struct {
		name string
		v    int
	}{
		{"plan_rounds", rp.Budgets.PlanRounds},
		{"checkpoint_rounds", rp.Budgets.CheckpointRounds},
		{"test_rounds", rp.Budgets.TestRounds},
		{"verify_rounds", rp.Budgets.VerifyRounds},
	}
	for _, b := range budgets {
		if b.v < 0 {
			return fmt.Errorf("budgets.%s must be >= 0, got %d", b.name, b.v)
		}
	}
	// Ceilings must be positive.
	limits := []struct {
		name string
		v    int64
	}{
		{"max_artifact_bytes_per_submit", rp.Limits.MaxArtifactBytesPerSubmit},
		{"max_run_turns", int64(rp.Limits.MaxRunTurns)},
		{"max_wall_seconds", rp.Limits.MaxWallSeconds},
		{"test_timeout_seconds", rp.Limits.TestTimeoutSeconds},
		{"evidence_max_total_bytes", rp.Limits.EvidenceMaxTotalBytes},
		{"evidence_max_file_bytes", rp.Limits.EvidenceMaxFileBytes},
		{"evidence_max_requests", int64(rp.Limits.EvidenceMaxRequests)},
	}
	for _, l := range limits {
		if l.v <= 0 {
			return fmt.Errorf("limits.%s must be positive, got %d", l.name, l.v)
		}
	}
	if rp.Limits.EvidenceMaxFileBytes > rp.Limits.EvidenceMaxTotalBytes {
		return fmt.Errorf("limits.evidence_max_file_bytes (%d) exceeds evidence_max_total_bytes (%d)", rp.Limits.EvidenceMaxFileBytes, rp.Limits.EvidenceMaxTotalBytes)
	}
	return nil
}

// ValidateEffective cross-validates the resolved policy against the task
// contract. It is the final gate before freezing: the test gate must be an
// explicit command or an explicit disable, and disabling the gate is refused
// when the task declares required tests.
func ValidateEffective(tc TaskContract, rp RunPolicy) error {
	hasCmd := strings.TrimSpace(rp.TestGate.Command) != ""
	switch {
	case rp.TestGate.Disabled && hasCmd:
		return fmt.Errorf("test_gate: cannot set both a command and disabled")
	case !rp.TestGate.Disabled && !hasCmd:
		return fmt.Errorf("test_gate: set a non-blank command or explicitly disable it")
	case rp.TestGate.Disabled && len(tc.RequiredTests) > 0:
		return fmt.Errorf("test_gate: disabled, but the task contract requires tests: %v", tc.RequiredTests)
	}
	return nil
}

// Hash returns the hex-encoded SHA-256 of the exact input bytes, so a run records
// which task contract / policy source it was frozen against.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// strictDecode rejects unknown fields, duplicate keys, explicit null values, and
// any trailing content after the single top-level value.
func strictDecode(data []byte, v any) error {
	if err := checkStrictJSON(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// checkStrictJSON walks the token stream to reject duplicate object keys and
// explicit nulls anywhere, and requires exactly one top-level value followed by
// EOF (so trailing garbage or a stray delimiter is rejected).
func checkStrictJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSON(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unexpected trailing content after JSON value")
	}
	return nil
}

func walkJSON(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key := kt.(string)
				if seen[key] {
					return fmt.Errorf("duplicate key %q", key)
				}
				seen[key] = true
				if err := walkJSON(dec); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return err
			}
		case '[':
			for dec.More() {
				if err := walkJSON(dec); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return err
			}
		}
	case nil:
		return fmt.Errorf("explicit null values are not allowed")
	}
	return nil
}

func noBlankElements(field string, ss []string) error {
	for i, s := range ss {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("task contract: %s[%d] is blank", field, i)
		}
	}
	return nil
}
