// Package config parses, defaults, validates, and resolves a run's two explicit
// inputs: the versioned task contract (what the run must achieve) and the
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

	"github.com/David-c0degeek/claudex/internal/evidence"
)

// Supported schema versions. A supplied document MUST declare its version
// explicitly; a missing or different version fails closed.
// Task-contract v2 added the REQUIRED relevant_repo_paths selection. It is
// meaning-compatible with v1 but not wire-compatible: a v1 document declares no
// machine-resolvable selection at all, and silently treating that as "select
// nothing" would issue read-only review turns with no repository content to
// review. A v1 document therefore fails the version check outright.
const (
	TaskContractVersion = 2
	RunPolicyVersion    = 2
)

// MaxBudget is the shared ceiling for every quality budget and the run-turn cap.
// It bounds the durable counters the engine increments, so a checked used+1 is
// always representable and a frozen policy can never request a limit the state
// counters cannot reach. The state store validates its counters against the same
// ceiling.
//
// It is deliberately storage-safe, not merely integer-safe: every accepted turn
// stays in run state, and genstore hard-caps each generation at 16 MiB. At a
// worst-case accepted-turn entry of a few hundred bytes, 1<<14 turns keeps the
// accepted-turn history well under that record cap, so a frozen max_run_turns can
// always be persisted to its limit.
const MaxBudget = 1 << 14

// MaxEffectivePolicyWireBytes bounds the parsed policy's own encoding.
//
// It exists because the effective policy is carried in the bootstrap intent NEXT TO the source it was
// parsed from, so a large argv element or environment is paid for twice. The budget it comes from:
// the intent carries two 16 KiB source snapshots as base64 (2 * 21848 = 43696 bytes), leaving roughly
// 21840 of the 64 KiB transaction payload for this structure, the resolved execution and the identity
// fields.
const MaxEffectivePolicyWireBytes = 12 * 1024

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
	// RelevantFiles is descriptive human guidance and is never resolved against the repository.
	RelevantFiles []string `json:"relevant_files"`
	// RelevantRepoPaths is the STRICT machine-resolved selection: the exact repository paths a
	// read-only review turn's evidence packet materializes from the committed source object. Each is
	// an exact leaf blob path validated by evidence.IsSelectorPath, so it means the same thing on
	// every host, and a selector absent from the source tree fails packet production closed.
	RelevantRepoPaths []string `json:"relevant_repo_paths"`
	OpenQuestions     []string `json:"open_questions"`
}

// UnknownFSPolicy is what to do when the state filesystem classifies as Unknown.
type UnknownFSPolicy string

const (
	UnknownFSRefuse      UnknownFSPolicy = "refuse"      // default: do not run
	UnknownFSAcknowledge UnknownFSPolicy = "acknowledge" // proceed at operator's risk
)

// platformRequiredEnv is the fixed set of names the OPERATING SYSTEM requires for a process to start
// and for executable lookup to work, per GOOS. It is part of the authority model, not an addition made
// during resolution.
//
// It is UNEXPORTED. An exported map is writable by any importer, so a "closed set" that anyone could
// append to would be a claim the type system contradicts — the authority would be mutable at runtime
// while the documentation called it enumerated.
//
// The distinction between enumerating and injecting is the point. An earlier draft had the default
// allowlist contain only PATH and had resolution quietly add SystemRoot, ComSpec and PATHEXT on
// Windows. That contradicted this package's own claim that nothing unnamed reaches the command:
// recording an implicitly added value does not make it policy-authorized. The alternative — putting
// them in the DEFAULT — is worse in a different way, because then one policy document would parse to
// different values on different hosts and the bootstrap intent's parse-equality invariant could not
// hold.
//
// So they are neither defaulted nor injected: they are ENUMERATED, closed, and not operator-
// configurable. The authorized allowlist is `env.inherit` union platformRequiredEnv[GOOS], every
// resolved name is validated against that union, and an operator naming one of them in the SAME
// spelling is redundant rather than an error.
//
// PATHEXT is present on Windows because executable lookup consults it; its absence was a real defect
// found earlier, not a completeness gesture.
var platformRequiredEnv = map[string][]string{
	"windows": {"ComSpec", "PATHEXT", "SystemRoot"},
	// Unix needs nothing beyond what the operator names; PATH is already the default.
}

