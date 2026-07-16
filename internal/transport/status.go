package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/redact"
	"github.com/David-c0degeek/claudex/internal/state"
)

const (
	statusType          = "status"
	capMechanismCounter = "durable-state-counter"

	// tierMechanismBYO / tierMechanismManaged are the stable mechanisms backing
	// each tier, so the tier label is not a bare assertion.
	tierMechanismBYO     = "durable-byo-registration"
	tierMechanismManaged = "managed-launch-record"
)

// tierMechanisms binds each tier to the one mechanism that may back it.
var tierMechanisms = map[Tier]string{
	TierProtocolOnly: tierMechanismBYO,
	TierManaged:      tierMechanismManaged,
}

var (
	// ErrHonestySource means the honesty seam failed or returned invalid labels.
	ErrHonestySource = errors.New("transport: honesty source")
)

// Tier is the run's execution tier.
type Tier string

const (
	TierProtocolOnly Tier = "protocol-only"
	TierManaged      Tier = "managed"
)

// Capability is one honestly-labeled capability: its name, whether it is
// actually enforced, and the mechanism backing that claim.
type Capability struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // enforced | unavailable | requires-telemetry
	Mechanism string `json:"mechanism"`
}

var capabilityStatuses = map[string]bool{"enforced": true, "unavailable": true, "requires-telemetry": true}

// HonestyLabels are the tier and capabilities the honesty source reports. The
// tier is backed by a mechanism (a bounded identifier for the durable BYO
// registration or managed-launch record), so it is not a bare assertion.
type HonestyLabels struct {
	Tier          Tier         `json:"tier"`
	TierMechanism string       `json:"tier_mechanism"`
	Capabilities  []Capability `json:"capabilities"`
}

// StatusInput is the immutable, value-only projection handed to the honesty seam.
type StatusInput struct {
	RunID     string
	Revision  uint64
	Phase     state.Phase
	Lifecycle state.Lifecycle
}

// HonestySource answers the run's tier and capabilities from durable session
// registration and (for managed runs) the provider capability table. It MUST be
// pure and side-effect-free and return value-free errors. A BYO run returns
// protocol-only; it must never default to managed.
type HonestySource func(in StatusInput) (HonestyLabels, error)

// CapCounter is a scalar cap: usage against a limit, with capacity clamped at
// zero and any overage reported separately, plus the mechanism measuring it.
type CapCounter struct {
	Used       int    `json:"used"`
	Limit      int    `json:"limit"`
	Remaining  int    `json:"remaining"`
	ExceededBy int    `json:"exceeded_by"`
	Mechanism  string `json:"mechanism"`
}

// StepCap is one step's checkpoint-fix usage.
type StepCap struct {
	StepIndex  int `json:"step_index"`
	Used       int `json:"used"`
	Remaining  int `json:"remaining"`
	ExceededBy int `json:"exceeded_by"`
}

// CheckpointCap is the per-step checkpoint budget, in ascending step order.
type CheckpointCap struct {
	LimitPerStep int       `json:"limit_per_step"`
	Steps        []StepCap `json:"steps"`
	Mechanism    string    `json:"mechanism"`
}

// CapsReport is the frozen-budget usage. Wall time and per-submit bytes have
// different (clock/per-operation) semantics and are deferred to the caps work,
// so they are not squeezed into this used/limit/remaining shape.
type CapsReport struct {
	RunTurns         CapCounter    `json:"run_turns"`
	PlanRounds       CapCounter    `json:"plan_rounds"`
	TestRounds       CapCounter    `json:"test_rounds"`
	VerifyRounds     CapCounter    `json:"verify_rounds"`
	CheckpointRounds CheckpointCap `json:"checkpoint_rounds"`
}

// StopProjection is the discriminated stop reason, present when the run failed or
// needs recovery.
type StopProjection struct {
	Kind       string `json:"kind"` // failure | recovery
	Code       string `json:"code"`
	Reason     string `json:"reason"` // redacted
	NextAction string `json:"next_action"`
	AtRevision uint64 `json:"at_revision"`
}

// StatusReport is the lock-free status projection wire message.
type StatusReport struct {
	ProtocolVersion int             `json:"protocol_version"`
	MessageType     string          `json:"message_type"`
	RunID           string          `json:"run_id"`
	Revision        uint64          `json:"revision"`
	Lifecycle       state.Lifecycle `json:"lifecycle"`
	Phase           state.Phase     `json:"phase"`
	WhoseTurn       *Role           `json:"whose_turn"`
	TurnID          *string         `json:"turn_id"`
	GateID          *string         `json:"gate_id"`
	Stop            *StopProjection `json:"stop"`
	Caps            CapsReport      `json:"caps"`
	Honesty         HonestyLabels   `json:"honesty"`
}

