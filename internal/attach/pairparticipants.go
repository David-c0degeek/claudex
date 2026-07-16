package attach

import (
	"fmt"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// pairPlanFor reconstructs the deterministic run-scoped pair-attach plan from an
// intent: registry pair-fill first, then the INIT->PLAN_DRAFT transition that
// issues the lead's first turn. It binds the envelope to the payload so a forged
// record cannot drive it.
func pairPlanFor(lay layout, g *genstore.Guard, raw txn.Intent) (txn.Plan, error) {
	if raw.Kind != pairIntentKind {
		return txn.Plan{}, fmt.Errorf("attach: intent kind %q is not a pair attach", raw.Kind)
	}
	in, err := decodePairIntent(raw.Payload)
	if err != nil {
		return txn.Plan{}, err
	}
	if raw.TxnID != in.TxnID || raw.ExpectedStateRevision != in.ExpectedStateRevision {
		return txn.Plan{}, fmt.Errorf("attach: pair envelope disagrees with the payload")
	}
	runDir := lay.runDir(state.RunDirRelFor(in.RunID))
	registry := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir))
	runState := state.Open(filepath.Join(runDir, "state"), runLock(runDir))

	steps := []txn.Step{
		pairFillStep(registry, g, in),
		planDraftStep(runState, g, in),
	}
	for i := range steps {
		steps[i] = withFailpoint(steps[i])
	}
	return txn.Plan{Intent: raw, Steps: steps}, nil
}

// runLock is the per-run mutation lock (the two-tier protocol: run-scoped work
// uses the run's own lock, not the repo lock).
func runLock(runDir string) string { return filepath.Join(runDir, "run.lock") }

// pairFillStep fills the pair slot at generation 1 with the complementary agent.
// The store's transition rules guarantee the lead slot stays byte-identical and
// the agents are distinct; Observe confirms the pair is exactly this session.
func pairFillStep(store *state.RegistryStore, g *genstore.Guard, in PairAttachIntent) txn.Step {
	return txn.Step{
		Name: "registry-pair-fill",
		Status: func() (txn.StepStatus, error) {
			reg, ok, err := store.Load()
			if err != nil {
				return "", err
			}
			if !ok || reg.RunID != in.RunID || reg.Lead == nil {
				return txn.StatusIndeterminate, nil
			}
			if reg.Pair == nil {
				return txn.StatusNotApplied, nil
			}
			if pairMatches(reg.Pair, in) {
				return txn.StatusApplied, nil
			}
			return txn.StatusIndeterminate, nil
		},
		Apply: func() error {
			_, err := store.MutateLocked(g, in.ExpectedRegistryRevision, func(gen uint64, next *state.Registry) error {
				next.Pair = &state.RoleSlot{
					Agent:            in.PairAgent,
					CurrentSessionID: in.PairSessionID,
					Sessions:         []state.SessionRecord{{SessionID: in.PairSessionID, Generation: 1, IssuedRegistryRevision: gen}},
				}
				return nil
			})
			return err
		},
	}
}

func pairMatches(slot *state.RoleSlot, in PairAttachIntent) bool {
	return slot.Agent == in.PairAgent &&
		slot.CurrentSessionID == in.PairSessionID &&
		len(slot.Sessions) == 1 &&
		slot.Sessions[0].SessionID == in.PairSessionID &&
		slot.Sessions[0].Generation == 1
}

// planDraftStep advances INIT -> PLAN_DRAFT, issuing the lead's first immutable
// turn at the resulting state revision, and freezing the run's start/deadline.
// Observe compares the intended semantic transition; the store's immutability
// rules guarantee every carried-over field is unchanged, and only the
// store-assigned revision (and the turn's issued revision, bound to it) may vary.
func planDraftStep(store *state.Store, g *genstore.Guard, in PairAttachIntent) txn.Step {
	return txn.Step{
		Name: "state-plan-draft",
		Status: func() (txn.StepStatus, error) {
			rs, ok, err := store.Load()
			if err != nil {
				return "", err
			}
			if !ok || rs.RunID != in.RunID {
				return txn.StatusIndeterminate, nil
			}
			if rs.Phase == state.PhaseInit && rs.Assignment == nil {
				return txn.StatusNotApplied, nil
			}
			if planDraftApplied(rs, in) {
				return txn.StatusApplied, nil
			}
			return txn.StatusIndeterminate, nil
		},
		Apply: func() error {
			_, err := store.MutateLocked(g, in.ExpectedStateRevision, func(gen uint64, next *state.RunState) error {
				next.Phase = state.PhasePlanDraft
				next.Assignment = &state.Ref{ID: in.FirstTurnID, IssuedRevision: gen}
				next.StartedUnix = in.StartedUnix
				next.DeadlineUnix = in.DeadlineUnix
				return nil
			})
			return err
		},
	}
}

func planDraftApplied(rs state.RunState, in PairAttachIntent) bool {
	return rs.Phase == state.PhasePlanDraft &&
		rs.Lifecycle == state.LifecycleRunning &&
		rs.Assignment != nil &&
		rs.Assignment.ID == in.FirstTurnID &&
		rs.Assignment.IssuedRevision == rs.Revision &&
		rs.StartedUnix == in.StartedUnix &&
		rs.DeadlineUnix == in.DeadlineUnix &&
		rs.Gate == nil && rs.Recovery == nil && rs.Failure == nil &&
		len(rs.AcceptedTurns) == 0
}
