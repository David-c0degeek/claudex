package state

import (
	"fmt"
	"reflect"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/protocol"
)

// emptyCheckSetDigest pins the canonical digest of the empty implementation-check
// array, so a check-set ref with no keys is bound to the exact canonical bytes of
// `[]` rather than an arbitrary digest.
var emptyCheckSetDigest = mustCanonDigest("[]")

func mustCanonDigest(raw string) string {
	d, err := canonjson.Digest([]byte(raw))
	if err != nil {
		panic("state: canonical digest of " + raw + ": " + err.Error())
	}
	return d
}

// cloneKeys deep-copies a key slice while preserving nil-vs-non-nil-empty: a
// non-nil empty set stays a non-nil empty set, so an empty check set keeps its one
// durable encoding across generations (append([]string(nil), empty...) would
// collapse it to nil).
func cloneKeys(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string{}, in...)
}

// qualityPauseTable is the closed set of legal quality-budget pauses: which phase
// the budget exhausted in, which phase the run resumes into, the budget kind, and
// the FIX return target it restores. Origin and resume differ for every entry,
// which is why both are stored.
var qualityPauseTable = []struct {
	origin, resume Phase
	kind           BudgetKind
	fixReturn      Phase
}{
	{PhasePlanCritique, PhasePlanRevise, BudgetPlan, ""},
	{PhaseCheckpoint, PhaseFix, BudgetCheckpoint, PhaseCheckpoint},
	{PhaseTests, PhaseFix, BudgetTest, PhaseTests},
	{PhaseVerify, PhaseFix, BudgetVerify, PhaseVerify},
}

// isImplementationPhase reports whether a phase carries an agreed plan and cursor.
func isImplementationPhase(p Phase) bool {
	switch p {
	case PhaseImplementStep, PhaseCheckpoint, PhaseFix, PhaseTests, PhaseVerify, PhaseDone:
		return true
	}
	return false
}

// isHumanOriginPhase reports whether a phase may raise a human-decision gate. Every
// submit phase may request a decision; TESTS is coordinator-authored and cannot.
func isHumanOriginPhase(p Phase) bool {
	switch p {
	case PhasePlanDraft, PhasePlanCritique, PhasePlanRevise, PhaseImplementStep,
		PhaseCheckpoint, PhaseFix, PhaseVerify:
		return true
	}
	return false
}

// isFixReturnPhase reports whether a phase is a legal FIX return target.
func isFixReturnPhase(p Phase) bool {
	return p == PhaseCheckpoint || p == PhaseTests || p == PhaseVerify
}

// effectivePhase is the phase the run is (or, while paused, will resume) acting in.
// All phase-family shape checks run on it, so a paused generation is validated as
// the phase it will return to.
func effectivePhase(rs *RunState) Phase {
	if rs.Pause != nil {
		return rs.Pause.ResumePhase
	}
	return rs.Phase
}

// effectiveFixReturn is the FIX return target in force (from the pause while paused).
func effectiveFixReturn(rs *RunState) Phase {
	if rs.Pause != nil {
		return rs.Pause.FixReturn
	}
	return rs.FixReturn
}

// effAtPlanEnd reports whether the effective phase sits at the plan end
// (cursor == StepCount): TESTS/VERIFY/DONE, and a FIX returning to TESTS/VERIFY.
func effAtPlanEnd(eff, effFix Phase) bool {
	switch eff {
	case PhaseTests, PhaseVerify, PhaseDone:
		return true
	case PhaseFix:
		return effFix == PhaseTests || effFix == PhaseVerify
	}
	return false
}

