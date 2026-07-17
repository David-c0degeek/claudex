package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

// IDKind names which identity a decision's route requires the adapter to supply.
type IDKind int

const (
	IDNone       IDKind = iota // no identity issued (a terminal edge; reserved, not in this subset)
	IDAssignment               // a next-turn assignment id (every running edge)
	IDGate                     // a gate id (a paused edge)
)

// RequiredID derives, from the SAME route table Apply consumes, which identity the
// decision issues. It fails closed on a malformed/unknown decision; in this subset
// IDNone is unreachable from a successful Evaluate (every supported non-gate route
// issues an assignment), so a None result is treated as an error by the adapter.
func RequiredID(dec Decision) (IDKind, error) {
	switch dec.Route {
	case RouteGate:
		return IDGate, nil
	case RouteDraftAccepted, RoutePromote, RouteToRevise, RouteRetryReview,
		RouteReviseAccepted, RouteToCheckpoint, RouteNextStep, RouteToFix:
		return IDAssignment, nil
	}
	return IDNone, fmt.Errorf("%w: no id kind for route %d", ErrBadDecision, dec.Route)
}

// --- planning-history materializer ---

// ErrHistory means the accepted planning history is inconsistent (a bad identity/
// ordering, a broken revision base chain, or a promote in a pre-agreement candidate).
var ErrHistory = errors.New("engine: inconsistent planning history")

// ErrHistoryTooLarge means the planning history exceeds a durable bound.
var ErrHistoryTooLarge = errors.New("engine: planning history exceeds its bound")

// MaxPlanningArtifacts and MaxMaterializeBytes bound a fold's inputs, so a
// reconstruction can never scan an unbounded history.
const (
	MaxPlanningArtifacts = 4096
	MaxMaterializeBytes  = 8 << 20
)

// AcceptedArtifact is one accepted planning turn, identity-bound: the engine
// verifies these values (never trusting the caller's parallel bookkeeping).
type AcceptedArtifact struct {
	TurnID          string
	Digest          string
	Phase           state.Phase
	ReceiptRevision uint64
	Canonical       []byte
}

// CandidateRefs is the durable summary of the reconstructed candidate, which the
// caller asserts against the locked state before use.
type CandidateRefs struct {
	Source      state.EventRef
	PlanDigest  string
	StepCount   int
	CheckKeys   []string
	CheckDigest string
}

// phaseMessageType maps a planning phase to the message type its accepted artifact
// must declare (the provenance the fold verifies).
var phaseMessageType = map[state.Phase]string{
	state.PhasePlanDraft:    "plan",
	state.PhasePlanCritique: "plan_critique",
	state.PhasePlanRevise:   "plan_revision",
}

