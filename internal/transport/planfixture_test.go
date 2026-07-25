package transport

import (
	"testing"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/state"
)

// dig returns a 64-char lower-hex digest fixture from a single hex character.
func dig(c string) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c[0]
	}
	return string(b)
}

// emptyChecks builds the canonical empty implementation-check set the first
// candidate must carry.
func emptyChecks() *state.CheckSetRef {
	d, err := canonjson.Digest([]byte("[]"))
	if err != nil {
		panic(err)
	}
	return &state.CheckSetRef{Keys: []string{}, Digest: d}
}

// driveAgreedImplement drives a run from its pristine INIT (at initRev) through the
// real plan negotiation — draft, critique, agreement — to a 1-step agreed plan at
// IMPLEMENT_STEP (cursor 0, no assignment). It returns the resulting revision. The
// v5 shape requires an agreed plan to be at any implementation phase, so every
// fixture that lands a run at IMPLEMENT_STEP goes through this.
func driveAgreedImplement(t *testing.T, store *state.Store, initRev uint64) uint64 {
	t.Helper()
	draft := mutate(t, store, initRev, func(rev uint64, n *state.RunState) {
		ref := &state.Ref{ID: "plan-turn", IssuedRevision: rev}
		n.Phase = state.PhasePlanDraft
		n.Assignment = ref
		bindEvidence(n, rev)
		n.FirstTurn = ref
		n.StartedUnix = n.CreatedUnix
		n.DeadlineUnix = n.CreatedUnix + n.EffectivePolicy.Limits.MaxWallSeconds
	})
	critique := mutate(t, store, draft, func(rev uint64, n *state.RunState) {
		n.AcceptedTurns["plan-turn"] = state.AcceptedTurn{
			ArtifactDigest: dig("1"),
			Receipt:        state.Receipt{TurnID: "plan-turn", Revision: rev, ArtifactDigest: dig("1")},
			Phase:          state.PhasePlanDraft,
		}
		n.Assignment = nil
		n.Evidence = nil // the binding is consumed with the turn it authorized
		n.Evidence = nil // consumed with the turn
		n.Phase = state.PhasePlanCritique
		n.CandidatePlan = &state.PlanRef{Source: state.EventRef{Digest: dig("1"), TurnID: "plan-turn"}, Digest: dig("2"), StepCount: 1}
		n.CandidateChecks = emptyChecks()
	})
	critAssigned := mutate(t, store, critique, func(rev uint64, n *state.RunState) {
		n.Assignment = &state.Ref{ID: "crit-turn", IssuedRevision: rev}
		bindEvidence(n, rev)
	})
	return mutate(t, store, critAssigned, func(rev uint64, n *state.RunState) {
		n.AcceptedTurns["crit-turn"] = state.AcceptedTurn{
			ArtifactDigest: dig("3"),
			Receipt:        state.Receipt{TurnID: "crit-turn", Revision: rev, ArtifactDigest: dig("3")},
			Phase:          state.PhasePlanCritique,
		}
		n.Assignment = nil
		n.Evidence = nil // the binding is consumed with the turn it authorized
		n.Evidence = nil // consumed with the turn
		plan := *n.CandidatePlan
		checks := *n.CandidateChecks
		n.AgreedPlan = &state.PlanAgreement{
			Plan:           plan,
			Critique:       state.EventRef{Digest: dig("3"), TurnID: "crit-turn"},
			Checks:         checks,
			AgreedRevision: rev,
		}
		n.CandidatePlan = nil
		n.CandidateChecks = nil
		n.Counters.StepFixes = []int{0}
		idx := 0
		n.StepIndex = &idx
		n.Phase = state.PhaseImplementStep
	})
}

// acceptAndHumanGate accepts the outstanding assigned turnID (recording it at
// originPhase) and raises a human-decision gate resuming to originPhase — the
// shape a direct test mutation needs when it did not go through Submit.
func acceptAndHumanGate(n *state.RunState, gen uint64, originPhase state.Phase, turnID, digest, gateID string) {
	n.AcceptedTurns[turnID] = state.AcceptedTurn{
		ArtifactDigest: digest,
		Receipt:        state.Receipt{TurnID: turnID, Revision: gen, ArtifactDigest: digest},
		Phase:          originPhase,
	}
	humanGate(n, gen, originPhase, turnID, digest, gateID)
}

// humanGate raises a human-decision gate that resumes to originPhase, carrying the
// source event and (when the origin is VERIFY/FIX) the transferred context. It sets
// the full four-way gate shape (pause + gate + AWAIT_GUIDANCE + paused) the v5 state
// requires. sourceTurn is the accepted agent turn that requested the decision.
func humanGate(n *state.RunState, gen uint64, originPhase state.Phase, sourceTurn, sourceDigest, gateID string) {
	pc := &state.PauseContext{
		Kind:        state.PauseHumanDecision,
		OriginPhase: originPhase,
		ResumePhase: originPhase,
		Source:      state.EventRef{Digest: sourceDigest, TurnID: sourceTurn},
	}
	if originPhase == state.PhaseVerify {
		pc.Verify = &state.VerifyRequirement{RequiredGeneration: n.Verify.RequiredGeneration}
		n.Verify = nil
	}
	if originPhase == state.PhaseFix {
		pc.FixReturn = n.FixReturn
		n.FixReturn = ""
	}
	n.Pause = pc
	n.Gate = &state.Ref{ID: gateID, IssuedRevision: gen}
	n.Phase = state.PhaseAwaitGuidance
	n.Lifecycle = state.LifecyclePaused
	n.Assignment = nil
	n.Evidence = nil // the binding is consumed with the turn it authorized
}
