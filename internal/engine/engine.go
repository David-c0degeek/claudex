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
type MaterializedCheck struct {
	Key        string
	TargetStep *string // nil = cross-cutting
}

// ProjectionFacts are the materialized artifacts the loader read and revalidated.
// Project rechecks their digests/keys against cur before deriving anything.
type ProjectionFacts struct {
	CandidatePlan   CanonicalPlan       // current plan (revise/critique need it)
	CandidateChecks []MaterializedCheck // current materialized check set
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
		default: // at limit
			base.Next = state.PhaseAwaitGuidance
			base.Route = RouteGate
			base.Gate = &GateSpec{Kind: state.PauseQualityBudget, OriginPhase: state.PhasePlanCritique, ResumePhase: state.PhasePlanRevise, Budget: state.BudgetPlan}
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
	converged := ev.Verdict == "AGREE" && !ev.MissingEvidence && !ev.Actionable
	if converged {
		if cursor >= stepCount-1 {
			// The final step converged: the edge into TESTS is deferred.
			return Decision{}, ErrPhaseUnsupported
		}
		base.Next = state.PhaseImplementStep
		base.Route = RouteNextStep
		return base, nil
	}
	if ev.Actionable {
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
// submitted event. It proves the decision is bound to this exact turn before any
// mutation. next.Revision must already be the resulting generation gen. It never
// appends the accepted turn — transport records the receipt after Apply.
func Apply(dec Decision, submitted state.EventRef, ids Ids, gen uint64, next *state.RunState) error {
	if submitted != dec.Source {
		return fmt.Errorf("engine: submitted event does not match the decision source")
	}
	if next.Revision != gen {
		return fmt.Errorf("engine: next revision %d does not match the resulting generation %d", next.Revision, gen)
	}
	if next.Phase != dec.FromPhase {
		return fmt.Errorf("engine: decision from-phase %s does not match the current phase %s", dec.FromPhase, next.Phase)
	}
	if next.Assignment == nil || next.Assignment.ID != submitted.TurnID {
		return fmt.Errorf("engine: the submitted turn is not the outstanding assignment")
	}
	if next.Assignment.IssuedRevision != dec.ExpectedStateRevision {
		return fmt.Errorf("engine: the consumed assignment was not issued at the expected revision %d", dec.ExpectedStateRevision)
	}
	// Id exactness: a gate issues only a gate id; every running edge issues only an
	// assignment id.
	if dec.Gate != nil {
		if ids.GateID == "" || ids.AssignmentTurnID != "" {
			return fmt.Errorf("engine: a gate decision requires exactly a gate id")
		}
	} else if ids.AssignmentTurnID == "" || ids.GateID != "" {
		return fmt.Errorf("engine: a running decision requires exactly an assignment id")
	}

	next.Assignment = nil // consume the submitted turn
	next.Phase = dec.Next

	switch dec.Route {
	case RouteGate:
		applyGate(dec.Gate, submitted, ids.GateID, gen, next)
	case RouteDraftAccepted:
		next.CandidatePlan = dec.Plan
		next.CandidateChecks = dec.Checks
		issue(next, ids, gen)
	case RoutePromote:
		next.AgreedPlan = &state.PlanAgreement{
			Plan:           *next.CandidatePlan,
			Critique:       submitted,
			Checks:         *dec.Checks,
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
		next.CandidateChecks = dec.Checks
		next.PendingFindings = dec.Findings
		next.Counters.PlanRevisions++
		issue(next, ids, gen)
	case RouteRetryReview:
		issue(next, ids, gen)
	case RouteReviseAccepted:
		next.CandidatePlan = dec.Plan
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
	default:
		return fmt.Errorf("engine: unknown route %d", dec.Route)
	}
	return nil
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
// the sorted key list plus the digest of the canonical materialized check array
// (the empty array yields the canonical empty digest state pins).
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
		arr = append(arr, map[string]any{"key": c.Key, "target_step": target})
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