// validateV5Shape enforces the durable local shape invariants the phase engine
// relies on: gate coherence, phase-family ownership, cursor bounds, and the
// presence rules for the VERIFY and FIX contexts. It encodes no phase edges.
func validateV5Shape(rs *RunState) error {
	// Gate coherence: pause, gate, AWAIT_GUIDANCE, and LifecyclePaused are all
	// present/true together or all absent/false together — the same exact shape the
	// wait/status contract assumes. D019 LifecyclePausedBudget is distinct and
	// carries none of them.
	hasPause := rs.Pause != nil
	hasGate := rs.Gate != nil
	isAwait := rs.Phase == PhaseAwaitGuidance
	isPaused := rs.Lifecycle == LifecyclePaused
	if !(hasPause == hasGate && hasGate == isAwait && isAwait == isPaused) {
		return fmt.Errorf("gate coherence: pause=%v gate=%v await=%v paused=%v must all agree", hasPause, hasGate, isAwait, isPaused)
	}
	if rs.Pause != nil {
		if err := validatePauseShape(rs); err != nil {
			return err
		}
	}

	eff := effectivePhase(rs)
	effFix := effectiveFixReturn(rs)

	hasCandidate := rs.CandidatePlan != nil
	hasChecks := rs.CandidateChecks != nil
	hasFindings := rs.PendingFindings != nil
	hasAgreed := rs.AgreedPlan != nil
	hasCursor := rs.StepIndex != nil

	// CandidateChecks is present exactly when CandidatePlan is (including the
	// canonical empty-set ref).
	if hasCandidate != hasChecks {
		return fmt.Errorf("candidate_checks must be present exactly when candidate_plan is")
	}

	// Phase-family ownership, on the effective phase.
	switch {
	case eff == PhaseInit || eff == PhasePlanDraft:
		if hasCandidate || hasFindings || hasAgreed || hasCursor {
			return fmt.Errorf("phase %s must have no plan staging, agreement, or cursor", eff)
		}
	case eff == PhasePlanCritique || eff == PhasePlanRevise:
		if !hasCandidate {
			return fmt.Errorf("phase %s requires a candidate plan and checks", eff)
		}
		if hasAgreed || hasCursor {
			return fmt.Errorf("phase %s must not have an agreed plan or cursor", eff)
		}
		if want := eff == PhasePlanRevise; hasFindings != want {
			return fmt.Errorf("phase %s pending-findings presence must be %v", eff, want)
		}
	case isImplementationPhase(eff):
		if !hasAgreed || !hasCursor {
			return fmt.Errorf("phase %s requires an agreed plan and cursor", eff)
		}
		if hasCandidate || hasFindings {
			return fmt.Errorf("phase %s must not carry candidate staging", eff)
		}
	default:
		return fmt.Errorf("unexpected effective phase %s", eff)
	}

	// The per-step fix vector is exactly the agreed step count, or empty.
	if hasAgreed {
		if len(rs.Counters.StepFixes) != rs.AgreedPlan.Plan.StepCount {
			return fmt.Errorf("step_fixes length %d must equal agreed step_count %d", len(rs.Counters.StepFixes), rs.AgreedPlan.Plan.StepCount)
		}
	} else if len(rs.Counters.StepFixes) != 0 {
		return fmt.Errorf("step_fixes must be empty without an agreed plan")
	}

	// Cursor bounds and position, by effective phase.
	if hasCursor {
		idx := *rs.StepIndex
		sc := rs.AgreedPlan.Plan.StepCount
		if idx < 0 || idx > sc {
			return fmt.Errorf("step_index %d out of range 0..%d", idx, sc)
		}
		if isImplementationPhase(eff) {
			if effAtPlanEnd(eff, effFix) {
				if idx != sc {
					return fmt.Errorf("phase %s requires the cursor at the plan end %d, got %d", eff, sc, idx)
				}
			} else if idx >= sc {
				return fmt.Errorf("phase %s requires the cursor before the plan end, got %d/%d", eff, idx, sc)
			}
		}
	}

	if err := validateVerifyPresence(rs, eff); err != nil {
		return err
	}
	return validateFixReturnPresence(rs, eff)
}

