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
	return []AcceptedArtifact{
		accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan", 1, false, f), 10),
		accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit-act", 1, "REVISE", false, []string{"blocking"}, nil, []map[string]any{addCheck("chk-1", "step one")}), 20),
		accepted(t, state.PhasePlanRevise, revisionArtifact(t, "t-rev", 1, f.digest(t), []string{"finding-a"}), 30),
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
	draft := planArtifact(t, "t-plan", 1, false, f)
	critAct := critiqueArtifact(t, "t-crit-act", 1, "REVISE", false, []string{"blocking"}, nil, []map[string]any{addCheck("chk-1", "step one")})
	revise := revisionArtifact(t, "t-rev", 1, f.digest(t), []string{"finding-a"})

	noisy := []AcceptedArtifact{
		// Human-gated draft: validated, discarded; the run resumes to PLAN_DRAFT.
		accepted(t, state.PhasePlanDraft, planArtifact(t, "t-plan-gated", 1, true, f), 5),
		accepted(t, state.PhasePlanDraft, draft, 10),
		// Human-gated critique carrying a check op that must never leak into the set.
		accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit-gated", 1, "REVISE", true, []string{"blocking"}, nil, []map[string]any{addCheck("chk-ignored", "")}), 12),
		// Reviewer retry (REVISE, only a nit) also carrying a discarded check op.
		accepted(t, state.PhasePlanCritique, critiqueArtifact(t, "t-crit-retry", 1, "REVISE", false, []string{"nit"}, nil, []map[string]any{addCheck("chk-ignored-2", "")}), 14),
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
		// Rebuild the revise answering the wrong finding key.
		h[2] = accepted(t, state.PhasePlanRevise, revisionArtifact(t, "t-rev", 1, f.digest(t), []string{"finding-z"}), 30)
		if _, _, err := MaterializeCandidate(h); !errors.Is(err, ErrSemantic) {
			t.Fatalf("err = %v, want ErrSemantic", err)
		}
	})

	t.Run("too many artifacts", func(t *testing.T) {
		big := make([]AcceptedArtifact, MaxPlanningArtifacts+1)
		if _, _, err := MaterializeCandidate(big); !errors.Is(err, ErrHistoryTooLarge) {
			t.Fatalf("err = %v, want ErrHistoryTooLarge", err)
		}
	})
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
	cases := []struct {
		route Route
		want  IDKind
	}{
		{RouteGate, IDGate},
		{RouteDraftAccepted, IDAssignment},
		{RoutePromote, IDAssignment},
		{RouteToRevise, IDAssignment},
		{RouteRetryReview, IDAssignment},
		{RouteReviseAccepted, IDAssignment},
		{RouteToCheckpoint, IDAssignment},
		{RouteNextStep, IDAssignment},
		{RouteToFix, IDAssignment},
	}
	for _, c := range cases {
		got, err := RequiredID(Decision{Route: c.route})
		if err != nil {
			t.Fatalf("route %d: unexpected err %v", c.route, err)
		}
		if got != c.want {
			t.Fatalf("route %d: id kind = %d, want %d", c.route, got, c.want)
		}
	}
	// An unknown route fails closed.
	if _, err := RequiredID(Decision{Route: Route(999)}); !errors.Is(err, ErrBadDecision) {
		t.Fatalf("unknown route err = %v, want ErrBadDecision", err)
	}
}