// PlatformRequired returns the enumerated platform set for a GOOS. It returns a COPY: handing out the
// backing array would make the closed set appendable by its callers.
func PlatformRequired(goos string) []string {
	names := platformRequiredEnv[goos]
	out := make([]string, len(names))
	copy(out, names)
	return out
}

// EnvAssignment is one explicitly stated environment entry.
//
// It is an OBJECT with name and value fields rather than a JSON map entry, and that is forced by a real
// conflict rather than preference. The strict reader rejects any object key that is not canonical
// lower-case, at every depth — which is what stops a case variant aliasing a schema field, since Go's
// decoder matches keys case-insensitively. Environment names are conventionally UPPER-case, so as a map
// `{"set":{"PATH":"..."}}` could never be accepted: the check would fire on the name before any
// environment rule was reached. Teaching that walker where data begins would give a deliberately total
// check a set of exemptions, which is how such a check quietly stops applying. Moving the data into
// values keeps the walker dumb and correct.
type EnvAssignment struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TestGateEnv is the environment the mechanical test command is authorized to run with.
//
// It exists because naming what the command may see is the only way the environment can be a frozen
// input rather than an ambient one. Inherit lists the variables whose values are taken from the host
// AT FIRST ATTACH and frozen from then on; Set supplies values the policy states outright. Nothing
// reaches the command except what is named here and the enumerated platform-required set above.
//
// HOME is deliberately NOT in the default allowlist. Git, npm and cloud tooling load credential-bearing
// configuration from it, so inheriting it by default would have contradicted this contract's own
// "no provider token" rule; the run supplies a tool-owned per-run scratch HOME instead.
type TestGateEnv struct {
	Inherit []string `json:"inherit"`
	// Set preserves document order. Order is not semantic — duplicates are refused, so no entry can be
	// shadowed by another — but it is preserved so a re-parse of the same bytes compares equal to the
	// frozen policy, which BootstrapIntent requires.
	Set []EnvAssignment `json:"set"`
}

// MarshalJSON renders absent collections as empty ones rather than null.
//
// Go marshals a nil slice and a nil map as `null`, and this package refuses explicit nulls on the way
// back in — so the zero value of this struct could be written and then not read, which makes
// marshal-then-parse a partial function. That matters beyond ergonomics: the frozen policy is embedded
// in persisted run state, and BootstrapIntent requires the re-parsed policy to equal the frozen one
// exactly, so a shape that cannot round-trip is a shape that can strand a run.
//
// "Absent" and "empty" mean the same thing here — no variables — so collapsing them loses nothing.
func (e TestGateEnv) MarshalJSON() ([]byte, error) {
	type wire struct {
		Inherit []string        `json:"inherit"`
		Set     []EnvAssignment `json:"set"`
	}
	w := wire{Inherit: e.Inherit, Set: e.Set}
	if w.Inherit == nil {
		w.Inherit = []string{}
	}
	if w.Set == nil {
		w.Set = []EnvAssignment{}
	}
	return json.Marshal(w)
}

// TestGate is the mechanical test gate. Exactly one of a non-empty Argv or Disabled=true must hold; an
// empty command never silently disables the gate.
//
// Argv replaced a single Command string at run-policy v2. A string has to be split by somebody, and
// every candidate splitter is either a shell — which would give the gate an implicit interpreter and
// its whole injection surface — or a quoting dialect that differs between platforms. An explicit vector
// means a shell must be NAMED to be used: ["sh", "-c", "…"]. Empty later elements are preserved,
// because an empty argument is meaningful to some commands and silently dropping it would change what
// ran.
type TestGate struct {
	Argv     []string    `json:"argv"`
	Disabled bool        `json:"disabled"`
	Env      TestGateEnv `json:"env"`
}

// MarshalJSON renders an absent argv as an empty vector rather than null, for the same reason
// TestGateEnv does: this struct is embedded in persisted run state, and a value that marshals to
// something the parser refuses makes the round-trip a partial function.
func (g TestGate) MarshalJSON() ([]byte, error) {
	type wire struct {
		Argv     []string    `json:"argv"`
		Disabled bool        `json:"disabled"`
		Env      TestGateEnv `json:"env"`
	}
	w := wire{Argv: g.Argv, Disabled: g.Disabled, Env: g.Env}
	if w.Argv == nil {
		w.Argv = []string{}
	}
	return json.Marshal(w)
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
	// MaxTestAttempts bounds the append-only attempt ledger. It is NOT the same thing as the TestFixes
	// budget: an indeterminate attempt — one the runner could not decide, as opposed to one the code
	// failed — is retried without spending a fix, so without a separate ceiling those retries would be
	// unbounded. Exhausting it halts for operator action rather than reporting a code failure.
	MaxTestAttempts int `json:"max_test_attempts"`
	// MaxTestOutputBytes bounds the combined RETAINED excerpt of the command's streams, counted in RAW
	// bytes before encoding.
	MaxTestOutputBytes int64 `json:"max_test_output_bytes"`
	// MaxTestRecordBytes bounds the CANONICAL encoding of one attempt result. The result is issued to
	// the verifier as an evidence-packet file, so a record that cannot be packetized would be a run
	// whose outcome exists and cannot be shown; see the cross-validation in Validate.
	MaxTestRecordBytes int64 `json:"max_test_record_bytes"`
}