// Status reads run state once without the mutation lock and projects a validated
// status report. Coherence and ownership inconsistencies fail closed.
func Status(store *state.Store, honesty HonestySource) (StatusReport, error) {
	if honesty == nil {
		return StatusReport{}, ErrMissingSeam
	}
	rs, ok, err := store.Load()
	if err != nil {
		return StatusReport{}, err
	}
	if !ok {
		return StatusReport{}, ErrNoRun
	}

	facts := captureFacts(rs)
	if err := coherenceCheck(facts); err != nil {
		return StatusReport{}, err
	}
	// Project the stop before ownership: a required recovery dominates the agent
	// turn (as it does in wait), so it suppresses the owner rather than reporting
	// two next actors.
	stop, err := projectStop(facts)
	if err != nil {
		return StatusReport{}, err
	}
	recoveryPresent := stop != nil && stop.Kind == "recovery"
	whose, turnID, err := projectOwner(facts, recoveryPresent)
	if err != nil {
		return StatusReport{}, err
	}

	labels, err := honesty(StatusInput{RunID: rs.RunID, Revision: rs.Revision, Phase: rs.Phase, Lifecycle: rs.Lifecycle})
	if err != nil {
		return StatusReport{}, ErrHonestySource // value-free sentinel; never surface the seam's string
	}
	labels = cloneLabels(labels)
	if err := validateHonesty(labels); err != nil {
		// Collapse an invalid source RESULT too: its tier/name fields are arbitrary
		// and must not cross the boundary.
		return StatusReport{}, ErrHonestySource
	}

	report := StatusReport{
		ProtocolVersion: protocol.SupportedVersion,
		MessageType:     statusType,
		RunID:           rs.RunID,
		Revision:        rs.Revision,
		Lifecycle:       rs.Lifecycle,
		Phase:           rs.Phase,
		WhoseTurn:       whose,
		TurnID:          turnID,
		Stop:            stop,
		Caps:            projectCaps(rs),
		Honesty:         labels,
	}
	if facts.gateID != "" {
		id := facts.gateID
		report.GateID = &id
	}
	if err := report.validate(); err != nil {
		return StatusReport{}, fmt.Errorf("transport: constructed status is invalid: %w", err)
	}
	return report, nil
}

// projectOwner reports the turn owner only for a live assignment under a running
// agent phase. A cancelled/terminal run with a leftover actionable phase but no
// assignment reports no owner; an assignment under a non-agent or non-running
// shape, or a running agent phase with no assignment, fails closed.
func projectOwner(f runFacts, recoveryPresent bool) (*Role, *string, error) {
	spec, agentPhase := TurnSpec(f.phase)
	hasAssignment := f.assignmentID != ""
	running := f.lifecycle == state.LifecycleRunning
	if hasAssignment {
		if !agentPhase || !running {
			return nil, nil, fmt.Errorf("%w: an assignment under phase %s / lifecycle %s", ErrCorruptState, f.phase, f.lifecycle)
		}
		if recoveryPresent {
			return nil, nil, nil // a required recovery dominates the agent turn
		}
		role := spec.Role
		id := f.assignmentID
		return &role, &id, nil
	}
	// A running agent phase normally needs an owner; a pending recovery is the
	// next actor instead.
	if agentPhase && running && !recoveryPresent {
		return nil, nil, fmt.Errorf("%w: running agent phase %s with no assignment", ErrCorruptState, f.phase)
	}
	return nil, nil, nil
}

func projectStop(f runFacts) (*StopProjection, error) {
	if state.IsFailureLifecycle(f.lifecycle) {
		if f.failure == nil {
			return nil, fmt.Errorf("%w: a failed lifecycle has no failure projection", ErrCorruptState)
		}
		return &StopProjection{Kind: "failure", Code: f.failure.code, Reason: f.failure.reason, NextAction: f.failure.nextAction, AtRevision: f.failure.atRevision}, nil
	}
	// A recovery is reported only on a running run (the same lifecycle rule wait
	// applies): a terminal/paused/gated run is not "recovery required".
	if f.recovery != nil && f.lifecycle == state.LifecycleRunning {
		return &StopProjection{Kind: "recovery", Code: f.recovery.code, Reason: f.recovery.reason, NextAction: f.recovery.nextAction, AtRevision: f.recovery.atRevision}, nil
	}
	return nil, nil
}