// validatePauseShape enforces the two-kind pause union: the human table (resume ==
// origin, an origin that may raise a gate) and the closed quality table.
func validatePauseShape(rs *RunState) error {
	p := rs.Pause
	if !knownPhases[p.OriginPhase] || !knownPhases[p.ResumePhase] {
		return fmt.Errorf("pause has an unknown origin/resume phase")
	}
	if p.FixReturn != "" && !isFixReturnPhase(p.FixReturn) {
		return fmt.Errorf("pause.fix_return %q is not a legal FIX return target", p.FixReturn)
	}
	if p.Verify != nil && p.Verify.RequiredGeneration == 0 {
		return fmt.Errorf("pause.verify required_generation must be > 0")
	}
	switch p.Kind {
	case PauseHumanDecision:
		if p.Budget != nil {
			return fmt.Errorf("human-decision pause must not carry a budget")
		}
		if p.OriginPhase != p.ResumePhase {
			return fmt.Errorf("human-decision pause must resume to its origin phase")
		}
		if !isHumanOriginPhase(p.OriginPhase) {
			return fmt.Errorf("phase %s cannot raise a human-decision gate", p.OriginPhase)
		}
		if (p.Verify != nil) != (p.ResumePhase == PhaseVerify) {
			return fmt.Errorf("pause.verify presence must match a VERIFY resume")
		}
		if (p.FixReturn != "") != (p.ResumePhase == PhaseFix) {
			return fmt.Errorf("pause.fix_return presence must match a FIX resume")
		}
	case PauseQualityBudget:
		if p.Verify != nil {
			return fmt.Errorf("quality-budget pause must not carry a verify requirement")
		}
		if p.Budget == nil {
			return fmt.Errorf("quality-budget pause requires a budget")
		}
		ok := false
		for _, e := range qualityPauseTable {
			if e.origin == p.OriginPhase && e.resume == p.ResumePhase && e.kind == p.Budget.Kind && e.fixReturn == p.FixReturn {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("quality-budget pause (%s->%s, %s, fix=%q) is not in the closed table", p.OriginPhase, p.ResumePhase, p.Budget.Kind, p.FixReturn)
		}
	default:
		return fmt.Errorf("unknown pause kind %q", p.Kind)
	}
	return nil
}

// validateVerifyPresence enforces where the verify requirement lives: top-level
// only in a running VERIFY, in Pause.Verify only for a human VERIFY gate, and
// present (in exactly one place) iff the effective phase is VERIFY.
func validateVerifyPresence(rs *RunState, eff Phase) error {
	wantTop := rs.Pause == nil && rs.Phase == PhaseVerify
	if (rs.Verify != nil) != wantTop {
		return fmt.Errorf("top-level verify presence must be %v for phase %s (paused=%v)", wantTop, rs.Phase, rs.Pause != nil)
	}
	if rs.Verify != nil && rs.Verify.RequiredGeneration == 0 {
		return fmt.Errorf("verify.required_generation must be > 0")
	}
	hasReq := rs.Verify != nil || (rs.Pause != nil && rs.Pause.Verify != nil)
	if hasReq != (eff == PhaseVerify) {
		return fmt.Errorf("a verify requirement must be present exactly for an effective VERIFY phase")
	}
	return nil
}

// validateFixReturnPresence enforces the FIX return target's location: top-level
// only in a running FIX, in Pause.FixReturn only for a FIX-resume gate, and present
// iff the effective phase is FIX.
func validateFixReturnPresence(rs *RunState, eff Phase) error {
	wantTop := rs.Pause == nil && rs.Phase == PhaseFix
	if (rs.FixReturn != "") != wantTop {
		return fmt.Errorf("top-level fix_return presence must be %v for phase %s", wantTop, rs.Phase)
	}
	if rs.FixReturn != "" && !isFixReturnPhase(rs.FixReturn) {
		return fmt.Errorf("fix_return %q is not a legal FIX return target", rs.FixReturn)
	}
	hasFix := rs.FixReturn != "" || (rs.Pause != nil && rs.Pause.FixReturn != "")
	if hasFix != (eff == PhaseFix) {
		return fmt.Errorf("a fix return target must be present exactly for an effective FIX phase")
	}
	return nil
}

// validateV5Refs enforces the well-formedness and accepted-event provenance of the
// v5 references, using only durable fields (AcceptedTurns).
func validateV5Refs(rs *RunState) error {
	if rs.CandidatePlan != nil {
		if err := validatePlanRef("candidate_plan", *rs.CandidatePlan, rs, PhasePlanDraft, PhasePlanRevise); err != nil {
			return err
		}
	}
	if rs.CandidateChecks != nil {
		if err := validateCheckSet("candidate_checks", *rs.CandidateChecks); err != nil {
			return err
		}
	}
	if rs.PendingFindings != nil {
		fo := rs.PendingFindings
		if len(fo.Keys) == 0 {
			return fmt.Errorf("pending_findings must be non-empty")
		}
		if err := validateSortedKeys("pending_findings", fo.Keys); err != nil {
			return err
		}
		if err := validateEventRef("pending_findings.source", fo.Source, rs, PhasePlanCritique); err != nil {
			return err
		}
	}
	if rs.AgreedPlan != nil {
		ag := rs.AgreedPlan
		if err := validatePlanRef("agreed_plan.plan", ag.Plan, rs, PhasePlanDraft, PhasePlanRevise); err != nil {
			return err
		}
		if err := validateCheckSet("agreed_plan.checks", ag.Checks); err != nil {
			return err
		}
		if err := validateEventRef("agreed_plan.critique", ag.Critique, rs, PhasePlanCritique); err != nil {
			return err
		}
		if ag.AgreedRevision == 0 || ag.AgreedRevision > rs.Revision {
			return fmt.Errorf("agreed_plan.agreed_revision %d out of range 1..%d", ag.AgreedRevision, rs.Revision)
		}
		// The agreement was reached at the revision the agreeing critique was accepted.
		if rs.AcceptedTurns[ag.Critique.TurnID].Receipt.Revision != ag.AgreedRevision {
			return fmt.Errorf("agreed_plan.critique was not accepted at the agreed revision %d", ag.AgreedRevision)
		}
	}
	if rs.Pause != nil {
		if err := validatePauseSource(rs); err != nil {
			return err
		}
	}
	return nil
}

func validatePlanRef(field string, pr PlanRef, rs *RunState, allowed ...Phase) error {
	if !isHex64(pr.Digest) {
		return fmt.Errorf("%s.digest is not a 64-char lower-hex sha256", field)
	}
	if pr.StepCount < protocol.MinPlanSteps || pr.StepCount > protocol.MaxPlanSteps {
		return fmt.Errorf("%s.step_count %d out of range %d..%d", field, pr.StepCount, protocol.MinPlanSteps, protocol.MaxPlanSteps)
	}
	return validateEventRef(field+".source", pr.Source, rs, allowed...)
}

func validateCheckSet(field string, cs CheckSetRef) error {
	if cs.Keys == nil {
		return fmt.Errorf("%s.keys must be a non-nil array (use [] for the empty set)", field)
	}
	if err := validateSortedKeys(field, cs.Keys); err != nil {
		return err
	}
	if !isHex64(cs.Digest) {
		return fmt.Errorf("%s.digest is not a 64-char lower-hex sha256", field)
	}
	// The empty-set digest is biconditional: a key set is empty exactly when it
	// carries the canonical digest of the empty array, so neither a non-empty set
	// with the empty digest nor an empty set with any other digest can slip through.
	if (len(cs.Keys) == 0) != (cs.Digest == emptyCheckSetDigest) {
		return fmt.Errorf("%s: keys are empty iff the digest is the canonical empty-array digest", field)
	}
	return nil
}

// validateSortedKeys enforces the shared key grammar (canonical, no duplicates) and
// a strictly increasing order, so a set has one durable encoding.
func validateSortedKeys(field string, keys []string) error {
	if err := protocol.ValidateKeySet(field, keys); err != nil {
		return err
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			return fmt.Errorf("%s keys must be strictly sorted", field)
		}
	}
	return nil
}