// MaterializeCandidate reconstructs the current candidate plan and check set by
// replaying the accepted PLAN_DRAFT/PLAN_CRITIQUE/PLAN_REVISE artifacts in
// receipt-revision order. It fully projects every artifact against the running
// materialization (validating check removes, a revision's base chain, and its exact
// responses), classifies each with the shared gated/actionable/converged/retry rule,
// and commits only the committed outcomes — a human-gated or reviewer-retry artifact
// is validated but discarded. It rejects any history the real engine would reject.
func MaterializeCandidate(arts []AcceptedArtifact) (ProjectionFacts, CandidateRefs, error) {
	if len(arts) > MaxPlanningArtifacts {
		return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: %d artifacts", ErrHistoryTooLarge, len(arts))
	}
	var (
		plan       CanonicalPlan
		havePlan   bool
		planDigest string
		source     state.EventRef
		checks     []MaterializedCheck
		pending    []string
		expected   = state.PhasePlanDraft
		prevRcpt   uint64
		totalBytes int
	)
	for _, a := range arts {
		totalBytes += len(a.Canonical)
		if totalBytes > MaxMaterializeBytes {
			return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: %d bytes", ErrHistoryTooLarge, totalBytes)
		}
		if err := verifyArtifactIdentity(a, prevRcpt); err != nil {
			return ProjectionFacts{}, CandidateRefs{}, err
		}
		prevRcpt = a.ReceiptRevision
		if a.Phase != expected {
			return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: %s out of order (expected %s)", ErrHistory, a.Phase, expected)
		}

		rhd, err := artifactDecision(a.Canonical)
		if err != nil {
			return ProjectionFacts{}, CandidateRefs{}, err
		}

		switch a.Phase {
		case state.PhasePlanDraft:
			p, d, perr := materializePlanArtifact(a.Canonical)
			if perr != nil {
				return ProjectionFacts{}, CandidateRefs{}, perr
			}
			if rhd { // human-gated draft: validated, not committed
				continue
			}
			plan, planDigest, havePlan = p, d, true
			checks = nil
			pending = nil
			source = state.EventRef{Digest: a.Digest, TurnID: a.TurnID}
			expected = state.PhasePlanCritique
		case state.PhasePlanRevise:
			if !havePlan {
				return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: revision without a committed plan", ErrHistory)
			}
			p, d, perr := materializeRevisionArtifact(a.Canonical, plan, planDigest, pending)
			if perr != nil {
				return ProjectionFacts{}, CandidateRefs{}, perr
			}
			if rhd { // human-gated revision: validated (base+responses), not committed
				continue
			}
			plan, planDigest = p, d
			pending = nil
			source = state.EventRef{Digest: a.Digest, TurnID: a.TurnID}
			expected = state.PhasePlanCritique
		case state.PhasePlanCritique:
			if !havePlan {
				return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: critique before a committed plan", ErrHistory)
			}
			resulting, actionable, converged, cerr := materializeCritiqueArtifact(a.Canonical, checks, plan)
			if cerr != nil {
				return ProjectionFacts{}, CandidateRefs{}, cerr
			}
			switch {
			case rhd: // human-gated critique: validated, not committed
			case converged:
				return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: a converged critique cannot be a pre-agreement candidate", ErrHistory)
			case actionable:
				checks = resulting
				pending = actionableKeys(a.Canonical)
				expected = state.PhasePlanRevise
			default: // reviewer retry: validated, not committed
			}
		default:
			return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: unexpected phase %s", ErrHistory, a.Phase)
		}
	}
	if !havePlan {
		return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: no committed candidate plan", ErrHistory)
	}
	checkRef, err := materializeChecks(checks)
	if err != nil {
		return ProjectionFacts{}, CandidateRefs{}, err
	}
	facts := ProjectionFacts{
		CandidatePlan:   clonePlan(plan),
		CandidateChecks: cloneMaterializedChecks(checks),
		CandidateSource: source,
	}
	refs := CandidateRefs{
		Source:      source,
		PlanDigest:  planDigest,
		StepCount:   len(plan.Steps),
		CheckKeys:   append([]string(nil), checkRef.Keys...),
		CheckDigest: checkRef.Digest,
	}
	return facts, refs, nil
}

// verifyArtifactIdentity checks the identity-bound facts before any projection.
func verifyArtifactIdentity(a AcceptedArtifact, prevRcpt uint64) error {
	if a.ReceiptRevision <= prevRcpt {
		return fmt.Errorf("%w: receipt revision %d not strictly after %d", ErrHistory, a.ReceiptRevision, prevRcpt)
	}
	if !state.IsHex64(a.Digest) {
		return fmt.Errorf("%w: digest is not hex64", ErrHistory)
	}
	sum := sha256.Sum256(a.Canonical)
	if hex.EncodeToString(sum[:]) != a.Digest {
		return fmt.Errorf("%w: artifact bytes do not match the digest", ErrHistory)
	}
	var env struct {
		MessageType   string `json:"message_type"`
		TurnID        string `json:"turn_id"`
		StateRevision uint64 `json:"state_revision"`
	}
	if err := json.Unmarshal(a.Canonical, &env); err != nil {
		return fmt.Errorf("%w: undecodable envelope", ErrHistory)
	}
	if env.TurnID != a.TurnID {
		return fmt.Errorf("%w: envelope turn id does not match", ErrHistory)
	}
	if want, ok := phaseMessageType[a.Phase]; !ok || env.MessageType != want {
		return fmt.Errorf("%w: message type %q does not match phase %s", ErrHistory, env.MessageType, a.Phase)
	}
	if env.StateRevision == 0 || env.StateRevision >= a.ReceiptRevision {
		return fmt.Errorf("%w: submitted revision %d is not before the receipt revision %d", ErrHistory, env.StateRevision, a.ReceiptRevision)
	}
	return nil
}

func artifactDecision(canonical []byte) (bool, error) {
	_, rhd, err := envelope(canonical)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrHistory, err)
	}
	return rhd, nil
}

