// Package engine is the pure phase engine: it owns the sole legal phase-edge and
// convergence table for the pairing loop. It is a pure function of the current run
// state plus a projected event; it performs no I/O, mints no identities, and reads
// no files. The outer orchestration/test layer loads and revalidates the immutable
// artifacts, projects a typed Event, calls Evaluate to obtain a Decision, and calls
// Apply to realize that Decision into an already-cloned next run state. State's own
// transition validation is the backstop.
//
// Scope: this build covers PLAN_DRAFT/PLAN_CRITIQUE/PLAN_REVISE, IMPLEMENT_STEP,
// CHECKPOINT, and FIX-returning-to-CHECKPOINT, plus the plan and checkpoint quality
// gates and human-decision gates. The final-checkpoint edge into TESTS and the
// FIX->VERIFY edge are deliberately uncallable (Evaluate returns ErrPhaseUnsupported)
// until the run-lock Registry reauthorization and ownerless-VERIFY support land.
package engine

import (
	"errors"
	"fmt"
	"sort"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

// ErrPhaseUnsupported is returned by Evaluate for a legal-but-not-yet-implemented
// edge (the final-checkpoint transition into TESTS/VERIFY). It produces no Decision.
var ErrPhaseUnsupported = errors.New("engine: phase edge not yet supported")

// ErrSemantic is a projection error: the artifact is schema-valid but semantically
// inconsistent with the current state (a wrong base digest, non-exact revision
// responses, a removed missing check, or a dangling obligation at convergence). A
// semantic error rejects the submit; it never becomes a human gate.
var ErrSemantic = errors.New("engine: semantic projection error")

// ErrBudgetCorrupt is returned when a durable counter already exceeds its frozen
// limit — an impossible state under the current no-grants model, failed closed
// rather than routed to a reviewer retry.
var ErrBudgetCorrupt = errors.New("engine: counter exceeds the frozen budget")

// ErrPhaseMismatch is returned by Evaluate when the event kind is not the one the
// current phase accepts — an exported-boundary guard so a fabricated event cannot
// drive a wrong-phase edge.
var ErrPhaseMismatch = errors.New("engine: event kind does not match the current phase")

// ErrBadDecision is returned by Apply when a Decision is malformed for its route
// (wrong edge, missing/extra payload, or an impossible precondition) before any
// mutation, so a rejected Apply never partially writes.
var ErrBadDecision = errors.New("engine: malformed decision")

// phaseEvent is the closed map from a phase to the one event kind it accepts.
var phaseEvent = map[state.Phase]EventKind{
	state.PhasePlanDraft:     EvPlanDrafted,
	state.PhasePlanCritique:  EvPlanCritiqued,
	state.PhasePlanRevise:    EvPlanRevised,
	state.PhaseImplementStep: EvStepImplemented,
	state.PhaseCheckpoint:    EvStepCheckpointed,
	state.PhaseFix:           EvFixImplemented,
}

func semanticf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrSemantic, fmt.Sprintf(format, a...))
}

// --- materialized artifact facts supplied by the (impure) loader ---

// PlanStep is one materialized plan step.
type PlanStep struct {
	Title       string
	Description string
	Files       []string
	Tests       []string
}

// CanonicalPlan is the full materialized current plan document. Project verifies
// its materialized digest equals cur.CandidatePlan.Digest before using it.
type CanonicalPlan struct {
	Markdown      string
	Steps         []PlanStep
	Risks         []string
	OpenQuestions []string
}

// MaterializedCheck is one established implementation-check obligation (no action).
// Its full content is carried into the durable digest so two obligations that
// differ only in description or evidence are distinct durable sets.
type MaterializedCheck struct {
	Key         string
	Description string
	Evidence    string
	TargetStep  *string // nil = cross-cutting
}

// ProjectionFacts are the materialized artifacts the loader read and revalidated.
// Project rechecks their digests/keys/source against cur before deriving anything.
type ProjectionFacts struct {
	CandidatePlan   CanonicalPlan       // current plan (revise/critique need it)
	CandidateChecks []MaterializedCheck // current materialized check set
	CandidateSource state.EventRef      // the accepted event that produced the current plan
}

// --- projected events ---

// EventKind discriminates a projected event.
type EventKind int

const (
	EvPlanDrafted EventKind = iota
	EvPlanCritiqued
	EvPlanRevised
	EvStepImplemented
	EvStepCheckpointed
	EvFixImplemented
)