// validateEventRef verifies an accepted agent-artifact provenance ref: a non-empty
// canonical turn id present in AcceptedTurns whose recorded phase is one of allowed
// and whose artifact digest matches. The coordinator TESTS source (empty turn id)
// is validated by its containing pause, never here.
func validateEventRef(field string, ev EventRef, rs *RunState, allowed ...Phase) error {
	if ev.TurnID == "" {
		return fmt.Errorf("%s requires an accepted turn id", field)
	}
	if !validID(ev.TurnID) {
		return fmt.Errorf("%s turn id %q is not canonical", field, ev.TurnID)
	}
	if !isHex64(ev.Digest) {
		return fmt.Errorf("%s digest is not a 64-char lower-hex sha256", field)
	}
	at, ok := rs.AcceptedTurns[ev.TurnID]
	if !ok {
		return fmt.Errorf("%s references turn %q not in accepted_turns", field, ev.TurnID)
	}
	if at.ArtifactDigest != ev.Digest {
		return fmt.Errorf("%s digest disagrees with the accepted turn", field)
	}
	if !phaseIn(at.Phase, allowed) {
		return fmt.Errorf("%s accepted phase %s is not an allowed source phase", field, at.Phase)
	}
	return nil
}

// validatePauseSource checks the pause's provenance: an agent gate's source is the
// accepted event whose phase equals the origin phase; the sole coordinator case is
// a TESTS quality-budget gate (empty turn id, hex digest).
func validatePauseSource(rs *RunState) error {
	p := rs.Pause
	if p.Kind == PauseQualityBudget && p.OriginPhase == PhaseTests {
		if p.Source.TurnID != "" {
			return fmt.Errorf("a TESTS quality-budget pause source must have no turn id")
		}
		if !isHex64(p.Source.Digest) {
			return fmt.Errorf("pause.source digest is not a 64-char lower-hex sha256")
		}
		return nil
	}
	if err := validateEventRef("pause.source", p.Source, rs, p.OriginPhase); err != nil {
		return err
	}
	// An agent gate's source was accepted at the same revision the gate was issued.
	if rs.Gate == nil || rs.AcceptedTurns[p.Source.TurnID].Receipt.Revision != rs.Gate.IssuedRevision {
		return fmt.Errorf("pause.source must be accepted at the gate's issued revision")
	}
	return nil
}

