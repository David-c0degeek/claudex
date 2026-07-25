// Package engine is the pure phase engine: it owns the sole legal phase-edge and
// convergence table for the pairing loop. It is a pure function of the current run
// state plus a projected event; it performs no I/O, mints no identities, and reads
// no files. The outer orchestration/test layer loads and revalidates the immutable
// artifacts, projects a typed Event, calls Evaluate to obtain a Decision, and calls
// Apply to realize that Decision into an already-cloned next run state. State's own
// transition validation is the backstop.
//
// Scope: this build covers the full pairing loop — PLAN_DRAFT/PLAN_CRITIQUE/
// PLAN_REVISE, IMPLEMENT_STEP, CHECKPOINT, FIX (returning to CHECKPOINT/TESTS/VERIFY),
// the coordinator-authored ownerless TESTS phase, the ownerless VERIFY phase (its
// fresh-generation threshold and the verifier's verification), and DONE, plus every
// plan/checkpoint/test/verify quality gate and human-decision gate. The runtime
// wiring of the ownerless edges (the TESTS runner, the replacement that issues the
// verifier turn) lives in the outer layers, not here.
package engine

import (
	"errors"
	"fmt"
	"sort"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

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
	state.PhaseTests:         EvTestsOutcome,
	state.PhaseVerify:        EvVerified,
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
	Task            TaskFacts           // the frozen task contract (VERIFY needs it)
}

// TaskFacts carries the EXACT immutable task-snapshot bytes the loader read. Project
// recomputes their digest against cur.TaskSnapshot.Digest and parses the contract
// itself, so a caller cannot assert a digest with a forged criteria list.
type TaskFacts struct {
	SnapshotBytes string // the frozen task-snapshot bytes (config.Hash must match the run)
}

// RuntimeFacts are value-only NON-artifact facts the impure caller reads under the run
// guard and supplies to Evaluate: currently the locked current pair generation, used
// to compute the ownerless-VERIFY threshold. It is nonzero-but-irrelevant on edges
// that do not enter VERIFY.
type RuntimeFacts struct {
	CurrentPairGeneration uint64
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
	EvTestsOutcome // coordinator-authored TESTS pass/fail (ownerless; not an agent turn)
	EvVerified     // the verifier's verification artifact
)

// Event is the value-only projection of one turn's outcome. Every event but
// EvTestsOutcome is an accepted AGENT submit; EvTestsOutcome is coordinator-authored
// (its Source is an ownerless empty-turn evidence digest, never an accepted turn).
// Evaluate is a pure function of (cur, Event, RuntimeFacts). For an agent submit,
// Source is the SUBMITTED event's digest+turn — it becomes an accepted turn only when
// transport appends the receipt after Apply.
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

	// Terminal-phase-derived (tests outcome / verification):
	Pass bool // TESTS outcome pass, or a converged VERIFY pass

	// Critique payload (materialized for every valid critique; used only when routed):
	ResultingChecks state.CheckSetRef // check ops applied to the current set
	TargetsValid    bool              // every non-null target matches a unique step title
	Findings        []string          // actionable finding keys (sorted)
}

// --- decision + apply contract ---

// Route is the specific state mutation an accepted agent submit or coordinator-
// authored TESTS outcome drives.
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
	RouteToTests                     // enter TESTS ownerless (final CHECKPOINT converged, or FIX return)
	RouteToVerify                    // enter VERIFY ownerless with a fresh RequiredGeneration (TESTS pass, or FIX return)
	RouteTestsFix                    // TESTS fail -> FIX, ++TestFixes, FixReturn=TESTS
	RouteVerifyFix                   // VERIFY fail -> FIX, ++VerifyFixes, FixReturn=VERIFY, clear Verify
	RouteToDone                      // VERIFY pass -> DONE (terminal, ownerless)
)

// GateSpec describes a gate to open.
type GateSpec struct {
	Kind        state.PauseKind
	OriginPhase state.Phase
	ResumePhase state.Phase
	FixReturn   state.Phase              // set iff ResumePhase==FIX
	Budget      state.BudgetKind         // set iff Kind==quality_budget
	Verify      *state.VerifyRequirement // set iff a human gate resumes to VERIFY (transferred in)
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
	Verify   *state.VerifyRequirement  // RouteToVerify (the fresh ownerless-VERIFY threshold)
}