// RunPolicy is the operational envelope. There is no agent/turn timeout that
// could expire a turn_id: lease expiry is rejected, so idle turns are bounded
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
		// The default allowlist is PATH and nothing else, and it is deliberately PLATFORM-INDEPENDENT.
		// Windows additionally requires SystemRoot, ComSpec and PATHEXT for a process to start and for
		// executable lookup to work, but putting those in the default here would make the same policy
		// document parse to different values on different hosts — and BootstrapIntent requires the
		// re-parsed policy to equal the frozen EffectivePolicy exactly, so a run bootstrapped on one
		// platform could not be validated on another. The platform-required set therefore belongs to
		// resolution, where it is recorded in ResolvedExecution and bound like every other resolved
		// value, rather than to the portable policy document.
		TestGate: TestGate{Argv: []string{}, Env: TestGateEnv{Inherit: []string{"PATH"}, Set: []EnvAssignment{}}},
		Limits: Limits{
			MaxArtifactBytesPerSubmit: 262144,
			MaxRunTurns:               200,
			MaxWallSeconds:            10800,
			TestTimeoutSeconds:        1800,
			EvidenceMaxTotalBytes:     262144,
			EvidenceMaxFileBytes:      98304,
			EvidenceMaxRequests:       8,
			MaxTestAttempts:           20,
			MaxTestOutputBytes:        32768,
			// Chosen to satisfy the cross-validation below at the defaults:
			// 65536 <= 98304 <= 262144.
			MaxTestRecordBytes: 65536,
		},
	}
}

// taskContractKeys is the full harvested shape: every field must be present
// (arrays may be explicitly empty), matching the strict source schema.
var taskContractKeys = []string{
	"schema_version", "goal", "current_behavior", "desired_behavior", "scope",
	"non_goals", "constraints", "acceptance_criteria", "required_tests",
	"relevant_files", "relevant_repo_paths", "open_questions",
}

// ParseTaskContract decodes and validates a task-contract document.
// MaxContractBytes bounds a task-contract or run-policy source document. It is
// generous for hand-authored inputs, and small enough that a run's frozen input
// snapshots embed safely inside a bounded prepared-transaction payload.
const MaxContractBytes = 16 * 1024