// validateBudgetHonesty requires a quality-budget label to be truthful: the durable
// counter it names has reached its frozen policy limit at the paused generation. In
// the current no-grants model the counter equals the limit exactly (the blocked
// action never issued, so it never incremented). Subject 05 extends this to grants.
func validateBudgetHonesty(rs *RunState) error {
	p := rs.Pause
	if p == nil || p.Kind != PauseQualityBudget {
		return nil
	}
	lim := rs.EffectivePolicy.Budgets
	switch p.Budget.Kind {
	case BudgetPlan:
		if rs.Counters.PlanRevisions != lim.PlanRounds {
			return fmt.Errorf("plan-budget pause but plan_revisions %d != frozen limit %d", rs.Counters.PlanRevisions, lim.PlanRounds)
		}
	case BudgetCheckpoint:
		if rs.StepIndex == nil || *rs.StepIndex >= len(rs.Counters.StepFixes) {
			return fmt.Errorf("checkpoint-budget pause without a valid step cursor")
		}
		if got := rs.Counters.StepFixes[*rs.StepIndex]; got != lim.CheckpointRounds {
			return fmt.Errorf("checkpoint-budget pause but step fixes %d != frozen limit %d", got, lim.CheckpointRounds)
		}
	case BudgetTest:
		if rs.Counters.TestFixes != lim.TestRounds {
			return fmt.Errorf("test-budget pause but test_fixes %d != frozen limit %d", rs.Counters.TestFixes, lim.TestRounds)
		}
	case BudgetVerify:
		if rs.Counters.VerifyFixes != lim.VerifyRounds {
			return fmt.Errorf("verify-budget pause but verify_fixes %d != frozen limit %d", rs.Counters.VerifyFixes, lim.VerifyRounds)
		}
	default:
		return fmt.Errorf("unknown budget kind %q", p.Budget.Kind)
	}
	return nil
}

// validateV5Init requires a first generation to carry none of the v5 working set.
func validateV5Init(rs *RunState) error {
	if rs.CandidatePlan != nil || rs.CandidateChecks != nil || rs.PendingFindings != nil ||
		rs.AgreedPlan != nil || rs.StepIndex != nil || rs.FixReturn != "" ||
		rs.Verify != nil || rs.Pause != nil {
		return fmt.Errorf("initial state must have no plan/verify/pause working set")
	}
	return nil
}