// Ids are the prepared identities the adapter minted before the CAS loop. Apply
// requires exactly the id the Decision issues.
//
// Evidence is the review-evidence packet the adapter published for the assignment
// it is about to issue, frozen before the CAS. It is required for every read-only
// assignment and forbidden for an IMPLEMENT_STEP/FIX one; the state validator
// enforces that invariant, and Apply only binds what the adapter proved. Its
// IssuedRevision is filled in here, at the CAS, because the resulting revision is
// the one fact the off-lock producer cannot know.
type Ids struct {
	AssignmentTurnID string
	GateID           string
	Evidence         *EvidencePacket
}

// EvidencePacket is the published packet's locator, minus the revision binding.
// It is the value the adapter freezes in its serializable plan or intent, so a
// recovered transaction re-binds exactly the packet the original authorization
// published rather than re-deriving one.
type EvidencePacket struct {
	ManifestRelPath string
	RootDigest      string
}

// --- evaluate: the sole phase-edge/convergence table ---

// Evaluate returns the Decision for the current state and a projected event. rt
// supplies the value-only runtime facts (the locked pair generation) an edge that
// enters ownerless VERIFY needs. Pure; mints nothing.
func Evaluate(cur state.RunState, ev Event, rt RuntimeFacts) (Decision, error) {
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
		return evalFixImplemented(cur, ev, rt, base)
	case EvTestsOutcome:
		return evalTestsOutcome(cur, ev, rt, base)
	case EvVerified:
		return evalVerified(cur, ev, base)
	}
	return Decision{}, fmt.Errorf("engine: unknown event kind %d", ev.Kind)
}

func humanGate(base Decision, cur state.RunState, origin state.Phase) Decision {
	g := &GateSpec{Kind: state.PauseHumanDecision, OriginPhase: origin, ResumePhase: origin}
	if origin == state.PhaseFix {
		g.FixReturn = cur.FixReturn
	}
	// A human gate raised from a running VERIFY transfers the verify requirement into
	// the pause (state's context-transfer invariant requires it); deep-copy so the
	// decision never aliases the input state's pointer.
	if origin == state.PhaseVerify && cur.Verify != nil {
		v := *cur.Verify
		g.Verify = &v
	}
	base.Next = state.PhaseAwaitGuidance
	base.Route = RouteGate
	base.Gate = g
	return base
}

