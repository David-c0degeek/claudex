package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

// accepted canonicalizes a schema-valid artifact and binds it to a receipt revision,
// mirroring exactly what transport stores after Apply: the canonical bytes, their
// sha256 digest, the envelope turn id, and the phase. state_revision inside the bytes
// is whatever the builder declared; the materializer requires only that it be nonzero
// and strictly below the receipt revision.
func accepted(t *testing.T, phase state.Phase, canonical []byte, receiptRev uint64) AcceptedArtifact {
	t.Helper()
	msgType, ok := phaseMessageType[phase]
	if !ok {
		t.Fatalf("no message type for phase %s", phase)
	}
	canon, err := protocol.Validate(msgType, canonical)
	if err != nil {
		t.Fatalf("validate %s: %v", msgType, err)
	}
	sum := sha256.Sum256(canon)
	var env struct {
		TurnID string `json:"turn_id"`
	}
	if err := json.Unmarshal(canon, &env); err != nil {
		t.Fatalf("decode turn id: %v", err)
	}
	return AcceptedArtifact{
		TurnID:          env.TurnID,
		Digest:          hex.EncodeToString(sum[:]),
		Phase:           phase,
		ReceiptRevision: receiptRev,
		Canonical:       canon,
	}
}

// cleanPlanHistory is the minimal committed candidate: draft -> actionable critique
// (one blocking finding + one check add) -> revise answering it. It leaves the run at
// PLAN_REVISE-answered/PLAN_CRITIQUE with the plan unchanged and one materialized check.
func cleanPlanHistory(t *testing.T, f planFixture) []AcceptedArtifact {
	t.Helper()
	// Realistic revision chain: each submitted state_revision sits in
	// [prior receipt, own receipt) — a turn is submitted at or after the prior
	// receipt and always before its own.
	return []AcceptedArtifact{
		accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10),
		accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit-act", 11, "REVISE", false, []string{"blocking"}, nil, []map[string]any{addCheck("chk-1", "step one")}), 20),
		accepted(t, state.PhasePlanRevise, revisionArtifact(t, "t-rev", 21, f.digest(t), []string{"finding-a"}), 30),
	}
}

func TestMaterializeCandidateCleanHistory(t *testing.T) {
	f := twoStepPlan()
	facts, refs, err := MaterializeCandidate(cleanPlanHistory(t, f))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if refs.PlanDigest != f.digest(t) {
		t.Fatalf("plan digest = %s, want %s", refs.PlanDigest, f.digest(t))
	}
	if refs.StepCount != 2 {
		t.Fatalf("step count = %d, want 2", refs.StepCount)
	}
	if !reflect.DeepEqual(refs.CheckKeys, []string{"chk-1"}) {
		t.Fatalf("check keys = %v, want [chk-1]", refs.CheckKeys)
	}
	wantChecks, _ := materializeChecks([]MaterializedCheck{{Key: "chk-1", Description: "check chk-1", Evidence: "ev chk-1", TargetStep: strptr("step one")}})
	if refs.CheckDigest != wantChecks.Digest {
		t.Fatalf("check digest = %s, want %s", refs.CheckDigest, wantChecks.Digest)
	}
	// The source is the last plan-producing artifact (the accepted revise).
	if refs.Source.TurnID != "t-rev" {
		t.Fatalf("source turn = %s, want t-rev", refs.Source.TurnID)
	}
	if facts.CandidateSource != refs.Source {
		t.Fatalf("facts source %+v != refs source %+v", facts.CandidateSource, refs.Source)
	}
	// The reconstructed facts must re-verify against a durable candidate carrying the
	// same refs — the exact cross-check the coordinator performs before use.
	cur := candidateState(refs)
	if err := verifyCandidateFacts(cur, facts); err != nil {
		t.Fatalf("reconstructed facts do not verify against their own refs: %v", err)
	}
}