func ParseTaskContract(data []byte) (TaskContract, error) {
	if len(data) > MaxContractBytes {
		return TaskContract{}, fmt.Errorf("task contract: exceeds %d bytes", MaxContractBytes)
	}
	var tc TaskContract
	if err := strictDecode(data, &tc); err != nil {
		return TaskContract{}, fmt.Errorf("task contract: %w", err)
	}
	// Require the full shape: every key present (arrays may be empty).
	present := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &present); err != nil {
		return TaskContract{}, fmt.Errorf("task contract: %w", err)
	}
	for _, k := range taskContractKeys {
		if _, ok := present[k]; !ok {
			return TaskContract{}, fmt.Errorf("task contract: %s is required (present the full shape; arrays may be empty)", k)
		}
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
	// Every string array must have no blank elements.
	arrays := []struct {
		name string
		v    []string
	}{
		{"non_goals", tc.NonGoals},
		{"constraints", tc.Constraints},
		{"acceptance_criteria", tc.AcceptanceCriteria},
		{"required_tests", tc.RequiredTests},
		{"relevant_files", tc.RelevantFiles},
		{"relevant_repo_paths", tc.RelevantRepoPaths},
		{"open_questions", tc.OpenQuestions},
	}
	for _, a := range arrays {
		if err := noBlankElements(a.name, a.v); err != nil {
			return TaskContract{}, err
		}
	}
	if err := validateRepoPaths(tc.RelevantRepoPaths); err != nil {
		return TaskContract{}, err
	}
	if len(tc.AcceptanceCriteria) == 0 {
		return TaskContract{}, fmt.Errorf("task contract: at least one acceptance_criteria is required")
	}
	// Acceptance criteria must be exactly unique (case-sensitive), so a verification
	// can prove complete, exact coverage of the frozen task without a duplicate
	// making that impossible. The error reports the offending index only: the
	// criterion text is caller-supplied and could carry a secret, so it is never
	// echoed.
	seen := make(map[string]int, len(tc.AcceptanceCriteria))
	for i, c := range tc.AcceptanceCriteria {
		if first, ok := seen[c]; ok {
			return TaskContract{}, fmt.Errorf("task contract: acceptance_criteria[%d] duplicates acceptance_criteria[%d]", i, first)
		}
		seen[c] = i
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
	if len(data) > MaxContractBytes {
		return RunPolicy{}, fmt.Errorf("run policy: exceeds %d bytes", MaxContractBytes)
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
	// The version is compared BEFORE the v2 shape is decoded, and the order is the whole remediation.
	//
	// Decoding first meant a genuine v1 document — one carrying `test_gate.command` — was reported as
	// an unknown field. That tells an operator their file is malformed when in fact it is a previous
	// version of a schema that has moved, so the one useful instruction, "this is v1, migrate it", was
	// exactly what the error withheld.
	if *probe.SchemaVersion != RunPolicyVersion {
		return RunPolicy{}, fmt.Errorf("run policy: schema_version must be %d (got %d); v1 documents use test_gate.command, which v2 replaced with an explicit argv vector and env allowlist — migrate the document",
			RunPolicyVersion, *probe.SchemaVersion)
	}
	rp := DefaultRunPolicy()
	if err := strictDecode(data, &rp); err != nil {
		return RunPolicy{}, fmt.Errorf("run policy: %w", err)
	}
	if rp.SchemaVersion != RunPolicyVersion {
		return RunPolicy{}, fmt.Errorf("run policy: schema_version must be %d (got %d)", RunPolicyVersion, rp.SchemaVersion)
	}
	// Absent collections are normalized to EMPTY before validation, so parsing is idempotent under
	// marshaling.
	//
	// This is not tidiness. BootstrapIntent requires reflect.DeepEqual between the frozen policy and a
	// re-parse of its snapshot, and a nil slice is not DeepEqual to an empty one — while MarshalJSON
	// deliberately writes absent collections as `[]` so the document can be read back at all. Without
	// normalization, parse produced nil, marshal produced `[]`, and re-parse produced empty: the round
	// trip was not an identity, and a policy that had been through it could strand a run on an
	// invariant about a difference nothing can observe. A disabled gate hit it first, because that is
	// the shape with no argv.
	rp.TestGate.Argv = orEmpty(rp.TestGate.Argv)
	rp.TestGate.Env.Inherit = orEmpty(rp.TestGate.Env.Inherit)
	rp.TestGate.Env.Set = orEmpty(rp.TestGate.Env.Set)
	if err := rp.Validate(); err != nil {
		return RunPolicy{}, fmt.Errorf("run policy: %w", err)
	}
	return rp, nil
}

// orEmpty replaces an absent collection with an empty one, so "absent" and "empty" — which mean the
// same thing here — also COMPARE the same.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Validate checks the run policy's structural invariants (positive ceilings,
// non-negative budgets, a valid filesystem policy, a base branch). It is
// exported so CLI-override wiring can re-run it on the effective policy before
// freezing. It does NOT check the test-gate/task relationship — that needs the
// task contract; see ValidateEffective.
func (rp RunPolicy) Validate() error {
	if rp.SchemaVersion != RunPolicyVersion {
		return fmt.Errorf("schema_version must be %d (got %d)", RunPolicyVersion, rp.SchemaVersion)
	}
	if strings.TrimSpace(rp.BaseBranch) == "" {
		return fmt.Errorf("base_branch is required")
	}
	switch rp.UnknownFSPolicy {
	case UnknownFSRefuse, UnknownFSAcknowledge:
	default:
		return fmt.Errorf("unknown_fs_policy must be %q or %q, got %q", UnknownFSRefuse, UnknownFSAcknowledge, rp.UnknownFSPolicy)
	}
	// Budgets may be zero but not negative, and must fit the shared counter ceiling
	// so the engine can always represent a checked used+1 (the counters the state
	// store bounds against MaxBudget). max_run_turns shares the same ceiling.
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
		if b.v > MaxBudget {
			return fmt.Errorf("budgets.%s must be <= %d, got %d", b.name, MaxBudget, b.v)
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
		{"max_test_attempts", int64(rp.Limits.MaxTestAttempts)},
		{"max_test_output_bytes", rp.Limits.MaxTestOutputBytes},
		{"max_test_record_bytes", rp.Limits.MaxTestRecordBytes},
	}
	for _, l := range limits {
		if l.v <= 0 {
			return fmt.Errorf("limits.%s must be positive, got %d", l.name, l.v)
		}
	}
	if err := rp.TestGate.validate(); err != nil {
		return err
	}
	// The EFFECTIVE policy carries into the bootstrap intent ALONGSIDE the policy source it was parsed
	// from, so its bytes are counted twice: once base64-expanded as PolicyCanonical, and again as this
	// structure. The 16 KiB source bound therefore says nothing useful about the intent's size — a
	// perfectly valid policy can spend its whole source budget on one argv element, and that element
	// appears in both terms. Review measured exactly that: a legal maximum-sized pair produced a 69769
	// byte payload against a 65536 byte ceiling.
	//
	// Bounding this structure directly is what makes the two terms independent, so the intent's total
	// can be reasoned about at all. BootstrapIntent additionally checks the assembled payload, because
	// arithmetic across four separate ceilings is a proof that rots.
	wire, err := json.Marshal(rp)
	if err != nil {
		return fmt.Errorf("effective policy: %w", err)
	}
	if len(wire) > MaxEffectivePolicyWireBytes {
		return fmt.Errorf("the effective policy encodes to %d bytes, limit %d; the test gate's argv or environment is too large to carry alongside the policy source",
			len(wire), MaxEffectivePolicyWireBytes)
	}
	// The attempt ledger lives inside a full-snapshot RunState, so its ceiling shares the counter
	// ceiling every other stored count is bounded by.
	if rp.Limits.MaxTestAttempts > MaxBudget {
		return fmt.Errorf("limits.max_test_attempts must be <= %d, got %d", MaxBudget, rp.Limits.MaxTestAttempts)
	}
	// Cross-validated, not merely present. An attempt result is issued to the verifier as a file inside
	// an evidence packet, so a record ceiling above the packet's per-file ceiling would describe a run
	// whose outcome is policy-valid and cannot be shown to the party that has to review it. Checking
	// each bound in isolation would accept exactly that policy.
	if rp.Limits.MaxTestRecordBytes > rp.Limits.EvidenceMaxFileBytes {
		return fmt.Errorf("limits.max_test_record_bytes (%d) exceeds evidence_max_file_bytes (%d), so a result could never be issued as evidence",
			rp.Limits.MaxTestRecordBytes, rp.Limits.EvidenceMaxFileBytes)
	}
	// max_run_turns shares the counter ceiling so a frozen policy never requests a
	// turn budget the engine's checked counters cannot represent.
	if rp.Limits.MaxRunTurns > MaxBudget {
		return fmt.Errorf("limits.max_run_turns must be <= %d, got %d", MaxBudget, rp.Limits.MaxRunTurns)
	}
	if rp.Limits.EvidenceMaxFileBytes > rp.Limits.EvidenceMaxTotalBytes {
		return fmt.Errorf("limits.evidence_max_file_bytes (%d) exceeds evidence_max_total_bytes (%d)", rp.Limits.EvidenceMaxFileBytes, rp.Limits.EvidenceMaxTotalBytes)
	}
	return nil
}

// validate enforces the test gate's own shape: exactly one of a command or an explicit disable, and an
// environment allowlist that can only name variables an operating system can actually carry.
//
// The name grammar is [A-Za-z_][A-Za-z0-9_]* and it is narrower than any platform requires. That is
// what makes the Windows case-fold exact: with names restricted to ASCII, "one variable to the OS" is a
// plain A-Z mapping that validation and child-environment construction cannot implement differently.
// A rule stated as "case-insensitive" would be locale- and API-dependent, and the two could disagree.
//
// The FOLD-COLLISION check is deliberately not here. Whether Path and PATH are one variable or two is a
// property of the host, and this document is portable; a policy that is fine on Linux and ambiguous on
// Windows is refused at attach, where the platform is known, rather than made unparseable everywhere.
func (g TestGate) validate() error {
	hasArgv := len(g.Argv) > 0
	if hasArgv == g.Disabled {
		return fmt.Errorf("test_gate must set exactly one of argv or disabled")
	}
	if hasArgv {
		if strings.TrimSpace(g.Argv[0]) == "" {
			return fmt.Errorf("test_gate.argv[0] must name a command")
		}
		for i, a := range g.Argv {
			// Later arguments may be empty ON PURPOSE and are preserved; none may contain NUL, which no
			// exec interface can carry.
			if strings.ContainsRune(a, 0) {
				return fmt.Errorf("test_gate.argv[%d] contains NUL", i)
			}
		}
	}
	for i, n := range g.Env.Inherit {
		if err := validateEnvName(n); err != nil {
			return fmt.Errorf("test_gate.env.inherit[%d]: %w", i, err)
		}
	}
	inherited := make(map[string]bool, len(g.Env.Inherit))
	for _, n := range g.Env.Inherit {
		if inherited[n] {
			return fmt.Errorf("test_gate.env.inherit names %s twice", n)
		}
		inherited[n] = true
	}
	assigned := make(map[string]bool, len(g.Env.Set))
	for i, e := range g.Env.Set {
		if err := validateEnvName(e.Name); err != nil {
			return fmt.Errorf("test_gate.env.set[%d]: %w", i, err)
		}
		if strings.ContainsRune(e.Value, 0) {
			return fmt.Errorf("test_gate.env.set[%d] value of %s contains NUL", i, e.Name)
		}
		if assigned[e.Name] {
			return fmt.Errorf("test_gate.env.set names %s twice", e.Name)
		}
		assigned[e.Name] = true
		// A name in both halves has two answers for one variable, and which one wins would be an
		// implementation detail rather than a stated policy.
		if inherited[e.Name] {
			return fmt.Errorf("test_gate.env names %s in both inherit and set", e.Name)
		}
	}
	return nil
}

// validateEnvName enforces the portable ASCII grammar for an environment variable name.
func validateEnvName(name string) error {
	if name == "" {
		return fmt.Errorf("empty environment variable name")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return fmt.Errorf("environment variable name %q starts with a digit", name)
			}
		default:
			return fmt.Errorf("environment variable name %q is not [A-Za-z_][A-Za-z0-9_]*", name)
		}
	}
	return nil
}