// verifyRequirement returns the ownerless-VERIFY threshold: one generation newer than
// the locked current pair generation. A zero pair generation (a missing Registry fact)
// or a max value (which would overflow the +1) is rejected before a Decision forms.
func verifyRequirement(rt RuntimeFacts) (*state.VerifyRequirement, error) {
	if rt.CurrentPairGeneration == 0 {
		return nil, semanticf("entering VERIFY requires a positive current pair generation")
	}
	if rt.CurrentPairGeneration == ^uint64(0) {
		return nil, semanticf("current pair generation overflows the verify threshold")
	}
	return &state.VerifyRequirement{RequiredGeneration: rt.CurrentPairGeneration + 1}, nil
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
			// The final step converged: enter TESTS ownerless (coordinator-authored).
			base.Next = state.PhaseTests
			base.Route = RouteToTests
			return base, nil
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

func evalFixImplemented(cur state.RunState, ev Event, rt RuntimeFacts, base Decision) (Decision, error) {
	if ev.Decision {
		return humanGate(base, cur, state.PhaseFix), nil
	}
	// A FIX returns to the phase whose review sent it here.
	switch cur.FixReturn {
	case state.PhaseCheckpoint:
		base.Next = state.PhaseCheckpoint
		base.Route = RouteToCheckpoint
		return base, nil
	case state.PhaseTests:
		base.Next = state.PhaseTests
		base.Route = RouteToTests
		return base, nil
	case state.PhaseVerify:
		// Re-enter ownerless VERIFY with a FRESH threshold (newer than the verifier
		// that failed): another qualifying replacement must issue the verifier turn.
		req, err := verifyRequirement(rt)
		if err != nil {
			return Decision{}, err
		}
		base.Next = state.PhaseVerify
		base.Route = RouteToVerify
		base.Verify = req
		return base, nil
	}
	return Decision{}, semanticf("unexpected fix return target %q", cur.FixReturn)
}

// evalTestsOutcome routes the coordinator-authored TESTS pass/fail. TESTS is
// ownerless and cannot raise a human decision.
func evalTestsOutcome(cur state.RunState, ev Event, rt RuntimeFacts, base Decision) (Decision, error) {
	if ev.Pass {
		// Enter ownerless VERIFY with the fresh-session threshold.
		req, err := verifyRequirement(rt)
		if err != nil {
			return Decision{}, err
		}
		base.Next = state.PhaseVerify
		base.Route = RouteToVerify
		base.Verify = req
		return base, nil
	}
	// Fail: route to the lead's FIX (returning to TESTS) unless the test budget is spent.
	limit := cur.EffectivePolicy.Budgets.TestRounds
	switch cmpBudget(cur.Counters.TestFixes, limit) {
	case budgetOver:
		return Decision{}, ErrBudgetCorrupt
	case budgetUnder:
		base.Next = state.PhaseFix
		base.Route = RouteTestsFix
		return base, nil
	default: // at limit: test-quality gate resuming to FIX.
		base.Next = state.PhaseAwaitGuidance
		base.Route = RouteGate
		base.Gate = &GateSpec{Kind: state.PauseQualityBudget, OriginPhase: state.PhaseTests, ResumePhase: state.PhaseFix, FixReturn: state.PhaseTests, Budget: state.BudgetTest}
		return base, nil
	}
}

// evalVerified routes the verifier's verification (Project already enforced exact
// criteria coverage and the pass/blocker consistency). Human decision has priority.
func evalVerified(cur state.RunState, ev Event, base Decision) (Decision, error) {
	if ev.Decision {
		return humanGate(base, cur, state.PhaseVerify), nil
	}
	if ev.Pass {
		base.Next = state.PhaseDone
		base.Route = RouteToDone
		return base, nil
	}
	// Fail: route to the lead's FIX (returning to VERIFY) unless the verify budget is spent.
	limit := cur.EffectivePolicy.Budgets.VerifyRounds
	switch cmpBudget(cur.Counters.VerifyFixes, limit) {
	case budgetOver:
		return Decision{}, ErrBudgetCorrupt
	case budgetUnder:
		base.Next = state.PhaseFix
		base.Route = RouteVerifyFix
		return base, nil
	default: // at limit: verify-quality gate resuming to FIX.
		base.Next = state.PhaseAwaitGuidance
		base.Route = RouteGate
		base.Gate = &GateSpec{Kind: state.PauseQualityBudget, OriginPhase: state.PhaseVerify, ResumePhase: state.PhaseFix, FixReturn: state.PhaseVerify, Budget: state.BudgetVerify}
		return base, nil
	}
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

	// Consume the submitted turn. The evidence binding is cleared with it: it authorized exactly that
	// assignment, so it must not outlive it — every route below either reissues both through issue()
	// or leaves the state ownerless with neither.
	next.Assignment = nil
	next.Evidence = nil
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
		if err := issue(next, ids, gen); err != nil {
			return err
		}
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
		if err := issue(next, ids, gen); err != nil {
			return err
		}
	case RouteToRevise:
		next.CandidateChecks = copyCheckSet(dec.Checks)
		next.PendingFindings = copyFindings(dec.Findings)
		next.Counters.PlanRevisions++
		if err := issue(next, ids, gen); err != nil {
			return err
		}
	case RouteRetryReview:
		if err := issue(next, ids, gen); err != nil {
			return err
		}
	case RouteReviseAccepted:
		next.CandidatePlan = copyPlanRef(dec.Plan)
		next.PendingFindings = nil // preserve checks, clear findings
		if err := issue(next, ids, gen); err != nil {
			return err
		}
	case RouteToCheckpoint:
		next.FixReturn = "" // clears when leaving FIX; a no-op from IMPLEMENT
		if err := issue(next, ids, gen); err != nil {
			return err
		}
	case RouteNextStep:
		*next.StepIndex++
		if err := issue(next, ids, gen); err != nil {
			return err
		}
	case RouteToFix:
		next.Counters.StepFixes[*next.StepIndex]++
		next.FixReturn = state.PhaseCheckpoint
		if err := issue(next, ids, gen); err != nil {
			return err
		}
	case RouteToTests:
		// Ownerless: from the final CHECKPOINT (cursor at last step) or a FIX return.
		next.FixReturn = ""
		*next.StepIndex = next.AgreedPlan.Plan.StepCount // the cursor sits at the plan end
	case RouteToVerify:
		next.FixReturn = ""
		*next.StepIndex = next.AgreedPlan.Plan.StepCount
		v := *dec.Verify
		next.Verify = &v
	case RouteTestsFix:
		next.Counters.TestFixes++
		next.FixReturn = state.PhaseTests
		if err := issue(next, ids, gen); err != nil {
			return err
		}
	case RouteVerifyFix:
		next.Counters.VerifyFixes++
		next.FixReturn = state.PhaseVerify
		next.Verify = nil // leaving VERIFY for FIX clears the requirement
		if err := issue(next, ids, gen); err != nil {
			return err
		}
	case RouteToDone:
		next.Verify = nil
		next.Lifecycle = state.LifecycleCompleted // terminal: no assignment, no gate
	}
	return nil
}