// The guardrail Codex required: human-gated draft/critique/revise turns and
// non-actionable reviewer-retry critiques are accepted but commit nothing, so
// interleaving them must not change the reconstructed plan/check facts one bit.
func TestMaterializeCandidateIgnoresGatedAndRetryTurns(t *testing.T) {
	f := twoStepPlan()

	// The three committed artifacts are byte-identical to the clean history (same turn
	// ids, state revisions, and content), so an unchanged reconstruction is provable by
	// deep-equality — only the interspersed non-committing turns and receipt revisions
	// differ.
	// The three committed artifacts carry fixed submitted revisions (5/15/25) high
	// enough to satisfy the realistic revision chain in BOTH histories, so their bytes
	// are byte-identical and an unchanged reconstruction is provable by deep-equality.
	draft := planArtifact(t, "t-plan", 5, false, f)
	critAct := critiqueArtifact(t, "t-crit-act", 15, "REVISE", false, []string{"blocking"}, nil, []map[string]any{addCheck("chk-1", "step one")})
	revise := revisionArtifact(t, "t-rev", 25, f.digest(t), []string{"finding-a"})

	noisy := []AcceptedArtifact{
		// Human-gated draft: validated, discarded; the run resumes to PLAN_DRAFT.
		accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan-gated", 1, true, f), 3),
		accepted(t, state.PhasePlanDraft, draft, 10),
		// Human-gated critique carrying a check op that must never leak into the set.
		accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit-gated", 10, "REVISE", true, []string{"blocking"}, nil, []map[string]any{addCheck("chk-ignored", "")}), 11),
		// Reviewer retry (REVISE, only a nit) also carrying a discarded check op.
		accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit-retry", 11, "REVISE", false, []string{"nit"}, nil, []map[string]any{addCheck("chk-ignored-2", "")}), 12),
		accepted(t, state.PhasePlanCritique, critAct, 20),
		accepted(t, state.PhasePlanRevise, revise, 30),
	}

	clean := []AcceptedArtifact{
		accepted(t, state.PhasePlanDraft, draft, 10),
		accepted(t, state.PhasePlanCritique, critAct, 20),
		accepted(t, state.PhasePlanRevise, revise, 30),
	}

	cf, cr, cerr := MaterializeCandidate(clean)
	nf, nr, nerr := MaterializeCandidate(noisy)
	if cerr != nil || nerr != nil {
		t.Fatalf("materialize errors: clean=%v noisy=%v", cerr, nerr)
	}
	if !reflect.DeepEqual(cf, nf) {
		t.Fatalf("facts differ:\n clean=%+v\n noisy=%+v", cf, nf)
	}
	if !reflect.DeepEqual(cr, nr) {
		t.Fatalf("refs differ:\n clean=%+v\n noisy=%+v", cr, nr)
	}
	if !reflect.DeepEqual(nr.CheckKeys, []string{"chk-1"}) {
		t.Fatalf("a discarded check op leaked into the set: %v", nr.CheckKeys)
	}
}