// Event is the value-only projection of one accepted submit. Evaluate is a pure
// function of (cur, Event). Source is the SUBMITTED event's digest+turn — it becomes
// an accepted turn only when transport appends the receipt after Apply.
type Event struct {
	Kind   EventKind
	Source state.EventRef

	// Plan/revision-derived:
	Materialized string // materialized plan-document digest
	StepCount    int

	// Review-derived (critique/checkpoint):
	Verdict         string // "AGREE" | "REVISE"
	Actionable      bool   // any blocking/major finding (checkpoint: or !TestsAdequate)
	MissingEvidence bool
	TestsAdequate   bool // checkpoint only
	Decision        bool // requires_human_decision

	// Critique payload (materialized for every valid critique; used only when routed):
	ResultingChecks state.CheckSetRef // check ops applied to the current set
	TargetsValid    bool              // every non-null target matches a unique step title
	Findings        []string          // actionable finding keys (sorted)
}

// --- decision + apply contract ---

// Route is the specific state mutation an accepted event drives.
type Route int

const (
	RouteGate           Route = iota // open a human/quality gate (no state advance)
	RouteDraftAccepted               // set CandidatePlan + canonical-empty checks
	RoutePromote                     // freeze AgreedPlan, clear staging, init cursor/step-fixes
	RouteToRevise                    // set findings + updated checks, ++PlanRevisions
	RouteRetryReview                 // re-issue the reviewer turn, no state change
	RouteReviseAccepted              // replace CandidatePlan, preserve checks, clear findings
	RouteToCheckpoint                // advance to CHECKPOINT (from IMPLEMENT or FIX)
	RouteNextStep                    // advance the cursor to the next step
	RouteToFix                       // ++StepFixes[cursor], set FixReturn=CHECKPOINT
)

// GateSpec describes a gate to open.
type GateSpec struct {
	Kind        state.PauseKind
	OriginPhase state.Phase
	ResumePhase state.Phase
	FixReturn   state.Phase      // set iff ResumePhase==FIX
	Budget      state.BudgetKind // set iff Kind==quality_budget
}

// Decision is the pure, value-only result of Evaluate. It is bound to the source
// phase/revision/event so a decision computed for another turn is unusable.
type Decision struct {
	FromPhase             state.Phase
	ExpectedStateRevision uint64
	Source                state.EventRef
	Next                  state.Phase
	Route                 Route

	Gate     *GateSpec                 // RouteGate
	Plan     *state.PlanRef            // RouteDraftAccepted, RouteReviseAccepted
	Checks   *state.CheckSetRef        // RouteDraftAccepted (empty), RouteToRevise (updated), RoutePromote (freeze)
	Findings *state.FindingObligations // RouteToRevise
}

// Ids are the prepared identities the adapter minted before the CAS loop. Apply
// requires exactly the id the Decision issues.
type Ids struct {
	AssignmentTurnID string
	GateID           string
}

// --- evaluate: the sole phase-edge/convergence table ---

// Evaluate returns the Decision for the current state and a projected event, or
// ErrPhaseUnsupported for the deferred final edge. Pure; mints nothing.
func Evaluate(cur state.RunState, ev Event) (Decision, error) {
	if want, ok := phaseEvent[cur.Phase]; !ok || want != ev.Kind {
		return Decision{}, fmt.Errorf("%w: phase %s, kind %d", ErrPhaseMismatch, cur.Phase, ev.Kind)
	}
	base := Decision{FromPhase: cur.Phase, ExpectedStateRevision: cur.Revision, Source: ev.Source}
	switch ev.Kind {
	case EvPlanDrafted:
		return evalPlanDrafted(cur, ev, base)
	case EvPlanCritiqued:
		return evalPlanCritiqued(cur, ev, base)
	case EvPlanRevised:
		return evalPlanRevised(cur, ev, base)
	case EvStepImplemented:
		return evalStepImplemented(cur, ev, base)
	case EvStepCheckpointed:
		return evalStepCheckpointed(cur, ev, base)
	case EvFixImplemented:
		return evalFixImplemented(cur, ev, base)
	}
	return Decision{}, fmt.Errorf("engine: unknown event kind %d", ev.Kind)
}

func humanGate(base Decision, cur state.RunState, origin state.Phase) Decision {
	g := &GateSpec{Kind: state.PauseHumanDecision, OriginPhase: origin, ResumePhase: origin}
	if origin == state.PhaseFix {
		g.FixReturn = cur.FixReturn
	}
	base.Next = state.PhaseAwaitGuidance
	base.Route = RouteGate
	base.Gate = g
	return base
}

