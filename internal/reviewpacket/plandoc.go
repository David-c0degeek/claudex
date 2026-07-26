package reviewpacket

import (
	"fmt"
	"sort"

	"github.com/David-c0degeek/claudex/internal/engine"
	"github.com/David-c0degeek/claudex/internal/state"
)

// materializedPlan returns the canonical bytes of the plan a review turn must actually see: the
// RESULTING plan document, not the submit artifact that produced it.
//
// The two are not the same thing. A plan_revision is a PATCH — it may leave sections null and
// inherit them from the base — so handing a reviewer the revision artifact shows them the delta and
// the responses, never the plan those responses produced. Run state says as much: PlanRef.Digest is
// the MATERIALIZED plan-document digest, deliberately distinct from Source.Digest, the submit
// artifact.
//
// The plan is therefore reconstructed by replaying the accepted planning history through
// engine.MaterializeCandidate — the ONE replay authority, the same fold the live projector and the
// coordinator's fact loader use — plus the acceptance in flight, which is not in the history yet.
// The result is checked against the digest the caller expects, so these bytes cannot silently be a
// different plan from the one state named.
//
// excludeTurn omits one accepted turn from the replay. It exists for the AGREED plan: that fold
// reconstructs a PRE-agreement candidate and refuses a history containing the critique that
// converged, so the agreed document is reproduced by replaying everything up to that critique. The
// digest check then proves the reconstruction really is the plan the run agreed to.
func materializedPlan(d Deps, rs state.RunState, pending *Pending, expectDigest, excludeTurn, what string) ([]byte, error) {
	arts, err := planningHistory(d, rs, pending, excludeTurn)
	if err != nil {
		return nil, err
	}
	facts, refs, err := engine.MaterializeCandidate(arts)
	if err != nil {
		return nil, fmt.Errorf("%w: replaying the planning history for the %s: %v", ErrResolve, what, err)
	}
	if expectDigest != "" && refs.PlanDigest != expectDigest {
		return nil, fmt.Errorf("%w: the replayed %s does not match the digest run state froze", ErrResolve, what)
	}
	body, err := engine.MarshalPlanDocument(facts.CandidatePlan)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding the %s: %v", ErrResolve, what, err)
	}
	return body, nil
}

// planningHistory loads every accepted planning artifact in receipt order, then appends the
// acceptance in flight when it is itself a planning turn. Bounds are enforced before and during the
// load, so a reconstruction can never scan an unbounded history.
func planningHistory(d Deps, rs state.RunState, pending *Pending, excludeTurn string) ([]engine.AcceptedArtifact, error) {
	type ref struct {
		turnID, digest string
		phase          state.Phase
		receipt        uint64
	}
	var refs []ref
	for tid, acc := range rs.AcceptedTurns {
		if !isPlanningPhase(acc.Phase) || tid == excludeTurn {
			continue
		}
		refs = append(refs, ref{tid, acc.ArtifactDigest, acc.Phase, acc.Receipt.Revision})
	}
	if len(refs) > engine.MaxPlanningArtifacts {
		return nil, fmt.Errorf("%w: %d planning artifacts", engine.ErrHistoryTooLarge, len(refs))
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].receipt < refs[j].receipt })

	arts := make([]engine.AcceptedArtifact, 0, len(refs)+1)
	total := 0
	add := func(turnID, digest string, phase state.Phase, receipt uint64, canonical []byte) error {
		if len(canonical) > engine.MaxMaterializeBytes || total > engine.MaxMaterializeBytes-len(canonical) {
			return fmt.Errorf("%w: over %d bytes", engine.ErrHistoryTooLarge, engine.MaxMaterializeBytes)
		}
		total += len(canonical)
		arts = append(arts, engine.AcceptedArtifact{
			TurnID: turnID, Digest: digest, Phase: phase, ReceiptRevision: receipt, Canonical: canonical,
		})
		return nil
	}
	for _, r := range refs {
		canonical, gerr := readArtifact(d, state.EventRef{Digest: r.digest, TurnID: r.turnID}, "planning artifact")
		if gerr != nil {
			return nil, gerr
		}
		if aerr := add(r.turnID, r.digest, r.phase, r.receipt, canonical); aerr != nil {
			return nil, aerr
		}
	}
	// The acceptance in flight is newer than everything in the ledger by definition: it is being
	// accepted by the very transition that is issuing this turn. Its receipt is that transition's
	// resulting revision, which is what makes the fold's [prevReceipt, thisReceipt) window contain
	// the revision the artifact declares it was submitted against.
	if pending != nil && isPlanningPhase(pending.Phase) {
		body, berr := pending.artifact(d, "planning artifact")
		if berr != nil {
			return nil, berr
		}
		if aerr := add(pending.Ref.TurnID, pending.Ref.Digest, pending.Phase, rs.Revision+1, body); aerr != nil {
			return nil, aerr
		}
	}
	return arts, nil
}

func isPlanningPhase(p state.Phase) bool {
	return p == state.PhasePlanDraft || p == state.PhasePlanCritique || p == state.PhasePlanRevise
}