// validateV5Transition enforces the durable-shape transition rules: the immutable
// agreement, the monotonic cursor, freshly-sourced staging, single-generation
// promotion, and the transfer/restore of the FIX/VERIFY context across a gate.
func validateV5Transition(old, next *RunState) error {
	// AgreedPlan is write-once.
	if old.AgreedPlan != nil && !reflect.DeepEqual(old.AgreedPlan, next.AgreedPlan) {
		return fmt.Errorf("agreed_plan is immutable once set")
	}
	// The cursor never rewinds.
	if old.StepIndex != nil {
		if next.StepIndex == nil || *next.StepIndex < *old.StepIndex {
			return fmt.Errorf("step_index must not rewind")
		}
	}
	// A newly set/replaced CandidatePlan is sourced by an event accepted now.
	if next.CandidatePlan != nil && !reflect.DeepEqual(old.CandidatePlan, next.CandidatePlan) {
		if err := requireFreshSource("candidate_plan.source", next.CandidatePlan.Source, next); err != nil {
			return err
		}
	}
	// Newly set PendingFindings are sourced by the critique accepted now.
	if next.PendingFindings != nil && !reflect.DeepEqual(old.PendingFindings, next.PendingFindings) {
		if err := requireFreshSource("pending_findings.source", next.PendingFindings.Source, next); err != nil {
			return err
		}
	}
	// Agreement promotion (nil -> present) is a single, exact generation.
	if old.AgreedPlan == nil && next.AgreedPlan != nil {
		if old.CandidatePlan == nil || !reflect.DeepEqual(old.CandidatePlan, &next.AgreedPlan.Plan) {
			return fmt.Errorf("agreement must promote the exact prior candidate plan")
		}
		if next.CandidatePlan != nil || next.CandidateChecks != nil || next.PendingFindings != nil {
			return fmt.Errorf("agreement must clear all candidate staging")
		}
		if next.AgreedPlan.AgreedRevision != next.Revision {
			return fmt.Errorf("agreed_plan.agreed_revision must be the promotion revision %d", next.Revision)
		}
		if err := requireFreshSource("agreed_plan.critique", next.AgreedPlan.Critique, next); err != nil {
			return err
		}
		if len(next.Counters.StepFixes) != next.AgreedPlan.Plan.StepCount {
			return fmt.Errorf("agreement must size step_fixes to the step count")
		}
		for i, v := range next.Counters.StepFixes {
			if v != 0 {
				return fmt.Errorf("agreement step_fixes[%d] must start at zero", i)
			}
		}
		if next.StepIndex == nil || *next.StepIndex != 0 {
			return fmt.Errorf("agreement must initialize the cursor to 0")
		}
	}
	// A newly opened agent gate's source is the event accepted at the gate revision.
	if next.Pause != nil && old.Pause == nil && next.Pause.Source.TurnID != "" {
		if err := requireFreshSource("pause.source", next.Pause.Source, next); err != nil {
			return err
		}
	}
	if err := validateCandidateChecksTransition(old, next); err != nil {
		return err
	}
	if err := validatePausedImmutability(old, next); err != nil {
		return err
	}
	if err := validateSamePhasePreservation(old, next); err != nil {
		return err
	}
	return validateContextTransfer(old, next)
}

// validateCandidateChecksTransition pins the materialized check-set lifecycle: the
// first candidate initializes the canonical empty set; thereafter the set changes
// only with a freshly accepted, actionable critique (the one that sets
// PendingFindings this generation). A plan revision, which carries no check ops,
// therefore preserves it, and a human-gated/inconclusive critique cannot change it.
func validateCandidateChecksTransition(old, next *RunState) error {
	if next.CandidateChecks == nil || reflect.DeepEqual(old.CandidateChecks, next.CandidateChecks) {
		return nil
	}
	if old.CandidatePlan == nil { // first candidate creation (from PLAN_DRAFT)
		if len(next.CandidateChecks.Keys) != 0 || next.CandidateChecks.Digest != emptyCheckSetDigest {
			return fmt.Errorf("the initial candidate_checks must be the canonical empty set")
		}
		return nil
	}
	freshCritique := next.PendingFindings != nil && !reflect.DeepEqual(old.PendingFindings, next.PendingFindings)
	if !freshCritique {
		return fmt.Errorf("candidate_checks may change only with a freshly accepted actionable critique")
	}
	return nil
}

// validatePausedImmutability freezes the suspended transition while a pause remains:
// the pause record, its gate, the cursor, and every counter are byte-identical until
// the pause is cleared, so nothing rewrites the parked decision or charges a counter
// before resumption.
func validatePausedImmutability(old, next *RunState) error {
	if old.Pause == nil || next.Pause == nil {
		return nil // opening or clearing a pause is governed by the other rules
	}
	if !reflect.DeepEqual(old.Pause, next.Pause) {
		return fmt.Errorf("pause is immutable while the run is paused")
	}
	if !reflect.DeepEqual(old.Gate, next.Gate) {
		return fmt.Errorf("gate is immutable while the run is paused")
	}
	if !reflect.DeepEqual(old.StepIndex, next.StepIndex) {
		return fmt.Errorf("step_index is immutable while the run is paused")
	}
	if !reflect.DeepEqual(old.Counters, next.Counters) {
		return fmt.Errorf("counters are immutable while the run is paused")
	}
	return nil
}