func evalPlanDrafted(cur state.RunState, ev Event, base Decision) (Decision, error) {
	if ev.Decision {
		return humanGate(base, cur, state.PhasePlanDraft), nil
	}
	plan := &state.PlanRef{Source: ev.Source, Digest: ev.Materialized, StepCount: ev.StepCount}
	empty, err := materializeChecks(nil)
	if err != nil {
		return Decision{}, err
	}
	base.Next = state.PhasePlanCritique
	base.Route = RouteDraftAccepted
	base.Plan = plan
	base.Checks = &empty
	return base, nil
}

func evalPlanCritiqued(cur state.RunState, ev Event, base Decision) (Decision, error) {
	if ev.Decision {
		return humanGate(base, cur, state.PhasePlanCritique), nil
	}
	converged := ev.Verdict == "AGREE" && !ev.MissingEvidence && !ev.Actionable
	if converged {
		if !ev.TargetsValid {
			return Decision{}, semanticf("a dangling check obligation cannot be frozen at agreement")
		}
		checks := ev.ResultingChecks
		base.Next = state.PhaseImplementStep
		base.Route = RoutePromote
		base.Checks = &checks
		return base, nil
	}
	if ev.Actionable {
		limit := cur.EffectivePolicy.Budgets.PlanRounds
		switch cmpBudget(cur.Counters.PlanRevisions, limit) {
		case budgetOver:
			return Decision{}, ErrBudgetCorrupt
		case budgetUnder:
			checks := ev.ResultingChecks
			base.Next = state.PhasePlanRevise
			base.Route = RouteToRevise
			base.Checks = &checks
			base.Findings = &state.FindingObligations{Source: ev.Source, Keys: ev.Findings}
			return base, nil
		default: // at limit: park at a plan quality gate that resumes to PLAN_REVISE,
			// carrying the same actionable projection the revise edge would (v5 requires
			// PendingFindings + the updated checks at the suspended PLAN_REVISE), but
			// without charging the counter.
			checks := ev.ResultingChecks
			base.Next = state.PhaseAwaitGuidance
			base.Route = RouteGate
			base.Gate = &GateSpec{Kind: state.PauseQualityBudget, OriginPhase: state.PhasePlanCritique, ResumePhase: state.PhasePlanRevise, Budget: state.BudgetPlan}
			base.Checks = &checks
			base.Findings = &state.FindingObligations{Source: ev.Source, Keys: ev.Findings}
			return base, nil
		}
	}
	// Reviewer retry: withheld verdict with no actionable defect (missing evidence
	// only, or REVISE with only minor/nit). Fresh pair turn, no counter, no findings.
	base.Next = state.PhasePlanCritique
	base.Route = RouteRetryReview
	return base, nil
}

func evalPlanRevised(cur state.RunState, ev Event, base Decision) (Decision, error) {
	// Semantic preconditions (exact responses, base digest, materialization) are
	// enforced in Project; a valid revision here either gates or re-enters critique.
	if ev.Decision {
		return humanGate(base, cur, state.PhasePlanRevise), nil
	}
	plan := &state.PlanRef{Source: ev.Source, Digest: ev.Materialized, StepCount: ev.StepCount}
	base.Next = state.PhasePlanCritique
	base.Route = RouteReviseAccepted
	base.Plan = plan
	return base, nil
}

func evalStepImplemented(cur state.RunState, ev Event, base Decision) (Decision, error) {
	if ev.Decision {
		return humanGate(base, cur, state.PhaseImplementStep), nil
	}
	base.Next = state.PhaseCheckpoint
	base.Route = RouteToCheckpoint
	return base, nil
}