// ValidateEffective is the final gate before freezing: it runs the structural
// policy validation (so CLI overrides applied after parsing cannot smuggle in an
// invalid limit, filesystem policy, or blank base) and then cross-validates the
// test gate against the task contract. The test gate must be an explicit command
// or an explicit disable, and disabling it is refused when the task declares
// required tests.
func ValidateEffective(tc TaskContract, rp RunPolicy) error {
	if err := rp.Validate(); err != nil {
		return err
	}
	// The XOR itself is checked by rp.Validate above, which every caller of this function reaches; what
	// is left is the part that needs the TASK, and therefore cannot live in the policy's own validation.
	if rp.TestGate.Disabled && len(tc.RequiredTests) > 0 {
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
				// Our schema keys are canonical lower-case. Reject any other
				// spelling so a case variant cannot alias a field (Go's decoder
				// matches keys case-insensitively) or hide a semantic duplicate.
				if key != strings.ToLower(key) {
					return fmt.Errorf("non-canonical key spelling %q (keys must be lower-case)", key)
				}
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

// validateRepoPaths holds relevant_repo_paths to the exact-leaf selector grammar the evidence packet
// resolves against the committed source tree. At least one is required: a read-only review turn is
// actionable only through materialized repository content, so a task that names none could never
// produce an actionable packet. Selectors must be exactly unique (byte-for-byte, case-sensitive) —
// a duplicate would consume a second logical entry and a second slice of the request budget while
// naming the same blob. Offending selectors are reported by index only, since the value is
// caller-supplied and could name something sensitive.
func validateRepoPaths(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("task contract: at least one relevant_repo_paths entry is required")
	}
	seen := make(map[string]int, len(paths))
	for i, p := range paths {
		if !evidence.IsSelectorPath(p) {
			return fmt.Errorf("task contract: relevant_repo_paths[%d] is not a canonical repository path "+
				"(forward-slash relative, no traversal, no backslash or drive colon, canonical UTF-8, exact bytes)", i)
		}
		if first, ok := seen[p]; ok {
			return fmt.Errorf("task contract: relevant_repo_paths[%d] duplicates relevant_repo_paths[%d]", i, first)
		}
		seen[p] = i
	}
	return nil
}