// validateApply proves the decision is well-formed and bound to this exact turn,
// reading only — it performs no mutation, so a failure leaves next untouched.
func validateApply(dec Decision, submitted state.EventRef, ids Ids, gen uint64, next *state.RunState) error {
	if submitted != dec.Source {
		return fmt.Errorf("%w: submitted event does not match the decision source", ErrBadDecision)
	}
	// Source well-formedness by authoring mode. A coordinator-authored TESTS outcome
	// carries an ownerless evidence digest (empty turn id); every other edge is an
	// agent submit whose source is a canonical turn.
	ownerless := isOwnerlessAuthoringPhase(dec.FromPhase)
	if ownerless {
		if submitted.TurnID != "" || !state.IsHex64(submitted.Digest) {
			return fmt.Errorf("%w: a TESTS outcome source is an empty-turn evidence digest", ErrBadDecision)
		}
	} else if !state.IsRunID(submitted.TurnID) || !state.IsHex64(submitted.Digest) {
		return fmt.Errorf("%w: the submitted event is not well-formed", ErrBadDecision)
	}
	if next.Revision != gen {
		return fmt.Errorf("%w: next revision %d != resulting generation %d", ErrBadDecision, next.Revision, gen)
	}
	if next.Phase != dec.FromPhase {
		return fmt.Errorf("%w: from-phase %s != current phase %s", ErrBadDecision, dec.FromPhase, next.Phase)
	}
	// The pre-state must be a live run.
	if next.Lifecycle != state.LifecycleRunning || next.Pause != nil || next.Gate != nil || next.Recovery != nil {
		return fmt.Errorf("%w: the run is not live", ErrBadDecision)
	}
	// Owner precondition: an ownerless authoring phase has NO outstanding assignment;
	// every agent submit consumes the bound assignment at its issued revision.
	if ownerless {
		if next.Assignment != nil {
			return fmt.Errorf("%w: an ownerless TESTS outcome has no outstanding assignment", ErrBadDecision)
		}
	} else {
		if next.Assignment == nil || next.Assignment.ID != submitted.TurnID {
			return fmt.Errorf("%w: the submitted turn is not the outstanding assignment", ErrBadDecision)
		}
		if next.Assignment.IssuedRevision != dec.ExpectedStateRevision {
			return fmt.Errorf("%w: the assignment was not issued at the expected revision %d", ErrBadDecision, dec.ExpectedStateRevision)
		}
	}
	// Route/edge + id kind come from the ONE route table (RequiredID also rejects an
	// illegal edge and a gate-presence mismatch).
	idKind, err := RequiredID(dec)
	if err != nil {
		return err
	}
	switch idKind {
	case IDGate:
		if !state.IsRunID(ids.GateID) || ids.AssignmentTurnID != "" {
			return fmt.Errorf("%w: a gate requires exactly a canonical gate id", ErrBadDecision)
		}
	case IDAssignment:
		if !state.IsRunID(ids.AssignmentTurnID) || ids.GateID != "" {
			return fmt.Errorf("%w: a running edge requires exactly a canonical assignment id", ErrBadDecision)
		}
		if !ownerless && ids.AssignmentTurnID == submitted.TurnID {
			return fmt.Errorf("%w: the next assignment reuses the consumed turn", ErrBadDecision)
		}
		if _, ok := next.AcceptedTurns[ids.AssignmentTurnID]; ok {
			return fmt.Errorf("%w: the next assignment reuses an accepted turn", ErrBadDecision)
		}
	case IDNone:
		if ids.AssignmentTurnID != "" || ids.GateID != "" {
			return fmt.Errorf("%w: an ownerless edge issues no identity", ErrBadDecision)
		}
	default:
		return fmt.Errorf("%w: unknown id kind %d", ErrBadDecision, idKind)
	}
	return validateRoute(dec, submitted, next)
}