func evalStepCheckpointed(cur state.RunState, ev Event, base Decision) (Decision, error) {
	if ev.Decision {
		return humanGate(base, cur, state.PhaseCheckpoint), nil
	}
	if cur.StepIndex == nil || cur.AgreedPlan == nil {
		return Decision{}, semanticf("checkpoint without an agreed plan cursor")
	}
	cursor := *cur.StepIndex
	stepCount := cur.AgreedPlan.Plan.StepCount
	// Inadequate tests are lead work even if a direct event left Actionable false.
	needsFix := ev.Actionable || !ev.TestsAdequate
	converged := ev.Verdict == "AGREE" && !ev.MissingEvidence && !ev.Actionable && ev.TestsAdequate
	if converged {
		if cursor >= stepCount-1 {
			// The final step converged: the edge into TESTS is deferred.
			return Decision{}, ErrPhaseUnsupported
		}
		base.Next = state.PhaseImplementStep
		base.Route = RouteNextStep
		return base, nil
	}
	if needsFix {
		limit := cur.EffectivePolicy.Budgets.CheckpointRounds
		if cursor >= len(cur.Counters.StepFixes) {
			return Decision{}, semanticf("cursor past the step-fix vector")
		}
		switch cmpBudget(cur.Counters.StepFixes[cursor], limit) {
		case budgetOver:
			return Decision{}, ErrBudgetCorrupt
		case budgetUnder:
			base.Next = state.PhaseFix
			base.Route = RouteToFix
			return base, nil
		default: // at limit
			base.Next = state.PhaseAwaitGuidance
			base.Route = RouteGate
			base.Gate = &GateSpec{Kind: state.PauseQualityBudget, OriginPhase: state.PhaseCheckpoint, ResumePhase: state.PhaseFix, FixReturn: state.PhaseCheckpoint, Budget: state.BudgetCheckpoint}
			return base, nil
		}
	}
	// Reviewer retry: withheld verdict with no actionable defect. No counter.
	base.Next = state.PhaseCheckpoint
	base.Route = RouteRetryReview
	return base, nil
}

func evalFixImplemented(cur state.RunState, ev Event, base Decision) (Decision, error) {
	if ev.Decision {
		return humanGate(base, cur, state.PhaseFix), nil
	}
	// This subset only supports FIX returning to CHECKPOINT.
	if cur.FixReturn != state.PhaseCheckpoint {
		return Decision{}, ErrPhaseUnsupported
	}
	base.Next = state.PhaseCheckpoint
	base.Route = RouteToCheckpoint
	return base, nil
}

type budgetCmp int

const (
	budgetUnder budgetCmp = iota
	budgetAt
	budgetOver
)

func cmpBudget(used, limit int) budgetCmp {
	switch {
	case used > limit:
		return budgetOver
	case used < limit:
		return budgetUnder
	default:
		return budgetAt
	}
}

// --- apply: realize a decision into an already-cloned next state ---

// Apply mutates next to realize dec, consuming the prepared ids and the independent
// submitted event. It fully validates the decision's binding, ids, route/edge, and
// payload BEFORE any write, so a rejected Apply never panics or partially mutates.
// next.Revision must already be the resulting generation gen. Apply never appends
// the accepted turn — transport records the receipt after Apply. Decision slices and
// pointers are copied into state, never aliased.
func Apply(dec Decision, submitted state.EventRef, ids Ids, gen uint64, next *state.RunState) error {
	if err := validateApply(dec, submitted, ids, gen, next); err != nil {
		return err
	}

	next.Assignment = nil // consume the submitted turn
	next.Phase = dec.Next

	switch dec.Route {
	case RouteGate:
		if dec.Checks != nil { // plan quality gate carries the actionable projection
			next.CandidateChecks = copyCheckSet(dec.Checks)
		}
		if dec.Findings != nil {
			next.PendingFindings = copyFindings(dec.Findings)
		}
		applyGate(dec.Gate, submitted, ids.GateID, gen, next)
	case RouteDraftAccepted:
		next.CandidatePlan = copyPlanRef(dec.Plan)
		next.CandidateChecks = copyCheckSet(dec.Checks)
		issue(next, ids, gen)
	case RoutePromote:
		next.AgreedPlan = &state.PlanAgreement{
			Plan:           *next.CandidatePlan,
			Critique:       submitted,
			Checks:         *copyCheckSet(dec.Checks),
			AgreedRevision: gen,
		}
		next.CandidatePlan = nil
		next.CandidateChecks = nil
		next.PendingFindings = nil
		next.Counters.StepFixes = make([]int, next.AgreedPlan.Plan.StepCount)
		idx := 0
		next.StepIndex = &idx
		issue(next, ids, gen)
	case RouteToRevise:
		next.CandidateChecks = copyCheckSet(dec.Checks)
		next.PendingFindings = copyFindings(dec.Findings)
		next.Counters.PlanRevisions++
		issue(next, ids, gen)
	case RouteRetryReview:
		issue(next, ids, gen)
	case RouteReviseAccepted:
		next.CandidatePlan = copyPlanRef(dec.Plan)
		next.PendingFindings = nil // preserve checks, clear findings
		issue(next, ids, gen)
	case RouteToCheckpoint:
		next.FixReturn = "" // clears when leaving FIX; a no-op from IMPLEMENT
		issue(next, ids, gen)
	case RouteNextStep:
		*next.StepIndex++
		issue(next, ids, gen)
	case RouteToFix:
		next.Counters.StepFixes[*next.StepIndex]++
		next.FixReturn = state.PhaseCheckpoint
		issue(next, ids, gen)
	}
	return nil
}

