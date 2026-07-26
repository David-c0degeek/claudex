package reviewpacket

import (
	"context"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/state"
)

// The in-manifest logical paths for materialized run ARTIFACTS. Like the frozen inputs they live in
// the reserved namespace, so no task-declared selector can collide with one.
const (
	candidatePlanPath = evidence.ReservedContextPrefix + "candidate-plan.json"
	critiquePath      = evidence.ReservedContextPrefix + "critique.json"
	agreedPlanPath    = evidence.ReservedContextPrefix + "agreed-plan.json"
	implReportPath    = evidence.ReservedContextPrefix + "implementation-report.json"
)

// Pending is the acceptance IN FLIGHT: the turn being accepted by the very transition that is
// issuing the new turn. It is not in the run's accepted history yet — the packet is published before
// the state CAS — but the next turn is usually a RESPONSE to it, so without it a critique would be
// asked to review a plan that state does not yet name, and a checkpoint would review a step whose
// report is not yet accepted.
//
// It carries the phase the artifact was submitted in, the candidate plan the acceptance establishes
// (for a decision that sets one), and the artifact ITSELF — by bytes when the caller already holds
// them, otherwise by identity.
//
// Both forms exist because the two submit paths publish at different points. The ordinary submit
// runs Prepare BEFORE the sink write, so the artifact is not durable yet and only the caller's
// in-hand canonical bytes can supply it. The git transaction publishes during PrepareGitSubmit, so
// by the time its packet is resolved the artifact is already readable by identity.
type Pending struct {
	Phase state.Phase
	Plan  *state.PlanRef
	// Body is the canonical artifact, when the caller holds it. Preferred over Ref.
	Body []byte
	// Ref identifies the artifact in the store, for a caller that has already published it.
	Ref state.EventRef
}

// artifact materializes the pending acceptance's bytes, from the caller's copy when it has one and
// from the store otherwise.
func (p *Pending) artifact(d Deps, what string) ([]byte, error) {
	if len(p.Body) > 0 {
		return p.Body, nil
	}
	return readArtifact(d, p.Ref, what)
}

// candidatePlan materializes the plan a planning turn must review. It is the RESULTING plan
// document, replayed from the accepted history plus the acceptance in flight — never the submit
// artifact, which for a revision is only a patch. The expected digest is the one the acceptance
// establishes when the decision sets a new candidate, otherwise the one already named by state.
func candidatePlan(d Deps, rs state.RunState, pending *Pending) ([]byte, error) {
	var expect string
	switch {
	case pending != nil && pending.Plan != nil:
		expect = pending.Plan.Digest
	case rs.CandidatePlan != nil:
		expect = rs.CandidatePlan.Digest
	default:
		return nil, fmt.Errorf("%w: there is no candidate plan to review", ErrResolve)
	}
	return materializedPlan(d, rs, pending, expect, "", "candidate plan")
}