func TestMaterializeCandidateRejectsBadHistory(t *testing.T) {
	f := twoStepPlan()
	good := cleanPlanHistory(t, f)

	t.Run("empty", func(t *testing.T) {
		if _, _, err := MaterializeCandidate(nil); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})

	t.Run("out of order: critique first", func(t *testing.T) {
		if _, _, err := MaterializeCandidate(good[1:]); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})

	t.Run("non-increasing receipt revision", func(t *testing.T) {
		h := cleanPlanHistory(t, f)
		h[1].ReceiptRevision = h[0].ReceiptRevision // not strictly after
		if _, _, err := MaterializeCandidate(h); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})

	t.Run("digest does not match bytes", func(t *testing.T) {
		h := cleanPlanHistory(t, f)
		h[0].Digest = hex.EncodeToString(make([]byte, 32)) // valid hex64, wrong value
		if _, _, err := MaterializeCandidate(h); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})

	t.Run("state revision not before receipt", func(t *testing.T) {
		// A draft whose declared state_revision equals its receipt revision.
		h := []AcceptedArtifact{accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 10, false, f), 10)}
		if _, _, err := MaterializeCandidate(h); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})

	t.Run("message type does not match phase", func(t *testing.T) {
		// A critique artifact mislabeled as a draft-phase turn.
		crit := critiqueArtifact(t, "t-x", 1, "REVISE", false, []string{"blocking"}, nil, nil)
		a := accepted(t, state.PhasePlanCritique, crit, 10)
		a.Phase = state.PhasePlanDraft // envelope still says plan_critique
		if _, _, err := MaterializeCandidate([]AcceptedArtifact{a}); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})

	t.Run("converged critique cannot be a pre-agreement candidate", func(t *testing.T) {
		h := []AcceptedArtifact{
			accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10),
			accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit", 1, "AGREE", false, nil, nil, nil), 20),
		}
		if _, _, err := MaterializeCandidate(h); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})

	t.Run("revision responses not exact", func(t *testing.T) {
		h := cleanPlanHistory(t, f)
		// Rebuild the revise answering the wrong finding key (keep the realistic revision).
		h[2] = accepted(t, state.PhasePlanRevise, revisionArtifact(t, "t-rev", 21, f.digest(t), []string{"finding-z"}), 30)
		if _, _, err := MaterializeCandidate(h); !errors.Is(err, ErrSemantic) {
			t.Fatalf("err = %v, want ErrSemantic", err)
		}
	})

	t.Run("stale submitted revision", func(t *testing.T) {
		// A critique submitted against a revision below the draft's receipt: a turn
		// accepted after receipt 10 cannot have been submitted at revision 5.
		h := []AcceptedArtifact{
			accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10),
			accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit", 5, "REVISE", false, []string{"blocking"}, nil, nil), 20),
		}
		if _, _, err := MaterializeCandidate(h); !errors.Is(err, ErrHistory) {
			t.Fatalf("stale err = %v, want ErrHistory", err)
		}
	})

	t.Run("duplicate turn id", func(t *testing.T) {
		dup := accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10)
		if _, _, err := MaterializeCandidate([]AcceptedArtifact{dup, dup}); !errors.Is(err, ErrHistory) {
			t.Fatalf("dup err = %v, want ErrHistory", err)
		}
	})

	t.Run("too many artifacts", func(t *testing.T) {
		big := make([]AcceptedArtifact, MaxPlanningArtifacts+1)
		if _, _, err := MaterializeCandidate(big); !errors.Is(err, ErrHistoryTooLarge) {
			t.Fatalf("err = %v, want ErrHistoryTooLarge", err)
		}
	})
}

// A bare committed draft leaves the run at PLAN_CRITIQUE with a canonical-empty check
// set: the refs must carry a NON-NIL empty key slice and the empty digest, and the
// facts must round-trip so the first PLAN_CRITIQUE submit verifies.
func TestMaterializeCandidateDraftOnlyEmptyChecks(t *testing.T) {
	f := twoStepPlan()
	h := []AcceptedArtifact{accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10)}
	facts, refs, err := MaterializeCandidate(h)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if refs.CheckKeys == nil || len(refs.CheckKeys) != 0 {
		t.Fatalf("check keys must be a non-nil empty slice, got %#v", refs.CheckKeys)
	}
	empty, _ := materializeChecks(nil)
	if refs.CheckDigest != empty.Digest {
		t.Fatalf("check digest = %s, want the empty-array digest %s", refs.CheckDigest, empty.Digest)
	}
	if refs.ExpectedPhase != state.PhasePlanCritique {
		t.Fatalf("expected phase = %s, want PLAN_CRITIQUE", refs.ExpectedPhase)
	}
	if refs.PendingFindings != nil {
		t.Fatalf("no pending after a bare draft: %+v", refs.PendingFindings)
	}
	if err := verifyCandidateFacts(candidateState(refs), facts); err != nil {
		t.Fatalf("first-critique round-trip failed: %v", err)
	}
}