// isOwnerlessAuthoringPhase reports whether the phase's transition is coordinator-
// authored with no outstanding assignment (only TESTS; the verifier submit in VERIFY
// is a normal agent turn).
func isOwnerlessAuthoringPhase(p state.Phase) bool { return p == state.PhaseTests }

func badDecision(msg string) error { return fmt.Errorf("%w: %s", ErrBadDecision, msg) }

func anyPayload(dec Decision) bool {
	return dec.Plan != nil || dec.Checks != nil || dec.Findings != nil || dec.Verify != nil
}

// validateRoute enforces the exact route→edge table, the running-edge budget
// preconditions (so a forged over/at-limit edge cannot bypass the sole budget
// table), the gate subset table, and the local well-formedness of every payload
// Apply persists — all before the first write.
func validateRoute(dec Decision, submitted state.EventRef, next *state.RunState) error {
	// A verify requirement payload belongs to exactly one route; every other route
	// (including a gate that transfers it via GateSpec.Verify) carries none.
	if dec.Route != RouteToVerify && dec.Verify != nil {
		return badDecision("only to-verify carries a verify requirement payload")
	}
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
	case RouteToTests:
		if anyPayload(dec) {
			return badDecision("to-tests carries no payload")
		}
		// CHECKPOINT enters TESTS from the final step (cursor sc-1, Apply advances it);
		// a FIX return is already at the plan end. No verify requirement yet.
		if err := checkTerminalShape(next, dec.FromPhase == state.PhaseCheckpoint, false); err != nil {
			return err
		}
		if dec.FromPhase == state.PhaseFix && next.FixReturn != state.PhaseTests {
			return badDecision("a FIX to TESTS requires a TESTS fix return")
		}
	case RouteToVerify:
		if dec.Plan != nil || dec.Checks != nil || dec.Findings != nil || dec.Verify == nil || dec.Verify.RequiredGeneration == 0 {
			return badDecision("to-verify requires exactly a positive verify requirement")
		}
		// Entering VERIFY from TESTS or a FIX return: at the plan end, no current
		// requirement yet.
		if err := checkTerminalShape(next, false, false); err != nil {
			return err
		}
		if dec.FromPhase == state.PhaseFix && next.FixReturn != state.PhaseVerify {
			return badDecision("a FIX to VERIFY requires a VERIFY fix return")
		}
	case RouteToDone:
		if anyPayload(dec) {
			return badDecision("to-done carries no payload")
		}
		if err := checkTerminalShape(next, false, true); err != nil {
			return err
		}
	case RouteTestsFix:
		if anyPayload(dec) {
			return badDecision("tests-fix carries no payload")
		}
		if err := checkTerminalShape(next, false, false); err != nil {
			return err
		}
		return runningBudget(next.Counters.TestFixes, b.TestRounds)
	case RouteVerifyFix:
		if anyPayload(dec) {
			return badDecision("verify-fix carries no payload")
		}
		if err := checkTerminalShape(next, false, true); err != nil {
			return err
		}
		return runningBudget(next.Counters.VerifyFixes, b.VerifyRounds)
	default:
		return badDecision("unknown route")
	}
	return nil
}

