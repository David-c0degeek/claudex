package transport

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

const (
	statusType          = "status"
	capMechanismCounter = "durable-state-counter"
)

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

// HonestyLabels are the tier and capabilities the honesty source reports.
type HonestyLabels struct {
	Tier         Tier         `json:"tier"`
	Capabilities []Capability `json:"capabilities"`
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
	whose, turnID, err := projectOwner(facts)
	if err != nil {
		return StatusReport{}, err
	}
	stop, err := projectStop(facts)
	if err != nil {
		return StatusReport{}, err
	}

	labels, err := honesty(StatusInput{RunID: rs.RunID, Revision: rs.Revision, Phase: rs.Phase, Lifecycle: rs.Lifecycle})
	if err != nil {
		return StatusReport{}, ErrHonestySource // value-free sentinel; never surface the seam's string
	}
	if err := validateHonesty(labels); err != nil {
		return StatusReport{}, err
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
		Honesty:         cloneLabels(labels),
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
func projectOwner(f runFacts) (*Role, *string, error) {
	spec, agentPhase := TurnSpec(f.phase)
	hasAssignment := f.assignmentID != ""
	running := f.lifecycle == state.LifecycleRunning
	if hasAssignment {
		if !agentPhase || !running {
			return nil, nil, fmt.Errorf("%w: an assignment under phase %s / lifecycle %s", ErrCorruptState, f.phase, f.lifecycle)
		}
		role := spec.Role
		id := f.assignmentID
		return &role, &id, nil
	}
	if agentPhase && running {
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
	if f.recovery != nil {
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

func validateHonesty(l HonestyLabels) error {
	if l.Tier != TierProtocolOnly && l.Tier != TierManaged {
		return fmt.Errorf("%w: unknown tier %q", ErrHonestySource, l.Tier)
	}
	if len(l.Capabilities) > 64 {
		return fmt.Errorf("%w: too many capabilities", ErrHonestySource)
	}
	seen := map[string]bool{}
	for _, c := range l.Capabilities {
		if c.Name == "" || c.Mechanism == "" {
			return fmt.Errorf("%w: a capability needs a name and mechanism", ErrHonestySource)
		}
		if !capabilityStatuses[c.Status] {
			return fmt.Errorf("%w: capability %q has an unknown status", ErrHonestySource, c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("%w: duplicate capability %q", ErrHonestySource, c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}

func cloneLabels(l HonestyLabels) HonestyLabels {
	out := HonestyLabels{Tier: l.Tier}
	if l.Capabilities != nil {
		out.Capabilities = append([]Capability(nil), l.Capabilities...)
	} else {
		out.Capabilities = []Capability{}
	}
	return out
}

// validate fully checks the report: the schema plus the discriminant invariants.
func (s StatusReport) validate() error {
	_, err := s.Marshal()
	return err
}

// Marshal validates the report against its schema and returns canonical wire
// bytes.
func (s StatusReport) Marshal() ([]byte, error) {
	if s.Stop != nil && s.Stop.Kind != "failure" && s.Stop.Kind != "recovery" {
		return nil, fmt.Errorf("%w: unknown stop kind", ErrAssignmentInvalid)
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