// rawAccepted binds a possibly schema-INVALID artifact (skips protocol.Validate), so
// the fold's own schema pass is what must reject it.
func rawAccepted(t *testing.T, phase state.Phase, canonical []byte, receiptRev uint64) AcceptedArtifact {
	t.Helper()
	sum := sha256.Sum256(canonical)
	var env struct {
		TurnID string `json:"turn_id"`
	}
	if err := json.Unmarshal(canonical, &env); err != nil {
		t.Fatalf("decode turn id: %v", err)
	}
	return AcceptedArtifact{
		TurnID:          env.TurnID,
		Digest:          hex.EncodeToString(sum[:]),
		Phase:           phase,
		ReceiptRevision: receiptRev,
		Canonical:       canonical,
	}
}

// A digest-correct artifact that transport could never have accepted (missing a
// required field, an unknown property, or an invalid enum) is rejected by the fold's
// schema pass.
func TestMaterializeCandidateSchemaValidates(t *testing.T) {
	f := twoStepPlan()

	t.Run("missing required field", func(t *testing.T) {
		bad := mustJSON(t, map[string]any{
			"protocol_version": 1, "message_type": "plan", "turn_id": "t-plan", "state_revision": 1,
			"human_context": nil, "requires_human_decision": false, "decision_question": nil,
			"plan_markdown": f.Markdown, "risks": f.Risks, "open_questions": f.OpenQ, // steps omitted
		})
		if _, _, err := MaterializeCandidate([]AcceptedArtifact{rawAccepted(t, state.PhasePlanDraft, bad, 10)}); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})

	t.Run("unknown property", func(t *testing.T) {
		bad := mustJSON(t, map[string]any{
			"protocol_version": 1, "message_type": "plan", "turn_id": "t-plan", "state_revision": 1,
			"human_context": nil, "requires_human_decision": false, "decision_question": nil,
			"plan_markdown": f.Markdown, "steps": stepsJSON(f.Steps), "risks": f.Risks, "open_questions": f.OpenQ,
			"surprise": "extra",
		})
		if _, _, err := MaterializeCandidate([]AcceptedArtifact{rawAccepted(t, state.PhasePlanDraft, bad, 10)}); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})

	t.Run("invalid verdict enum", func(t *testing.T) {
		draft := accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10)
		badCrit := mustJSON(t, map[string]any{
			"protocol_version": 1, "message_type": "plan_critique", "turn_id": "t-crit", "state_revision": 1,
			"human_context": nil, "requires_human_decision": false, "decision_question": nil,
			"verdict": "MAYBE", "findings": []any{}, "implementation_checks": []any{},
			"missing_evidence": []any{}, "simpler_alternative": nil, "notes": "n",
		})
		h := []AcceptedArtifact{draft, rawAccepted(t, state.PhasePlanCritique, badCrit, 20)}
		if _, _, err := MaterializeCandidate(h); !errors.Is(err, ErrHistory) {
			t.Fatalf("err = %v, want ErrHistory", err)
		}
	})
}

// The refs bind the expected phase and the exact outstanding findings (source+keys),
// so a well-shaped state carrying a different pending set cannot pass the coordinator.
func TestMaterializeCandidateBindsPendingAndExpectedPhase(t *testing.T) {
	f := twoStepPlan()

	// Ends at an actionable critique: the run should be at PLAN_REVISE with the exact
	// pending finding keys sourced from that critique.
	atRevise := []AcceptedArtifact{
		accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10),
		accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit-act", 11, "REVISE", false, []string{"blocking"}, nil, []map[string]any{addCheck("chk-1", "step one")}), 20),
	}
	_, refs, err := MaterializeCandidate(atRevise)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if refs.ExpectedPhase != state.PhasePlanRevise {
		t.Fatalf("expected phase = %s, want PLAN_REVISE", refs.ExpectedPhase)
	}
	if refs.PendingFindings == nil || !reflect.DeepEqual(refs.PendingFindings.Keys, []string{"finding-a"}) {
		t.Fatalf("pending findings = %+v, want [finding-a]", refs.PendingFindings)
	}
	if refs.PendingFindings.Source.TurnID != "t-crit-act" {
		t.Fatalf("pending source = %+v, want t-crit-act", refs.PendingFindings.Source)
	}

	// Ends at a committed revise: the run is back at PLAN_CRITIQUE with no pending.
	_, refs2, err := MaterializeCandidate(cleanPlanHistory(t, f))
	if err != nil {
		t.Fatalf("materialize full: %v", err)
	}
	if refs2.ExpectedPhase != state.PhasePlanCritique {
		t.Fatalf("expected phase = %s, want PLAN_CRITIQUE", refs2.ExpectedPhase)
	}
	if refs2.PendingFindings != nil {
		t.Fatalf("pending should be nil after a committed revise: %+v", refs2.PendingFindings)
	}
}