// validateApply proves the decision is well-formed and bound to this exact turn,
// reading only — it performs no mutation, so a failure leaves next untouched.
func validateApply(dec Decision, submitted state.EventRef, ids Ids, gen uint64, next *state.RunState) error {
	// Source binding — and the independent submitted event must be well-formed on its
	// own (a canonical turn id and a sha256 digest), not merely equal to dec.Source.
	if submitted != dec.Source {
		return fmt.Errorf("%w: submitted event does not match the decision source", ErrBadDecision)
	}
	if !state.IsRunID(submitted.TurnID) || !state.IsHex64(submitted.Digest) {
		return fmt.Errorf("%w: the submitted event is not well-formed", ErrBadDecision)
	}
	if next.Revision != gen {
		return fmt.Errorf("%w: next revision %d != resulting generation %d", ErrBadDecision, next.Revision, gen)
	}
	if next.Phase != dec.FromPhase {
		return fmt.Errorf("%w: from-phase %s != current phase %s", ErrBadDecision, dec.FromPhase, next.Phase)
	}
	// The pre-state must be a live run with the bound assignment outstanding.
	if next.Lifecycle != state.LifecycleRunning || next.Pause != nil || next.Gate != nil || next.Recovery != nil {
		return fmt.Errorf("%w: the run is not live", ErrBadDecision)
	}
	if next.Assignment == nil || next.Assignment.ID != submitted.TurnID {
		return fmt.Errorf("%w: the submitted turn is not the outstanding assignment", ErrBadDecision)
	}
	if next.Assignment.IssuedRevision != dec.ExpectedStateRevision {
		return fmt.Errorf("%w: the assignment was not issued at the expected revision %d", ErrBadDecision, dec.ExpectedStateRevision)
	}
	// Route/edge table.
	if !routeAllowed(dec.Route, dec.FromPhase, dec.Next) {
		return fmt.Errorf("%w: route %d is not legal for %s->%s", ErrBadDecision, dec.Route, dec.FromPhase, dec.Next)
	}
	// Gate is the sole paused discriminant.
	if (dec.Route == RouteGate) != (dec.Gate != nil) {
		return fmt.Errorf("%w: gate presence must match the gate route", ErrBadDecision)
	}
	// Id exactness: canonical (state's persisted grammar), exactly the id this edge
	// issues, and a new assignment that is neither the consumed nor an accepted turn.
	if dec.Gate != nil {
		if !state.IsRunID(ids.GateID) || ids.AssignmentTurnID != "" {
			return fmt.Errorf("%w: a gate requires exactly a canonical gate id", ErrBadDecision)
		}
	} else {
		if !state.IsRunID(ids.AssignmentTurnID) || ids.GateID != "" {
			return fmt.Errorf("%w: a running edge requires exactly a canonical assignment id", ErrBadDecision)
		}
		if ids.AssignmentTurnID == submitted.TurnID {
			return fmt.Errorf("%w: the next assignment reuses the consumed turn", ErrBadDecision)
		}
		if _, ok := next.AcceptedTurns[ids.AssignmentTurnID]; ok {
			return fmt.Errorf("%w: the next assignment reuses an accepted turn", ErrBadDecision)
		}
	}
	return validateRoute(dec, submitted, next)
}

func badDecision(msg string) error { return fmt.Errorf("%w: %s", ErrBadDecision, msg) }

func anyPayload(dec Decision) bool {
	return dec.Plan != nil || dec.Checks != nil || dec.Findings != nil
}