func projectCaps(rs state.RunState) CapsReport {
	b := rs.EffectivePolicy.Budgets
	c := rs.Counters
	return CapsReport{
		RunTurns:     scalarCap(len(rs.AcceptedTurns), rs.EffectivePolicy.Limits.MaxRunTurns),
		PlanRounds:   scalarCap(c.PlanRevisions, b.PlanRounds),
		TestRounds:   scalarCap(c.TestFixes, b.TestRounds),
		VerifyRounds: scalarCap(c.VerifyFixes, b.VerifyRounds),
		CheckpointRounds: CheckpointCap{
			LimitPerStep: b.CheckpointRounds,
			Steps:        stepCaps(b.CheckpointRounds, c.StepFixes),
			Mechanism:    capMechanismCounter,
		},
	}
}

func scalarCap(used, limit int) CapCounter {
	return CapCounter{Used: used, Limit: limit, Remaining: clampZero(limit - used), ExceededBy: clampZero(used - limit), Mechanism: capMechanismCounter}
}

func stepCaps(limitPerStep int, stepFixes []int) []StepCap {
	steps := make([]StepCap, 0, len(stepFixes))
	for i, used := range stepFixes {
		steps = append(steps, StepCap{StepIndex: i, Used: used, Remaining: clampZero(limitPerStep - used), ExceededBy: clampZero(used - limitPerStep)})
	}
	return steps
}

