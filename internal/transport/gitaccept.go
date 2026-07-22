package transport

import (
	"context"
	"errors"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/engine"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

// ErrGitParticipantRequired means an IMPLEMENT_STEP/FIX submit reached the standalone accept path,
// which has no git transaction participant to produce the required commit evidence. The
// coordinator routes those phases through the git transaction; a direct low-level call fails closed.
var ErrGitParticipantRequired = errors.New("transport: an implementation submit requires the git transaction")

// GitAcceptPlan is the bounded, serializable acceptance plan the git transaction freezes in its
// journal intent — everything the state-cas step needs to rebuild the acceptance WITHOUT the
// prepared-transition closure, the artifact bytes, or the whole run state. The state-cas step
// re-applies the frozen engine decision and records the accepted turn WITH its git-commit evidence.
type GitAcceptPlan struct {
	RunID                 string                  `json:"run_id"`
	ExpectedStateRevision uint64                  `json:"expected_state_revision"`
	TurnID                string                  `json:"turn_id"`
	Digest                string                  `json:"digest"`
	Phase                 state.Phase             `json:"phase"`
	Decision              engine.Decision         `json:"decision"`
	IssuedTurnID          string                  `json:"issued_turn_id"`
	IssuedGateID          string                  `json:"issued_gate_id"`
	CurrentPairGeneration uint64                  `json:"current_pair_generation"`
	GitCommit             state.GitCommitEvidence `json:"git_commit"`
	// IndexPreDigest/IndexTargetDigest freeze the checked-out index identities the
	// transaction's index-cas step swaps between: I0 (the real index at snapshot time)
	// and the deterministic target built from the frozen commit. The driver fills them
	// with GitCommit after the snapshot; PrepareGitSubmit leaves them empty.
	IndexPreDigest    string `json:"index_pre_digest"`
	IndexTargetDigest string `json:"index_target_digest"`
}

// GitAcceptState classifies the run against a frozen accept plan for the state-cas step.
type GitAcceptState int

const (
	// GitAcceptNotApplied: the run is at the expected pre-revision with the plan's turn assigned
	// and unaccepted — the acceptance has not run.
	GitAcceptNotApplied GitAcceptState = iota
	// GitAcceptApplied: the turn is already accepted with the exact digest and git evidence.
	GitAcceptApplied
	// GitAcceptForeign: any other shape — fail closed.
	GitAcceptForeign
)

// ObserveGitAccept classifies the current run state against a frozen accept plan: applied if the
// turn is already accepted with the exact digest and git-commit evidence; not-applied if the run is
// at the expected pre-revision with the plan's turn assigned and unaccepted; foreign otherwise.
func ObserveGitAccept(rs state.RunState, plan GitAcceptPlan) GitAcceptState {
	if acc, ok := rs.AcceptedTurns[plan.TurnID]; ok {
		if acc.ArtifactDigest == plan.Digest && acc.GitCommit != nil && *acc.GitCommit == plan.GitCommit {
			return GitAcceptApplied
		}
		return GitAcceptForeign
	}
	if rs.Revision == plan.ExpectedStateRevision &&
		rs.Lifecycle == state.LifecycleRunning && rs.Recovery == nil && rs.Gate == nil &&
		rs.Assignment != nil && rs.Assignment.ID == plan.TurnID && rs.Assignment.IssuedRevision == rs.Revision {
		return GitAcceptNotApplied
	}
	return GitAcceptForeign
}

// PrepareGitSubmit runs the shared authorize -> prepare -> publish pipeline for an
// IMPLEMENT_STEP/FIX submit under the CALLER's held run guard (the coordinator's git
// transaction driver), stopping after the artifact publish — the acceptance itself is
// the transaction's state-cas step, applied later from the returned plan. It NEVER
// releases g; the driver owns the guard for the whole transaction. An accept-once
// replay returns the durable receipt as a non-nil SubmitResult (with a zero plan); the
// caller returns it without starting a transaction. The returned plan's git and index
// identities are empty — the driver freezes them after the snapshot.
func PrepareGitSubmit(ctx context.Context, deps SubmitDeps, g *genstore.Guard, sessionID string, raw []byte) (GitAcceptPlan, *SubmitResult, error) {
	if err := deps.validate(); err != nil {
		return GitAcceptPlan{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return GitAcceptPlan{}, nil, err
	}
	n, err := Normalize(raw)
	if err != nil {
		return GitAcceptPlan{}, nil, err
	}
	canonRedacted := []byte(n.CanonicalRedacted)
	digest := n.Digest
	env := n.envelope()

	// The same authorization pipeline the standalone path runs; the journal-head check
	// inside proves g is the run's live guard before any registry authority.
	auth, replay, aerr := authorizeLockedSubmit(deps, g, sessionID, digest, env)
	if aerr != nil {
		return GitAcceptPlan{}, nil, aerr
	}
	if replay != nil {
		res, cerr := confirmReplay(deps, g, *replay, env.TurnID, canonRedacted)
		if cerr != nil {
			if res.Idempotent {
				return GitAcceptPlan{}, &res, cerr
			}
			return GitAcceptPlan{}, nil, cerr
		}
		return GitAcceptPlan{}, &res, nil
	}
	if !gitEvidencePhase(auth.rs.Phase) {
		return GitAcceptPlan{}, nil, fmt.Errorf("%w: phase %s does not take the git transaction", ErrPhaseNotActionable, auth.rs.Phase)
	}

	pt, perr := preparePublish(ctx, deps, auth, raw, canonRedacted, digest, env)
	if perr != nil {
		return GitAcceptPlan{}, nil, perr
	}
	dec, ok := pt.Decision()
	if !ok {
		return GitAcceptPlan{}, nil, fmt.Errorf("%w: the prepared transition carries no engine decision", ErrTransitionInvalid)
	}
	return GitAcceptPlan{
		RunID:                 auth.rs.RunID,
		ExpectedStateRevision: auth.rs.Revision,
		TurnID:                env.TurnID,
		Digest:                digest,
		Phase:                 auth.rs.Phase,
		Decision:              dec,
		IssuedTurnID:          pt.IssuedTurnID(),
		IssuedGateID:          pt.IssuedGateID(),
		CurrentPairGeneration: auth.curPairGen,
	}, nil, nil
}

// FinalizeGitAccept applies the frozen decision and records the accepted turn with its git-commit
// evidence, inside a state CAS the git transaction's state-cas step drives. rs is the pre-state (at
// the expected revision); next is the generation being built (gen). It reuses the same live-owner,
// issued-id, and VERIFY-threshold guards the standalone accept enforces.
func FinalizeGitAccept(rs state.RunState, gen uint64, next *state.RunState, plan GitAcceptPlan) error {
	if err := engine.Apply(plan.Decision, plan.Decision.Source, engine.Ids{AssignmentTurnID: plan.IssuedTurnID, GateID: plan.IssuedGateID}, gen, next); err != nil {
		return err
	}
	if next.AcceptedTurns == nil {
		return fmt.Errorf("%w: the transition cleared the accepted-turns map", ErrTransitionInvalid)
	}
	if err := requireLiveOwner(next, plan.TurnID, gen); err != nil {
		return err
	}
	pt := NewPreparedTransition(plan.IssuedTurnID, plan.IssuedGateID, nil)
	if err := bindIssued(pt, next); err != nil {
		return err
	}
	if err := checkEnterVerifyThreshold(rs, next, plan.CurrentPairGeneration); err != nil {
		return err
	}
	gc := plan.GitCommit
	next.AcceptedTurns[plan.TurnID] = state.AcceptedTurn{
		ArtifactDigest: plan.Digest,
		Receipt:        state.Receipt{TurnID: plan.TurnID, Revision: gen, ArtifactDigest: plan.Digest},
		Phase:          plan.Phase,
		GitCommit:      &gc,
	}
	return nil
}