// Each call returns fresh deep copies: mutating one call's nested plan/check/pending
// slices does not corrupt a subsequent reconstruction of the same history.
func TestMaterializeCandidateReturnsDeepCopies(t *testing.T) {
	f := twoStepPlan()
	h := []AcceptedArtifact{
		accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10),
		accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit-act", 11, "REVISE", false, []string{"blocking"}, nil, []map[string]any{addCheck("chk-1", "step one")}), 20),
	}
	facts, refs, err := MaterializeCandidate(h)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	facts.CandidatePlan.Steps[0].Title = "MUT"
	facts.CandidatePlan.Steps[0].Files[0] = "MUT"
	facts.CandidateChecks[0].Key = "MUT"
	refs.CheckKeys[0] = "MUT"
	refs.PendingFindings.Keys[0] = "MUT"

	facts2, refs2, err := MaterializeCandidate(h)
	if err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	if facts2.CandidatePlan.Steps[0].Title == "MUT" || facts2.CandidatePlan.Steps[0].Files[0] == "MUT" {
		t.Fatalf("plan slices are shared across calls")
	}
	if facts2.CandidateChecks[0].Key == "MUT" || refs2.CheckKeys[0] == "MUT" {
		t.Fatalf("check slices are shared across calls")
	}
	if refs2.PendingFindings.Keys[0] == "MUT" {
		t.Fatalf("pending keys are shared across calls")
	}
}

func TestMaterializeCandidateBytesBound(t *testing.T) {
	f := twoStepPlan()
	a := accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10)
	a.Canonical = make([]byte, MaxMaterializeBytes+1) // over the byte bound
	if _, _, err := MaterializeCandidate([]AcceptedArtifact{a}); !errors.Is(err, ErrHistoryTooLarge) {
		t.Fatalf("err = %v, want ErrHistoryTooLarge", err)
	}
}

// candidateState builds the minimal durable RunState the reconstructed refs describe,
// so verifyCandidateFacts can round-trip the materializer's own output.
func candidateState(refs CandidateRefs) state.RunState {
	return state.RunState{
		CandidatePlan:   &state.PlanRef{Source: refs.Source, Digest: refs.PlanDigest, StepCount: refs.StepCount},
		CandidateChecks: &state.CheckSetRef{Keys: refs.CheckKeys, Digest: refs.CheckDigest},
	}
}

// Project rejects loader-supplied facts whose candidate source disagrees with the
// durable candidate source, even when the plan digest and checks match — a swapped
// history that reproduces the plan bytes must not pass as the current candidate.
func TestProjectRejectsWrongCandidateSource(t *testing.T) {
	store := newStore(t)
	f := twoStepPlan()
	draft := bootstrapPlanDraft(t, store, "t-plan")
	mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, f), assign("t-crit1"))
	crs, _, _ := store.Load()
	wrong := ProjectionFacts{
		CandidatePlan:   f.canonicalPlan(),
		CandidateSource: state.EventRef{TurnID: "t-bogus", Digest: hex.EncodeToString(make([]byte, 32))},
	}
	if _, err := step(t, store, wrong, critiqueArtifact(t, "t-crit1", crs.Revision, "AGREE", false, nil, nil, nil), assign("x")); !errors.Is(err, ErrSemantic) {
		t.Fatalf("wrong source err = %v, want ErrSemantic", err)
	}
}