// materializePlanArtifact parses a plan and returns its CanonicalPlan + doc digest.
func materializePlanArtifact(canonical []byte) (CanonicalPlan, string, error) {
	var a struct {
		Markdown      string     `json:"plan_markdown"`
		Steps         []jsonStep `json:"steps"`
		Risks         []string   `json:"risks"`
		OpenQuestions []string   `json:"open_questions"`
	}
	if err := json.Unmarshal(canonical, &a); err != nil {
		return CanonicalPlan{}, "", semanticf("undecodable plan artifact")
	}
	steps := toPlanSteps(a.Steps)
	if err := protocol.ValidateStepTitles(stepTitles(steps)); err != nil {
		return CanonicalPlan{}, "", semanticf("plan step titles: %v", err)
	}
	p := CanonicalPlan{Markdown: a.Markdown, Steps: steps, Risks: a.Risks, OpenQuestions: a.OpenQuestions}
	d, err := planDocDigest(p.Markdown, p.Steps, p.Risks, p.OpenQuestions)
	return p, d, err
}

// materializeRevisionArtifact validates a revision against the base plan digest and
// the exact pending findings, then returns the materialized resulting plan + digest.
func materializeRevisionArtifact(canonical []byte, base CanonicalPlan, baseDigest string, pending []string) (CanonicalPlan, string, error) {
	res, err := applyRevision(canonical, base, baseDigest, pending)
	if err != nil {
		return CanonicalPlan{}, "", err
	}
	d, derr := planDocDigest(res.Markdown, res.Steps, res.Risks, res.OpenQuestions)
	return res, d, derr
}

// materializeCritiqueArtifact validates a critique's ops against the current checks
// and returns the resulting set plus the actionable/converged classification.
func materializeCritiqueArtifact(canonical []byte, current []MaterializedCheck, plan CanonicalPlan) (resulting []MaterializedCheck, actionable, converged bool, err error) {
	var a struct {
		Verdict  string `json:"verdict"`
		Findings []struct {
			Key      string `json:"key"`
			Severity string `json:"severity"`
		} `json:"findings"`
		ImplementationChecks []jsonCheckOp `json:"implementation_checks"`
		MissingEvidence      []string      `json:"missing_evidence"`
	}
	if err := json.Unmarshal(canonical, &a); err != nil {
		return nil, false, false, semanticf("undecodable critique artifact")
	}
	findingKeys := make([]string, 0, len(a.Findings))
	for _, f := range a.Findings {
		findingKeys = append(findingKeys, f.Key)
	}
	if err := protocol.ValidateKeySet("findings", findingKeys); err != nil {
		return nil, false, false, semanticf("%v", err)
	}
	opKeys := make([]string, 0, len(a.ImplementationChecks))
	for _, op := range a.ImplementationChecks {
		opKeys = append(opKeys, op.Key)
	}
	if err := protocol.ValidateKeySet("implementation_checks", opKeys); err != nil {
		return nil, false, false, semanticf("%v", err)
	}
	applied, err := applyCheckOps(current, a.ImplementationChecks)
	if err != nil {
		return nil, false, false, err
	}
	if _, err := materializeChecks(applied); err != nil { // bound + grammar
		return nil, false, false, err
	}
	for _, f := range a.Findings {
		if f.Severity == "blocking" || f.Severity == "major" {
			actionable = true
		}
	}
	converged = a.Verdict == "AGREE" && len(a.MissingEvidence) == 0 && !actionable
	return applied, actionable, converged, nil
}

func actionableKeys(canonical []byte) []string {
	var a struct {
		Findings []struct {
			Key      string `json:"key"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	_ = json.Unmarshal(canonical, &a)
	var keys []string
	for _, f := range a.Findings {
		if f.Severity == "blocking" || f.Severity == "major" {
			keys = append(keys, f.Key)
		}
	}
	sort.Strings(keys) // parity with evalPlanCritiqued's sorted actionable keys
	return keys
}

func clonePlan(p CanonicalPlan) CanonicalPlan {
	out := CanonicalPlan{Markdown: p.Markdown}
	out.Risks = append([]string(nil), p.Risks...)
	out.OpenQuestions = append([]string(nil), p.OpenQuestions...)
	out.Steps = make([]PlanStep, len(p.Steps))
	for i, s := range p.Steps {
		out.Steps[i] = PlanStep{
			Title: s.Title, Description: s.Description,
			Files: append([]string(nil), s.Files...),
			Tests: append([]string(nil), s.Tests...),
		}
	}
	return out
}

func cloneMaterializedChecks(cs []MaterializedCheck) []MaterializedCheck {
	out := make([]MaterializedCheck, len(cs))
	for i, c := range cs {
		out[i] = MaterializedCheck{Key: c.Key, Description: c.Description, Evidence: c.Evidence}
		if c.TargetStep != nil {
			t := *c.TargetStep
			out[i].TargetStep = &t
		}
	}
	return out
}