func clampZero(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// validateHonesty checks the labels are well-formed. Its errors are value-free:
// they never echo the (seam-supplied) tier, capability name, or mechanism, which
// could carry arbitrary text.
func validateHonesty(l HonestyLabels) error {
	if l.Tier != TierProtocolOnly && l.Tier != TierManaged {
		return fmt.Errorf("%w: unknown tier", ErrHonestySource)
	}
	if !isCanonicalID(l.TierMechanism) {
		return fmt.Errorf("%w: tier mechanism is not a canonical identifier", ErrHonestySource)
	}
	if tierMechanisms[l.Tier] != l.TierMechanism {
		return fmt.Errorf("%w: tier mechanism does not back the tier", ErrHonestySource)
	}
	if len(l.Capabilities) > 64 {
		return fmt.Errorf("%w: too many capabilities", ErrHonestySource)
	}
	seen := map[string]bool{}
	for _, c := range l.Capabilities {
		if !isCanonicalID(c.Name) || !isCanonicalID(c.Mechanism) {
			return fmt.Errorf("%w: a capability name and mechanism must be canonical identifiers", ErrHonestySource)
		}
		if !capabilityStatuses[c.Status] {
			return fmt.Errorf("%w: a capability has an unknown status", ErrHonestySource)
		}
		if seen[c.Name] {
			return fmt.Errorf("%w: a duplicate capability", ErrHonestySource)
		}
		seen[c.Name] = true
	}
	return nil
}

// isCanonicalID is a bounded lowercase-hyphen slug, so capability/tier mechanism
// labels are stable identifiers, not free text.
func isCanonicalID(s string) bool {
	if len(s) == 0 || len(s) > 64 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// cloneLabels copies the labels and sorts capabilities by name, so a set
// returned from a map produces deterministic status bytes.
func cloneLabels(l HonestyLabels) HonestyLabels {
	out := HonestyLabels{Tier: l.Tier, TierMechanism: l.TierMechanism, Capabilities: append([]Capability{}, l.Capabilities...)}
	sort.Slice(out.Capabilities, func(i, j int) bool { return out.Capabilities[i].Name < out.Capabilities[j].Name })
	return out
}

// semanticValidate enforces every cross-field invariant the JSON schema cannot,
// so a mutated public StatusReport cannot marshal contradictory wire data.
func (s StatusReport) semanticValidate() error {
	fail := func(msg string) error { return fmt.Errorf("%w: %s", ErrAssignmentInvalid, msg) }
	if s.ProtocolVersion != protocol.SupportedVersion || s.MessageType != statusType {
		return fail("wrong protocol_version or message_type")
	}
	if s.RunID == "" || s.Revision == 0 {
		return fail("missing run_id or revision")
	}

	// Ownership. A recovery stop dominates the agent turn, so it excludes an owner
	// and lets a running agent phase be ownerless.
	recoveryStop := s.Stop != nil && s.Stop.Kind == "recovery"
	spec, agentPhase := TurnSpec(s.Phase)
	running := s.Lifecycle == state.LifecycleRunning
	hasWhose, hasTurn := s.WhoseTurn != nil, s.TurnID != nil
	if hasWhose != hasTurn {
		return fail("whose_turn and turn_id must appear together")
	}
	if hasWhose {
		if recoveryStop {
			return fail("a recovery stop excludes an owner")
		}
		if !agentPhase || !running {
			return fail("an owner requires a running agent phase")
		}
		if *s.WhoseTurn != spec.Role {
			return fail("whose_turn disagrees with the phase")
		}
	} else if agentPhase && running && !recoveryStop {
		return fail("a running agent phase must have an owner")
	}

	// Gate / pause coherence.
	if s.GateID != nil {
		if s.Phase != state.PhaseAwaitGuidance || s.Lifecycle != state.LifecyclePaused {
			return fail("a gate requires paused AWAIT_GUIDANCE")
		}
		if hasTurn {
			return fail("a gate excludes a turn")
		}
	} else if s.Phase == state.PhaseAwaitGuidance {
		return fail("AWAIT_GUIDANCE requires a gate")
	}
	if s.Lifecycle == state.LifecyclePaused && s.Phase != state.PhaseAwaitGuidance {
		return fail("a paused lifecycle only at AWAIT_GUIDANCE")
	}

	// Stop.
	if state.IsFailureLifecycle(s.Lifecycle) {
		if s.Stop == nil || s.Stop.Kind != "failure" {
			return fail("a failure lifecycle needs a failure stop")
		}
	} else if s.Stop != nil && s.Stop.Kind == "failure" {
		return fail("a failure stop requires a failure lifecycle")
	}
	if s.Stop != nil {
		switch s.Stop.Kind {
		case "failure":
		case "recovery":
			if !running {
				return fail("a recovery stop requires a running lifecycle")
			}
		default:
			return fail("unknown stop kind")
		}
		if s.Stop.Code == "" || s.Stop.Reason == "" || s.Stop.NextAction == "" {
			return fail("a stop needs code, reason, and next_action")
		}
		// Every stop field must already be redacted; a mutated report must not
		// carry a secret to the wire, and control fields must not be silently
		// rewritten.
		for _, f := range []string{s.Stop.Code, s.Stop.Reason, s.Stop.NextAction} {
			if redact.Text(f) != f {
				return fail("a stop field is not redacted")
			}
		}
		if s.Stop.AtRevision == 0 || s.Stop.AtRevision > s.Revision {
			return fail("stop at_revision is out of range")
		}
	}

	// Caps.
	for _, c := range []CapCounter{s.Caps.RunTurns, s.Caps.PlanRounds, s.Caps.TestRounds, s.Caps.VerifyRounds} {
		if err := validateScalarCap(c); err != nil {
			return err
		}
	}
	if err := validateCheckpointCap(s.Caps.CheckpointRounds); err != nil {
		return err
	}

	return validateHonesty(s.Honesty)
}

func validateScalarCap(c CapCounter) error {
	fail := func(msg string) error { return fmt.Errorf("%w: %s", ErrAssignmentInvalid, msg) }
	if c.Used < 0 || c.Limit < 0 {
		return fail("cap used/limit must be non-negative")
	}
	if c.Remaining != clampZero(c.Limit-c.Used) || c.ExceededBy != clampZero(c.Used-c.Limit) {
		return fail("cap arithmetic is inconsistent")
	}
	if c.Mechanism != capMechanismCounter {
		return fail("cap mechanism is wrong")
	}
	return nil
}

func validateCheckpointCap(c CheckpointCap) error {
	fail := func(msg string) error { return fmt.Errorf("%w: %s", ErrAssignmentInvalid, msg) }
	if c.LimitPerStep < 0 || c.Mechanism != capMechanismCounter {
		return fail("checkpoint cap limit/mechanism is wrong")
	}
	for i, st := range c.Steps {
		if st.StepIndex != i {
			return fail("checkpoint steps must be exactly ascending from zero")
		}
		if st.Used < 0 || st.Remaining != clampZero(c.LimitPerStep-st.Used) || st.ExceededBy != clampZero(st.Used-c.LimitPerStep) {
			return fail("checkpoint step arithmetic is inconsistent")
		}
	}
	return nil
}

// validate fully checks the report: the discriminant invariants plus the schema.
func (s StatusReport) validate() error {
	_, err := s.Marshal()
	return err
}

// Marshal fully validates the report and returns canonical wire bytes.
func (s StatusReport) Marshal() ([]byte, error) {
	if err := s.semanticValidate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("transport: marshal status: %w", err)
	}
	canon, err := protocol.Validate(statusType, raw)
	if err != nil {
		return nil, fmt.Errorf("transport: status fails its schema: %w", err)
	}
	return canon, nil
}