// validateRoute enforces the exact route→edge table, the running-edge budget
// preconditions (so a forged over/at-limit edge cannot bypass the sole budget
// table), the gate subset table, and the local well-formedness of every payload
// Apply persists — all before the first write.
func validateRoute(dec Decision, submitted state.EventRef, next *state.RunState) error {
	b := next.EffectivePolicy.Budgets
	switch dec.Route {
	case RouteGate:
		return validateGate(dec, submitted, next)
	case RouteDraftAccepted:
		if dec.Plan == nil || dec.Checks == nil || dec.Findings != nil {
			return badDecision("draft-accept requires a plan and checks and no findings")
		}
		if err := checkPlanRef(dec.Plan, submitted); err != nil {
			return err
		}
		return checkCheckSet(dec.Checks)
	case RoutePromote:
		if dec.Checks == nil || dec.Plan != nil || dec.Findings != nil {
			return badDecision("promote requires frozen checks and no plan/findings")
		}
		if next.CandidatePlan == nil {
			return badDecision("promote requires a candidate plan to freeze")
		}
		return checkCheckSet(dec.Checks)
	case RouteToRevise:
		if dec.Checks == nil || dec.Findings == nil || dec.Plan != nil {
			return badDecision("revise requires checks and findings and no plan")
		}
		if err := runningBudget(next.Counters.PlanRevisions, b.PlanRounds); err != nil {
			return err
		}
		if err := checkCheckSet(dec.Checks); err != nil {
			return err
		}
		return checkFindings(dec.Findings, submitted)
	case RouteReviseAccepted:
		if dec.Plan == nil || dec.Checks != nil || dec.Findings != nil {
			return badDecision("revise-accept requires a plan and no checks/findings")
		}
		return checkPlanRef(dec.Plan, submitted)
	case RouteRetryReview:
		if anyPayload(dec) {
			return badDecision("reviewer-retry carries no payload")
		}
	case RouteToCheckpoint:
		if anyPayload(dec) {
			return badDecision("to-checkpoint carries no payload")
		}
		if dec.FromPhase == state.PhaseFix && next.FixReturn != state.PhaseCheckpoint {
			return badDecision("only a FIX returning to CHECKPOINT is supported")
		}
	case RouteNextStep:
		if anyPayload(dec) {
			return badDecision("next-step carries no payload")
		}
		if next.StepIndex == nil || next.AgreedPlan == nil || *next.StepIndex < 0 || *next.StepIndex >= next.AgreedPlan.Plan.StepCount-1 {
			return badDecision("next-step requires a cursor before the final step")
		}
	case RouteToFix:
		if anyPayload(dec) {
			return badDecision("to-fix carries no payload")
		}
		if next.StepIndex == nil || *next.StepIndex < 0 || *next.StepIndex >= len(next.Counters.StepFixes) {
			return badDecision("to-fix requires a valid step cursor")
		}
		return runningBudget(next.Counters.StepFixes[*next.StepIndex], b.CheckpointRounds)
	default:
		return badDecision("unknown route")
	}
	return nil
}

// validateGate enforces the exact two-kind gate subset table.
func validateGate(dec Decision, submitted state.EventRef, next *state.RunState) error {
	if dec.Plan != nil {
		return badDecision("a gate carries no plan")
	}
	g := dec.Gate
	// Every gate — human or quality — pauses out of the submit's own phase.
	if g.OriginPhase != dec.FromPhase {
		return badDecision("a gate origin must be the from-phase")
	}
	b := next.EffectivePolicy.Budgets
	switch g.Kind {
	case state.PauseHumanDecision:
		if g.Budget != "" || dec.Checks != nil || dec.Findings != nil {
			return badDecision("a human gate carries no budget/checks/findings")
		}
		if g.OriginPhase != dec.FromPhase || g.ResumePhase != dec.FromPhase || !isSubmitPhase(g.OriginPhase) {
			return badDecision("a human gate resumes to its submit origin")
		}
		if g.OriginPhase == state.PhaseFix {
			if g.FixReturn != state.PhaseCheckpoint || next.FixReturn != state.PhaseCheckpoint {
				return badDecision("a FIX human gate preserves the CHECKPOINT return")
			}
		} else if g.FixReturn != "" {
			return badDecision("only a FIX human gate carries a fix return")
		}
	case state.PauseQualityBudget:
		switch g.Budget {
		case state.BudgetPlan:
			if g.OriginPhase != state.PhasePlanCritique || g.ResumePhase != state.PhasePlanRevise || g.FixReturn != "" {
				return badDecision("a plan quality gate is PLAN_CRITIQUE->PLAN_REVISE")
			}
			if dec.Checks == nil || dec.Findings == nil {
				return badDecision("a plan quality gate carries checks and findings")
			}
			if err := checkCheckSet(dec.Checks); err != nil {
				return err
			}
			if err := checkFindings(dec.Findings, submitted); err != nil {
				return err
			}
			return gateBudget(next.Counters.PlanRevisions, b.PlanRounds)
		case state.BudgetCheckpoint:
			if g.OriginPhase != state.PhaseCheckpoint || g.ResumePhase != state.PhaseFix || g.FixReturn != state.PhaseCheckpoint {
				return badDecision("a checkpoint quality gate is CHECKPOINT->FIX")
			}
			if dec.Checks != nil || dec.Findings != nil {
				return badDecision("a checkpoint quality gate carries no checks/findings")
			}
			if next.StepIndex == nil || *next.StepIndex < 0 || *next.StepIndex >= len(next.Counters.StepFixes) {
				return badDecision("a checkpoint quality gate needs a valid cursor")
			}
			return gateBudget(next.Counters.StepFixes[*next.StepIndex], b.CheckpointRounds)
		default:
			return badDecision("unknown budget kind")
		}
	default:
		return badDecision("unknown pause kind")
	}
	return nil
}