// validateSamePhasePreservation keeps the VERIFY attempt and the FIX return target
// stable while the run stays in that phase (a reissued assignment must not silently
// become another verify attempt). The transfer/restore rules add to these.
func validateSamePhasePreservation(old, next *RunState) error {
	running := old.Pause == nil && next.Pause == nil
	if running && old.Phase == PhaseVerify && next.Phase == PhaseVerify {
		if !reflect.DeepEqual(old.Verify, next.Verify) {
			return fmt.Errorf("verify requirement must be preserved while the run stays in VERIFY")
		}
		if old.Counters.VerifyFixes != next.Counters.VerifyFixes {
			return fmt.Errorf("verify_fixes must not change while the run stays in VERIFY")
		}
	}
	if running && old.Phase == PhaseFix && next.Phase == PhaseFix {
		if old.FixReturn != next.FixReturn {
			return fmt.Errorf("fix_return must be preserved while the run stays in FIX")
		}
	}
	return nil
}

// validateContextTransfer enforces that a human gate raised from VERIFY or FIX
// carries the exact requirement/return target into the pause, and restoring it
// brings back the same value (never a consumed turn identity).
func validateContextTransfer(old, next *RunState) error {
	// VERIFY -> human gate: preserve the verify requirement.
	if old.Pause == nil && old.Phase == PhaseVerify && next.Pause != nil && next.Pause.ResumePhase == PhaseVerify {
		if old.Verify == nil || next.Pause.Verify == nil || *old.Verify != *next.Pause.Verify {
			return fmt.Errorf("pausing VERIFY must preserve the verify requirement")
		}
	}
	// Human gate -> VERIFY: restore the same verify requirement.
	if old.Pause != nil && old.Pause.ResumePhase == PhaseVerify && next.Pause == nil && next.Phase == PhaseVerify {
		if old.Pause.Verify == nil || next.Verify == nil || *old.Pause.Verify != *next.Verify {
			return fmt.Errorf("restoring VERIFY must preserve the verify requirement")
		}
	}
	// FIX -> human gate: preserve the FIX return target.
	if old.Pause == nil && old.Phase == PhaseFix && next.Pause != nil && next.Pause.ResumePhase == PhaseFix {
		if old.FixReturn == "" || old.FixReturn != next.Pause.FixReturn {
			return fmt.Errorf("pausing FIX must preserve the fix return target")
		}
	}
	// Human gate -> FIX: restore the same return target.
	if old.Pause != nil && old.Pause.ResumePhase == PhaseFix && next.Pause == nil && next.Phase == PhaseFix {
		if old.Pause.FixReturn == "" || old.Pause.FixReturn != next.FixReturn {
			return fmt.Errorf("restoring FIX must preserve the fix return target")
		}
	}
	return nil
}

// requireFreshSource verifies an accepted-event source is the turn newly accepted
// at the resulting revision (its receipt binds to next.Revision).
func requireFreshSource(field string, ev EventRef, next *RunState) error {
	if ev.TurnID == "" {
		return fmt.Errorf("%s must be a freshly accepted agent event", field)
	}
	at, ok := next.AcceptedTurns[ev.TurnID]
	if !ok {
		return fmt.Errorf("%s source turn %q is not accepted", field, ev.TurnID)
	}
	if at.Receipt.Revision != next.Revision {
		return fmt.Errorf("%s source must be accepted at the resulting revision %d, got %d", field, next.Revision, at.Receipt.Revision)
	}
	return nil
}

// addEventRefControls registers an EventRef's identity fields for secret rejection
// (a secret in any of them is refused, never rewritten).
func addEventRefControls(m map[string]string, prefix string, ev EventRef) {
	if ev.TurnID != "" {
		m[prefix+".turn_id"] = ev.TurnID
	}
	m[prefix+".digest"] = ev.Digest
}

func phaseIn(p Phase, set []Phase) bool {
	for _, q := range set {
		if p == q {
			return true
		}
	}
	return false
}

// v5CounterCeiling rejects a counter above the shared budget ceiling, so the
// derived verify attempt (VerifyFixes+1) stays within 1..config.MaxBudget+1 and the
// state agrees with the frozen policy ceiling.
func v5CounterCeiling(c Counters) error {
	if c.PlanRevisions > config.MaxBudget || c.TestFixes > config.MaxBudget || c.VerifyFixes > config.MaxBudget {
		return fmt.Errorf("counters exceed the ceiling %d", config.MaxBudget)
	}
	for i, v := range c.StepFixes {
		if v > config.MaxBudget {
			return fmt.Errorf("step fix %d exceeds the ceiling %d", i, config.MaxBudget)
		}
	}
	return nil
}
