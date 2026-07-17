package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

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

// RequiredID derives, from the SAME route table Apply's edge guard consumes, which
// identity the decision issues. It fails closed on a malformed decision — an unknown
// route, an illegal from->next edge, or a gate-presence that disagrees with the route
// — before any id is minted, so a fabricated decision cannot request the wrong id. In
// this subset IDNone is unreachable from a successful Evaluate (every supported
// non-gate route issues an assignment); a None result is always an error.
func RequiredID(dec Decision) (IDKind, error) {
	s, ok := routeTable[dec.Route]
	if !ok {
		return IDNone, fmt.Errorf("%w: no id kind for route %d", ErrBadDecision, dec.Route)
	}
	if !s.edge(dec.FromPhase, dec.Next) {
		return IDNone, fmt.Errorf("%w: illegal edge %s->%s for route %d", ErrBadDecision, dec.FromPhase, dec.Next, dec.Route)
	}
	if (s.id == IDGate) != (dec.Gate != nil) {
		return IDNone, fmt.Errorf("%w: gate presence does not match route %d", ErrBadDecision, dec.Route)
	}
	return s.id, nil
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
// caller asserts against the locked state before use. It carries every fact the
// coordinator cross-checks: the plan source/digest/step-count, both the check keys
// AND the check digest, the phase the run should be in, and the outstanding finding
// obligations (nil when none) — so a well-shaped state that carries a different
// pending set or expected phase than the immutable history cannot pass.
type CandidateRefs struct {
	Source          state.EventRef
	PlanDigest      string
	StepCount       int
	CheckKeys       []string
	CheckDigest     string
	ExpectedPhase   state.Phase
	PendingFindings *state.FindingObligations // deep-copied; nil when none outstanding
}

// phaseMessageType maps a planning phase to the message type its accepted artifact
// must declare (the provenance the fold verifies, and the schema it validates against).
var phaseMessageType = map[state.Phase]string{
	state.PhasePlanDraft:    "plan",
	state.PhasePlanCritique: "plan_critique",
	state.PhasePlanRevise:   "plan_revision",
}

// MaterializeCandidate reconstructs the current candidate plan and check set by
// replaying the accepted PLAN_DRAFT/PLAN_CRITIQUE/PLAN_REVISE artifacts in
// receipt-revision order. Every artifact is fully schema-validated (exactly as
// transport validated it at accept time) and projected through the SAME engine
// helpers the live projector uses, then classified with the shared
// gated/actionable/converged/retry rule; only the committed outcomes advance the
// running facts — a human-gated or reviewer-retry artifact is validated but discarded.
// It rejects any history the real engine would reject.
func MaterializeCandidate(arts []AcceptedArtifact) (ProjectionFacts, CandidateRefs, error) {
	if len(arts) > MaxPlanningArtifacts {
		return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: %d artifacts", ErrHistoryTooLarge, len(arts))
	}
	var (
		plan          CanonicalPlan
		havePlan      bool
		planDigest    string
		source        state.EventRef
		checks        []MaterializedCheck
		pending       []string
		pendingSource state.EventRef
		expected      = state.PhasePlanDraft
		prevRcpt      uint64
		totalBytes    int
	)
	for i := range arts {
		a := arts[i]
		// Overflow-safe byte bound: never form totalBytes+len before comparing.
		if len(a.Canonical) > MaxMaterializeBytes || totalBytes > MaxMaterializeBytes-len(a.Canonical) {
			return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: over %d bytes", ErrHistoryTooLarge, MaxMaterializeBytes)
		}
		totalBytes += len(a.Canonical)

		msgType, ok := phaseMessageType[a.Phase]
		if !ok {
			return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: unexpected phase %s", ErrHistory, a.Phase)
		}
		// Full schema validation. A real accepted artifact passed transport's
		// protocol.Validate; the fold must too, so a digest-correct artifact with a
		// missing/extra/invalid field (which transport could never have accepted) is
		// rejected before it can be projected. The stored bytes must equal the
		// canonical form.
		canon, verr := protocol.Validate(msgType, a.Canonical)
		if verr != nil {
			return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: schema: %v", ErrHistory, verr)
		}
		if !bytes.Equal(canon, a.Canonical) {
			return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: artifact bytes are not canonical", ErrHistory)
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
			p, d, perr := materializePlan(a.Canonical)
			if perr != nil {
				return ProjectionFacts{}, CandidateRefs{}, perr
			}
			if rhd { // human-gated draft: validated, not committed
				continue
			}
			plan, planDigest, havePlan = p, d, true
			checks = nil
			pending = nil
			pendingSource = state.EventRef{}
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
			pendingSource = state.EventRef{}
			source = state.EventRef{Digest: a.Digest, TurnID: a.TurnID}
			expected = state.PhasePlanCritique
		case state.PhasePlanCritique:
			if !havePlan {
				return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: critique before a committed plan", ErrHistory)
			}
			proj, cerr := projectCritiqueParts(a.Canonical, checks, plan.Steps)
			if cerr != nil {
				return ProjectionFacts{}, CandidateRefs{}, cerr
			}
			actionable := len(proj.ActionableKeys) > 0
			converged := proj.Verdict == "AGREE" && !proj.Missing && !actionable
			switch {
			case rhd: // human-gated critique: validated, not committed
			case converged:
				return ProjectionFacts{}, CandidateRefs{}, fmt.Errorf("%w: a converged critique cannot be a pre-agreement candidate", ErrHistory)
			case actionable:
				checks = proj.Resulting
				pending = proj.ActionableKeys
				pendingSource = state.EventRef{Digest: a.Digest, TurnID: a.TurnID}
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
		Source:        source,
		PlanDigest:    planDigest,
		StepCount:     len(plan.Steps),
		CheckKeys:     append([]string(nil), checkRef.Keys...),
		CheckDigest:   checkRef.Digest,
		ExpectedPhase: expected,
	}
	if len(pending) > 0 {
		refs.PendingFindings = &state.FindingObligations{
			Source: pendingSource,
			Keys:   append([]string(nil), pending...),
		}
	}
	return facts, refs, nil
}

// verifyArtifactIdentity checks the identity-bound facts before any projection. The
// artifact's message type is validated against its phase by protocol.Validate (the
// schema pass), so this checks only the receipt ordering, digest, envelope turn id,
// and the submitted-below-receipt revision invariant.
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
		TurnID        string `json:"turn_id"`
		StateRevision uint64 `json:"state_revision"`
	}
	if err := json.Unmarshal(a.Canonical, &env); err != nil {
		return fmt.Errorf("%w: undecodable envelope", ErrHistory)
	}
	if env.TurnID != a.TurnID {
		return fmt.Errorf("%w: envelope turn id does not match", ErrHistory)
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

// materializeRevisionArtifact validates a revision against the base plan digest and
// the exact pending findings, then returns the materialized resulting plan + digest.
// It delegates to the shared applyRevision (the same one projectRevision uses).
func materializeRevisionArtifact(canonical []byte, base CanonicalPlan, baseDigest string, pending []string) (CanonicalPlan, string, error) {
	res, err := applyRevision(canonical, base, baseDigest, pending)
	if err != nil {
		return CanonicalPlan{}, "", err
	}
	d, derr := planDocDigest(res.Markdown, res.Steps, res.Risks, res.OpenQuestions)
	return res, d, derr
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