// runningBudget requires a running fix/revise edge to be strictly under the frozen
// limit; used>limit is a corrupt counter, used==limit is the wrong route.
func runningBudget(used, limit int) error {
	switch cmpBudget(used, limit) {
	case budgetUnder:
		return nil
	case budgetOver:
		return ErrBudgetCorrupt
	default:
		return badDecision("a running edge chosen at the budget limit")
	}
}

// gateBudget requires a quality gate to sit exactly at the frozen limit.
func gateBudget(used, limit int) error {
	switch cmpBudget(used, limit) {
	case budgetAt:
		return nil
	case budgetOver:
		return ErrBudgetCorrupt
	default:
		return badDecision("a quality gate raised below the budget limit")
	}
}

func routeAllowed(r Route, from, next state.Phase) bool {
	switch r {
	case RouteGate:
		return next == state.PhaseAwaitGuidance
	case RouteDraftAccepted:
		return from == state.PhasePlanDraft && next == state.PhasePlanCritique
	case RoutePromote:
		return from == state.PhasePlanCritique && next == state.PhaseImplementStep
	case RouteToRevise:
		return from == state.PhasePlanCritique && next == state.PhasePlanRevise
	case RouteRetryReview:
		return (from == state.PhasePlanCritique && next == state.PhasePlanCritique) ||
			(from == state.PhaseCheckpoint && next == state.PhaseCheckpoint)
	case RouteReviseAccepted:
		return from == state.PhasePlanRevise && next == state.PhasePlanCritique
	case RouteToCheckpoint:
		return (from == state.PhaseImplementStep || from == state.PhaseFix) && next == state.PhaseCheckpoint
	case RouteNextStep:
		return from == state.PhaseCheckpoint && next == state.PhaseImplementStep
	case RouteToFix:
		return from == state.PhaseCheckpoint && next == state.PhaseFix
	}
	return false
}

// isSubmitPhase reports whether a phase may raise a human-decision gate (every
// agent submit phase; TESTS is coordinator-authored and cannot).
func isSubmitPhase(p state.Phase) bool {
	switch p {
	case state.PhasePlanDraft, state.PhasePlanCritique, state.PhasePlanRevise,
		state.PhaseImplementStep, state.PhaseCheckpoint, state.PhaseFix:
		return true
	}
	return false
}

func checkPlanRef(p *state.PlanRef, submitted state.EventRef) error {
	if p.Source != submitted {
		return badDecision("plan source is not the submitted event")
	}
	if !state.IsHex64(p.Digest) {
		return badDecision("plan digest is not a 64-char lower-hex sha256")
	}
	if p.StepCount < protocol.MinPlanSteps || p.StepCount > protocol.MaxPlanSteps {
		return badDecision("plan step count is out of range")
	}
	return nil
}

func checkCheckSet(c *state.CheckSetRef) error {
	if c.Keys == nil {
		return badDecision("check keys must be a non-nil array")
	}
	if err := protocol.ValidateKeySet("checks", c.Keys); err != nil {
		return fmt.Errorf("%w: %v", ErrBadDecision, err)
	}
	if !sortedStrings(c.Keys) {
		return badDecision("check keys must be strictly sorted")
	}
	if !state.IsHex64(c.Digest) {
		return badDecision("check digest is not a 64-char lower-hex sha256")
	}
	// The same canonical empty law state pins: empty keys iff the empty-array digest.
	if (len(c.Keys) == 0) != (c.Digest == emptyChecksDigest) {
		return badDecision("check keys are empty iff the digest is the canonical empty-array digest")
	}
	return nil
}