// checkTerminalShape enforces the exact pre-state a terminal-graph edge requires: an
// agreed plan with a positive step count, the cursor at the plan end (or, for the final
// CHECKPOINT that Apply advances into TESTS, one before it), and the current verify
// requirement present exactly for a VERIFY-origin edge.
func checkTerminalShape(next *state.RunState, atFinalStep, wantVerify bool) error {
	if next.AgreedPlan == nil || next.StepIndex == nil {
		return badDecision("the terminal graph requires an agreed plan and cursor")
	}
	sc := next.AgreedPlan.Plan.StepCount
	if sc < 1 {
		return badDecision("the terminal graph requires a positive plan step count")
	}
	want := sc
	if atFinalStep {
		want = sc - 1
	}
	if *next.StepIndex != want {
		return badDecision("the terminal graph cursor is not at the required position")
	}
	if (next.Verify != nil) != wantVerify {
		return badDecision("the verify requirement presence is wrong for this edge")
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
			if g.FixReturn != next.FixReturn || !isFixReturnPhase(next.FixReturn) {
				return badDecision("a FIX human gate preserves the current fix return")
			}
		} else if g.FixReturn != "" {
			return badDecision("only a FIX human gate carries a fix return")
		}
		// A VERIFY human gate transfers the current verify requirement into the pause
		// (present, at the plan end); every other human gate carries none.
		if g.OriginPhase == state.PhaseVerify {
			if err := checkTerminalShape(next, false, true); err != nil {
				return err
			}
			if next.Verify == nil || g.Verify == nil || *g.Verify != *next.Verify {
				return badDecision("a VERIFY human gate transfers the current verify requirement")
			}
		} else if g.Verify != nil {
			return badDecision("only a VERIFY human gate carries a verify requirement")
		}
	case state.PauseQualityBudget:
		if g.Verify != nil {
			return badDecision("a quality gate carries no verify requirement")
		}
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
		case state.BudgetTest:
			if g.OriginPhase != state.PhaseTests || g.ResumePhase != state.PhaseFix || g.FixReturn != state.PhaseTests {
				return badDecision("a test quality gate is TESTS->FIX")
			}
			if dec.Checks != nil || dec.Findings != nil {
				return badDecision("a test quality gate carries no checks/findings")
			}
			if err := checkTerminalShape(next, false, false); err != nil {
				return err
			}
			return gateBudget(next.Counters.TestFixes, b.TestRounds)
		case state.BudgetVerify:
			if g.OriginPhase != state.PhaseVerify || g.ResumePhase != state.PhaseFix || g.FixReturn != state.PhaseVerify {
				return badDecision("a verify quality gate is VERIFY->FIX")
			}
			if dec.Checks != nil || dec.Findings != nil {
				return badDecision("a verify quality gate carries no checks/findings")
			}
			if err := checkTerminalShape(next, false, true); err != nil {
				return err
			}
			return gateBudget(next.Counters.VerifyFixes, b.VerifyRounds)
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

// routeSpec is the per-route metadata the engine owns exactly once: the identity a
// route issues and whether a from->next edge is legal for it. Both Apply's edge guard
// (routeAllowed) and RequiredID consume this single table, so the id kind and the
// legal edge can never drift into two independent switches.
type routeSpec struct {
	id   IDKind
	edge func(from, next state.Phase) bool
}

var routeTable = map[Route]routeSpec{
	// A gate may be raised from an agent submit phase OR the coordinator-authored TESTS
	// phase (the test-quality gate).
	RouteGate: {IDGate, func(from, next state.Phase) bool {
		return (isSubmitPhase(from) || from == state.PhaseTests) && next == state.PhaseAwaitGuidance
	}},
	RouteDraftAccepted: {IDAssignment, func(from, next state.Phase) bool {
		return from == state.PhasePlanDraft && next == state.PhasePlanCritique
	}},
	RoutePromote: {IDAssignment, func(from, next state.Phase) bool {
		return from == state.PhasePlanCritique && next == state.PhaseImplementStep
	}},
	RouteToRevise: {IDAssignment, func(from, next state.Phase) bool {
		return from == state.PhasePlanCritique && next == state.PhasePlanRevise
	}},
	RouteRetryReview: {IDAssignment, func(from, next state.Phase) bool {
		return (from == state.PhasePlanCritique && next == state.PhasePlanCritique) ||
			(from == state.PhaseCheckpoint && next == state.PhaseCheckpoint)
	}},
	RouteReviseAccepted: {IDAssignment, func(from, next state.Phase) bool {
		return from == state.PhasePlanRevise && next == state.PhasePlanCritique
	}},
	RouteToCheckpoint: {IDAssignment, func(from, next state.Phase) bool {
		return (from == state.PhaseImplementStep || from == state.PhaseFix) && next == state.PhaseCheckpoint
	}},
	RouteNextStep: {IDAssignment, func(from, next state.Phase) bool {
		return from == state.PhaseCheckpoint && next == state.PhaseImplementStep
	}},
	RouteToFix: {IDAssignment, func(from, next state.Phase) bool { return from == state.PhaseCheckpoint && next == state.PhaseFix }},
	// Ownerless terminal-graph edges issue no identity (IDNone).
	RouteToTests: {IDNone, func(from, next state.Phase) bool {
		return (from == state.PhaseCheckpoint || from == state.PhaseFix) && next == state.PhaseTests
	}},
	RouteToVerify: {IDNone, func(from, next state.Phase) bool {
		return (from == state.PhaseTests || from == state.PhaseFix) && next == state.PhaseVerify
	}},
	RouteToDone: {IDNone, func(from, next state.Phase) bool { return from == state.PhaseVerify && next == state.PhaseDone }},
	RouteTestsFix: {IDAssignment, func(from, next state.Phase) bool {
		return from == state.PhaseTests && next == state.PhaseFix
	}},
	RouteVerifyFix: {IDAssignment, func(from, next state.Phase) bool {
		return from == state.PhaseVerify && next == state.PhaseFix
	}},
}

func routeAllowed(r Route, from, next state.Phase) bool {
	s, ok := routeTable[r]
	return ok && s.edge(from, next)
}

// isSubmitPhase reports whether a phase may raise a human-decision gate (every
// agent submit phase, including VERIFY's verifier turn; TESTS is coordinator-authored
// and cannot). It mirrors state.isHumanOriginPhase.
func isSubmitPhase(p state.Phase) bool {
	switch p {
	case state.PhasePlanDraft, state.PhasePlanCritique, state.PhasePlanRevise,
		state.PhaseImplementStep, state.PhaseCheckpoint, state.PhaseFix, state.PhaseVerify:
		return true
	}
	return false
}

// isFixReturnPhase reports whether a phase is a legal FIX return target (mirrors
// state.isFixReturnPhase, which is unexported).
func isFixReturnPhase(p state.Phase) bool {
	return p == state.PhaseCheckpoint || p == state.PhaseTests || p == state.PhaseVerify
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

// issue binds the newly assigned turn and, for a read-only turn, the review-evidence packet that
// makes it actionable. The two are written together and cleared together: an assignment and its
// binding have exactly the same lifetime, so no generation can exist in which one moved without the
// other. The resulting revision is stamped into the binding here because the CAS is the first point
// at which it is known.
func issue(next *state.RunState, ids Ids, gen uint64) error {
	// next.Phase is already the resulting phase, so the requirement is decidable here — and it is
	// checked rather than assumed: an adapter that forgot to publish a packet, or supplied one for a
	// worktree turn, is a wiring bug that must fail at the transition rather than produce a state
	// only the store validator would catch.
	readOnly := !state.RepoEditPhase(next.Phase)
	if readOnly && ids.Evidence == nil {
		return fmt.Errorf("%w: a read-only %s assignment requires a published evidence packet", ErrBadDecision, next.Phase)
	}
	if !readOnly && ids.Evidence != nil {
		return fmt.Errorf("%w: a %s assignment carries a worktree and must have no evidence packet", ErrBadDecision, next.Phase)
	}
	next.Assignment = &state.Ref{ID: ids.AssignmentTurnID, IssuedRevision: gen}
	next.Evidence = nil
	if ids.Evidence != nil {
		next.Evidence = &state.AssignmentEvidence{
			TurnID:          ids.AssignmentTurnID,
			IssuedRevision:  gen,
			ManifestRelPath: ids.Evidence.ManifestRelPath,
			RootDigest:      ids.Evidence.RootDigest,
		}
	}
	return nil
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
	// A human VERIFY gate moves the verify requirement into the pause; every gate
	// clears the top-level requirement (a paused run has none). A quality gate raised
	// from VERIFY drops the requirement entirely (a fresh one is created on re-entry).
	if g.Verify != nil {
		v := *g.Verify
		pc.Verify = &v
	}
	next.Verify = nil
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