// latestArtifactFor materializes the most recent artifact submitted in any of these phases,
// preferring the acceptance in flight — which is by definition newer than anything in the ledger.
func latestArtifactFor(d Deps, rs state.RunState, pending *Pending, what string, phases ...state.Phase) ([]byte, error) {
	if pending != nil {
		for _, p := range phases {
			if pending.Phase == p {
				return pending.artifact(d, what)
			}
		}
	}
	var found state.EventRef
	var ok bool
	for _, e := range state.Ledger(rs) { // receipt order; the last match is the newest
		for _, p := range phases {
			if e.Phase == p {
				found, ok = state.EventRef{Digest: e.ArtifactDigest, TurnID: e.TurnID}, true
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("%w: the run has no accepted %s", ErrResolve, what)
	}
	return readArtifact(d, found, what)
}

// ArtifactReader reads an ACCEPTED artifact's canonical bytes by its exact (turn, digest) identity.
// It is an interface rather than the concrete store so this package stays independent of transport;
// the production store satisfies it as-is, and the digest is re-verified by the store on read, so a
// corrupted artifact can never become packet content.
type ArtifactReader interface {
	Get(turnID, digest string) ([]byte, error)
}

// phaseEntries selects what a turn in this phase must actually see. The selection is PHASE-SPECIFIC
// by design, not a common core with extras: a plan reviewer and a checkpoint reviewer are being asked
// different questions, and a packet that answered both would be larger, slower, and vaguer about what
// the turn is for.
//
//   - PLAN_*: the frozen task and policy, the task-declared repository paths, and the plan context
//     for this particular turn (the candidate under critique, plus the critique being answered).
//   - CHECKPOINT: what the accepted implementation step CHANGED (parent -> commit), the agreed plan it
//     was meant to implement, and the report the lead submitted about it.
//   - VERIFY: the cumulative change from BaseCommit to the latest accepted commit, plus the task and
//     the agreed plan the whole run is judged against.
func phaseEntries(ctx context.Context, d Deps, rs state.RunState, phase state.Phase, src evidence.SourceObject, pending *Pending) ([]evidence.RecipeEntry, error) {
	switch phase {
	case state.PhasePlanDraft, state.PhasePlanCritique, state.PhasePlanRevise:
		return planEntries(ctx, d, rs, phase, src, pending)
	case state.PhaseCheckpoint:
		return checkpointEntries(ctx, d, rs, src, pending)
	case state.PhaseVerify:
		return verifyEntries(ctx, d, rs, src)
	default:
		return nil, fmt.Errorf("%w: %s issues no reviewable turn", ErrResolve, phase)
	}
}

// planEntries builds the plan-negotiation packet: what the run is trying to achieve, the repository
// content the task said matters, and the plan artifacts this turn is responding to.
func planEntries(ctx context.Context, d Deps, rs state.RunState, phase state.Phase, src evidence.SourceObject, pending *Pending) ([]evidence.RecipeEntry, error) {
	taskBytes, err := readSnapshot(d.RunDir, rs.TaskSnapshot, "task")
	if err != nil {
		return nil, err
	}
	policyBytes, err := readSnapshot(d.RunDir, rs.PolicySnapshot, "policy")
	if err != nil {
		return nil, err
	}
	tc, err := config.ParseTaskContract(taskBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: frozen task snapshot: %v", ErrResolve, err)
	}
	entries := []evidence.RecipeEntry{
		inlineEntry(taskContextPath, taskBytes),
		inlineEntry(policyContextPath, policyBytes),
	}
	// The task-declared repository selection, read from the PROVEN source object. A selector the
	// source tree does not contain fails closed: silently dropping it would hand the reviewer a packet
	// quietly missing what the task said mattered.
	for _, sel := range tc.RelevantRepoPaths {
		oid, mode, rerr := resolveBlob(ctx, d, src.Commit, sel)
		if rerr != nil {
			return nil, rerr
		}
		entries = append(entries, evidence.RecipeEntry{
			GitPath: sel, Mode: mode, Kind: evidence.EntryFile,
			Source: evidence.BlobSource{CommitBlobOID: oid},
		})
	}
	// PLAN_DRAFT is the first turn: there is no plan yet, so there is nothing further to show. Every
	// later planning turn is a RESPONSE, so it must carry what it is responding to.
	if phase == state.PhasePlanDraft {
		return entries, nil
	}
	plan, err := candidatePlan(d, rs, pending)
	if err != nil {
		return nil, err
	}
	entries = append(entries, inlineEntry(candidatePlanPath, plan))
	// A revision answers a critique, so the critique it must address is part of the turn. A critique
	// turn has no such predecessor.
	if phase == state.PhasePlanRevise {
		b, rerr := latestArtifactFor(d, rs, pending, "critique", state.PhasePlanCritique)
		if rerr != nil {
			return nil, rerr
		}
		entries = append(entries, inlineEntry(critiquePath, b))
	}
	return entries, nil
}

// checkpointEntries builds the step-review packet: the CHANGE the accepted step made, the agreed plan
// it was implementing, and the lead's report on it. The whole tree is deliberately absent — a
// checkpoint reviewer is asked about a step, and handing them everything would bury it.
func checkpointEntries(ctx context.Context, d Deps, rs state.RunState, src evidence.SourceObject, pending *Pending) ([]evidence.RecipeEntry, error) {
	if rs.AgreedPlan == nil {
		return nil, fmt.Errorf("%w: CHECKPOINT has no agreed plan", ErrResolve)
	}
	// The agreed plan is materialized too: when agreement came from a PLAN_REVISE, its source is a
	// patch, and the frozen digest is of the resulting document — which is what the replay must
	// reproduce.
	plan, err := materializedPlan(d, rs, pending, rs.AgreedPlan.Plan.Digest, rs.AgreedPlan.Critique.TurnID, "agreed plan")
	if err != nil {
		return nil, err
	}
	reportBytes, err := latestArtifactFor(d, rs, pending, "implementation report", state.PhaseImplementStep, state.PhaseFix)
	if err != nil {
		return nil, err
	}
	// The change is measured from the step's own parent, so the packet shows exactly what THIS step
	// did — not everything since the run began, which is the VERIFY question.
	parent, err := checkpointParent(rs, src)
	if err != nil {
		return nil, err
	}
	changed, err := changedEntries(ctx, d, parent, src.Commit)
	if err != nil {
		return nil, err
	}
	entries := []evidence.RecipeEntry{
		inlineEntry(agreedPlanPath, plan),
		inlineEntry(implReportPath, reportBytes),
	}
	return append(entries, changed...), nil
}

// verifyEntries builds the whole-run packet: everything the run changed end to end, judged against
// the frozen task and the agreed plan.
func verifyEntries(ctx context.Context, d Deps, rs state.RunState, src evidence.SourceObject) ([]evidence.RecipeEntry, error) {
	taskBytes, err := readSnapshot(d.RunDir, rs.TaskSnapshot, "task")
	if err != nil {
		return nil, err
	}
	if rs.AgreedPlan == nil {
		return nil, fmt.Errorf("%w: VERIFY has no agreed plan", ErrResolve)
	}
	plan, err := materializedPlan(d, rs, nil, rs.AgreedPlan.Plan.Digest, rs.AgreedPlan.Critique.TurnID, "agreed plan")
	if err != nil {
		return nil, err
	}
	if rs.BaseCommit == "" {
		return nil, fmt.Errorf("%w: run has no base commit to measure the cumulative change from", ErrResolve)
	}
	changed, err := changedEntries(ctx, d, rs.BaseCommit, src.Commit)
	if err != nil {
		return nil, err
	}
	entries := []evidence.RecipeEntry{
		inlineEntry(taskContextPath, taskBytes),
		inlineEntry(agreedPlanPath, plan),
	}
	return append(entries, changed...), nil
}

// checkpointParent is the commit the reviewed step started from: the parent recorded in the step's
// own git evidence when the acceptance is already durable, and otherwise the run's latest accepted
// commit — which is where the transaction that is issuing this turn began.
func checkpointParent(rs state.RunState, src evidence.SourceObject) (string, error) {
	for _, e := range state.Ledger(rs) {
		at := rs.AcceptedTurns[e.TurnID]
		if at.GitCommit != nil && at.GitCommit.Commit == src.Commit {
			return at.GitCommit.Parent, nil
		}
	}
	if latest, ok := state.LatestGitCommit(rs); ok {
		return latest.Commit, nil
	}
	if rs.BaseCommit == "" {
		return "", fmt.Errorf("%w: CHECKPOINT has no parent commit to measure the step against", ErrResolve)
	}
	return rs.BaseCommit, nil
}

// readArtifact materializes an accepted artifact by its exact (turn, digest) identity. The store
// re-verifies the digest on read, so a corrupted or substituted artifact fails closed here rather
// than becoming packet content.
func readArtifact(d Deps, ref state.EventRef, what string) ([]byte, error) {
	if d.Artifacts == nil {
		return nil, fmt.Errorf("%w: resolving a %s requires an artifact reader", ErrResolve, what)
	}
	if !state.IsRunID(ref.TurnID) || !state.IsHex64(ref.Digest) {
		return nil, fmt.Errorf("%w: the %s reference is not a canonical (turn, digest)", ErrResolve, what)
	}
	b, err := d.Artifacts.Get(ref.TurnID, ref.Digest)
	if err != nil {
		return nil, fmt.Errorf("%w: %s artifact: %v", ErrResolve, what, err)
	}
	return b, nil
}

// inlineEntry records already-durable run content (a frozen input or an accepted artifact) whose
// bytes are in hand, as opposed to a repository blob read from the object store.
func inlineEntry(path string, body []byte) evidence.RecipeEntry {
	return evidence.RecipeEntry{
		GitPath: path, Mode: contextMode, Kind: evidence.EntryFile,
		Source: evidence.BlobSource{Inline: body},
	}
}