// emptyChecksDigest is the canonical digest of the empty materialized check array,
// used to enforce the empty-set biconditional a Decision's check payload must obey.
var emptyChecksDigest = mustEmptyChecksDigest()

func mustEmptyChecksDigest() string {
	d, err := canonjson.DigestValue([]any{})
	if err != nil {
		panic("engine: empty check digest: " + err.Error())
	}
	return d
}

func checkFindings(f *state.FindingObligations, submitted state.EventRef) error {
	if len(f.Keys) == 0 {
		return badDecision("findings must be non-empty")
	}
	if err := protocol.ValidateKeySet("findings", f.Keys); err != nil {
		return fmt.Errorf("%w: %v", ErrBadDecision, err)
	}
	if !sortedStrings(f.Keys) {
		return badDecision("finding keys must be strictly sorted")
	}
	if f.Source != submitted {
		return badDecision("findings source is not the submitted event")
	}
	return nil
}

func sortedStrings(ss []string) bool {
	for i := 1; i < len(ss); i++ {
		if ss[i-1] >= ss[i] {
			return false
		}
	}
	return true
}

func copyPlanRef(p *state.PlanRef) *state.PlanRef { c := *p; return &c }

func copyCheckSet(c *state.CheckSetRef) *state.CheckSetRef {
	out := *c
	out.Keys = append([]string{}, c.Keys...)
	return &out
}

func copyFindings(f *state.FindingObligations) *state.FindingObligations {
	out := *f
	out.Keys = append([]string{}, f.Keys...)
	return &out
}

func issue(next *state.RunState, ids Ids, gen uint64) {
	next.Assignment = &state.Ref{ID: ids.AssignmentTurnID, IssuedRevision: gen}
}

func applyGate(g *GateSpec, source state.EventRef, gateID string, gen uint64, next *state.RunState) {
	pc := &state.PauseContext{
		Kind:        g.Kind,
		OriginPhase: g.OriginPhase,
		ResumePhase: g.ResumePhase,
		FixReturn:   g.FixReturn,
		Source:      source,
	}
	if g.Kind == state.PauseQualityBudget {
		pc.Budget = &state.BudgetPause{Kind: g.Budget}
	}
	// A human gate raised from FIX moves the return target into the pause.
	next.FixReturn = ""
	next.Gate = &state.Ref{ID: gateID, IssuedRevision: gen}
	next.Pause = pc
	next.Lifecycle = state.LifecyclePaused
}

// --- pure materialization helpers ---

// materializeChecks sorts checks by key and produces the CheckSetRef state stores:
// the sorted key list plus the digest of the canonical materialized check array,
// which carries every established field (key, description, evidence, target_step;
// action is an operation and is not retained). The empty array yields the canonical
// empty digest state pins. The resulting key set is grammar/count validated.
func materializeChecks(checks []MaterializedCheck) (state.CheckSetRef, error) {
	sorted := append([]MaterializedCheck(nil), checks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	keys := make([]string, 0, len(sorted))
	arr := make([]any, 0, len(sorted))
	for _, c := range sorted {
		keys = append(keys, c.Key)
		var target any
		if c.TargetStep != nil {
			target = *c.TargetStep
		}
		arr = append(arr, map[string]any{"key": c.Key, "description": c.Description, "evidence": c.Evidence, "target_step": target})
	}
	if err := protocol.ValidateKeySet("checks", keys); err != nil {
		return state.CheckSetRef{}, fmt.Errorf("%w: %v", ErrSemantic, err)
	}
	dig, err := canonjson.DigestValue(arr)
	if err != nil {
		return state.CheckSetRef{}, err
	}
	return state.CheckSetRef{Keys: keys, Digest: dig}, nil
}

// planDocDigest is the materialized plan-document digest: the canonical digest of
// the plan's content sections, distinct from the submit artifact digest.
func planDocDigest(markdown string, steps []PlanStep, risks, openQuestions []string) (string, error) {
	stepVals := make([]any, 0, len(steps))
	for _, s := range steps {
		stepVals = append(stepVals, map[string]any{
			"title":       s.Title,
			"description": s.Description,
			"files":       strsToVals(s.Files),
			"tests":       strsToVals(s.Tests),
		})
	}
	doc := map[string]any{
		"plan_markdown":  markdown,
		"steps":          stepVals,
		"risks":          strsToVals(risks),
		"open_questions": strsToVals(openQuestions),
	}
	return canonjson.DigestValue(doc)
}

func strsToVals(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

func stepTitles(steps []PlanStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Title)
	}
	return out
}