func TestRequiredID(t *testing.T) {
	gate := &GateSpec{Kind: state.PauseHumanDecision, OriginPhase: state.PhasePlanCritique, ResumePhase: state.PhasePlanCritique}
	// Each route paired with a LEGAL from->next edge (and the gate for RouteGate).
	cases := []struct {
		route Route
		from  state.Phase
		next  state.Phase
		gate  *GateSpec
		want  IDKind
	}{
		{RouteGate, state.PhasePlanCritique, state.PhaseAwaitGuidance, gate, IDGate},
		{RouteDraftAccepted, state.PhasePlanDraft, state.PhasePlanCritique, nil, IDAssignment},
		{RoutePromote, state.PhasePlanCritique, state.PhaseImplementStep, nil, IDAssignment},
		{RouteToRevise, state.PhasePlanCritique, state.PhasePlanRevise, nil, IDAssignment},
		{RouteRetryReview, state.PhasePlanCritique, state.PhasePlanCritique, nil, IDAssignment},
		{RouteReviseAccepted, state.PhasePlanRevise, state.PhasePlanCritique, nil, IDAssignment},
		{RouteToCheckpoint, state.PhaseImplementStep, state.PhaseCheckpoint, nil, IDAssignment},
		{RouteNextStep, state.PhaseCheckpoint, state.PhaseImplementStep, nil, IDAssignment},
		{RouteToFix, state.PhaseCheckpoint, state.PhaseFix, nil, IDAssignment},
	}
	for _, c := range cases {
		got, err := RequiredID(Decision{Route: c.route, FromPhase: c.from, Next: c.next, Gate: c.gate})
		if err != nil {
			t.Fatalf("route %d: unexpected err %v", c.route, err)
		}
		if got != c.want {
			t.Fatalf("route %d: id kind = %d, want %d", c.route, got, c.want)
		}
	}

	// Negatives: an unknown route, an illegal edge, and both gate-presence mismatches.
	if _, err := RequiredID(Decision{Route: Route(999)}); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("unknown route err = %v, want ErrBadDecision", err)
	}
	if _, err := RequiredID(Decision{Route: RouteDraftAccepted, FromPhase: state.PhasePlanCritique, Next: state.PhasePlanCritique}); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("illegal edge err = %v, want ErrBadDecision", err)
	}
	if _, err := RequiredID(Decision{Route: RouteGate, Next: state.PhaseAwaitGuidance, Gate: nil}); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("gate route without a gate err = %v, want ErrBadDecision", err)
	}
	if _, err := RequiredID(Decision{Route: RouteDraftAccepted, FromPhase: state.PhasePlanDraft, Next: state.PhasePlanCritique, Gate: gate}); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("non-gate route with a gate err = %v, want ErrBadDecision", err)
	}
}

// RequiredID agrees with the id every real Evaluate-produced decision issues.
func TestRequiredIDMatchesEvaluate(t *testing.T) {
	cur := state.RunState{Phase: state.PhasePlanDraft, Revision: 5}
	src := state.EventRef{TurnID: "turn-" + hex.EncodeToString(make([]byte, 16)), Digest: hex.EncodeToString(make([]byte, 32))}
	base := Event{Kind: EvPlanDrafted, Source: src, Materialized: hex.EncodeToString(make([]byte, 32)), StepCount: 2}

	accept, err := Evaluate(cur, base)
	if err != nil {
		t.Fatalf("evaluate accept: %v", err)
	}
	if k, err := RequiredID(accept); err != nil || k != IDAssignment {
		t.Fatalf("accept id = %d err=%v, want IDAssignment", k, err)
	}

	gated := base
	gated.Decision = true
	dec, err := Evaluate(cur, gated)
	if err != nil {
		t.Fatalf("evaluate gate: %v", err)
	}
	if k, err := RequiredID(dec); err != nil || k != IDGate {
		t.Fatalf("gate id = %d err=%v, want IDGate", k, err)
	}
}
