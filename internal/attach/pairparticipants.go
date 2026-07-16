package attach

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// canonDigest is a stable digest of a value's JSON encoding (Go sorts map keys,
// and these structs carry no floats), used to freeze and compare an exact
// pre-state so a forged intent or a drifted store fails closed.
func canonDigest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// pairPlanFor reconstructs the deterministic run-scoped pair-attach plan from an
// intent AND proves, before any effect, that it targets the authorized run's
// actual lead/run state — a forged but well-shaped journal cannot fill against a
// different lead or persist an invalid clock. The bound checks are the ones that
// hold whether or not the transaction has been applied (the applied/not-applied
// distinction is the participants' job); the state baseline is checked there.
func pairPlanFor(lay layout, g *genstore.Guard, expectedRunID string, raw txn.Intent) (txn.Plan, error) {
	if raw.Kind != pairIntentKind {
		return txn.Plan{}, fmt.Errorf("attach: intent kind %q is not a pair attach", raw.Kind)
	}
	in, err := decodePairIntent(raw.Payload)
	if err != nil {
		return txn.Plan{}, err
	}
	if raw.TxnID != in.TxnID || raw.ExpectedStateRevision != in.ExpectedStateRevision || in.RunID != expectedRunID {
		return txn.Plan{}, fmt.Errorf("attach: pair envelope/run disagrees with the payload")
	}

	runDir := lay.runDir(state.RunDirRelFor(in.RunID))
	registry := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir))
	runState := state.Open(filepath.Join(runDir, "state"), runLock(runDir))

	// Invariant bindings (hold through the whole transaction): the registry names
	// this run with the exact frozen lead, the pair agent is complementary, and the
	// deadline follows the frozen policy formula.
	reg, ok, err := registry.Load()
	if err != nil {
		return txn.Plan{}, err
	}
	if !ok || reg.RunID != in.RunID || reg.Lead == nil {
		return txn.Plan{}, fmt.Errorf("attach: pair target registry is not the authorized run")
	}
	if ld, derr := canonDigest(reg.Lead); derr != nil {
		return txn.Plan{}, derr
	} else if ld != in.LeadDigest {
		return txn.Plan{}, fmt.Errorf("attach: pair intent lead digest does not match the run")
	}
	if in.PairAgent != complementaryAgent(reg.Lead.Agent) {
		return txn.Plan{}, fmt.Errorf("attach: pair agent is not complementary to the lead")
	}
	rs, ok, err := runState.Load()
	if err != nil {
		return txn.Plan{}, err
	}
	if !ok || rs.RunID != in.RunID {
		return txn.Plan{}, fmt.Errorf("attach: pair target state is not the authorized run")
	}
	if in.DeadlineUnix != in.StartedUnix+rs.EffectivePolicy.Limits.MaxWallSeconds {
		return txn.Plan{}, fmt.Errorf("attach: pair intent deadline does not follow the run's wall cap")
	}

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

// pairFillStep fills the pair slot at generation 1. NotApplied requires the exact
// frozen registry (expected revision, exact frozen lead, pair nil); Applied
// requires the lead byte-identical AND the pair exactly this session at gen 1
// issued at the resulting revision.
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
			leadDigest, derr := canonDigest(reg.Lead)
			if derr != nil {
				return "", derr
			}
			if leadDigest != in.LeadDigest {
				return txn.StatusIndeterminate, nil // the frozen lead changed
			}
			if reg.Pair == nil {
				if reg.Revision == in.ExpectedRegistryRevision {
					return txn.StatusNotApplied, nil
				}
				return txn.StatusIndeterminate, nil
			}
			if pairMatches(reg.Pair, in, reg.Revision) {
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

func pairMatches(slot *state.RoleSlot, in PairAttachIntent, regRevision uint64) bool {
	return slot.Agent == in.PairAgent &&
		slot.CurrentSessionID == in.PairSessionID &&
		len(slot.Sessions) == 1 &&
		slot.Sessions[0].SessionID == in.PairSessionID &&
		slot.Sessions[0].Generation == 1 &&
		slot.Sessions[0].IssuedRegistryRevision == regRevision
}

// planDraftStep advances INIT -> PLAN_DRAFT, issuing the lead's first turn.
// NotApplied is the EXACT frozen INIT baseline (by digest) at the expected
// revision; Applied is MONOTONIC — the first turn is the current PLAN_DRAFT
// assignment with the baseline otherwise byte-identical, OR (crash after this
// effect, before journal progress) the first turn has been accepted downstream
// (an append-only proof), so a downstream Submit advancing the run can never
// brick this recovery.
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
			applied, derr := planDraftApplied(rs, in)
			if derr != nil {
				return "", derr
			}
			if applied {
				return txn.StatusApplied, nil
			}
			baseline, derr := isBaseline(rs, in)
			if derr != nil {
				return "", derr
			}
			if baseline {
				return txn.StatusNotApplied, nil
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

// isBaseline reports whether rs is the exact frozen INIT baseline.
func isBaseline(rs state.RunState, in PairAttachIntent) (bool, error) {
	d, err := canonDigest(rs)
	if err != nil {
		return false, err
	}
	return d == in.BaseStateDigest, nil
}

// planDraftApplied is the monotonic applied proof (see planDraftStep).
func planDraftApplied(rs state.RunState, in PairAttachIntent) (bool, error) {
	if _, accepted := rs.AcceptedTurns[in.FirstTurnID]; accepted {
		return true, nil // append-only proof: the first turn was issued and accepted
	}
	if rs.Phase != state.PhasePlanDraft || rs.Lifecycle != state.LifecycleRunning ||
		rs.Assignment == nil || rs.Assignment.ID != in.FirstTurnID || rs.Assignment.IssuedRevision != rs.Revision ||
		rs.StartedUnix != in.StartedUnix || rs.DeadlineUnix != in.DeadlineUnix {
		return false, nil
	}
	// Every carried field must be the frozen baseline: revert the known changes and
	// require the baseline digest.
	base := rs
	base.Phase = state.PhaseInit
	base.Assignment = nil
	base.StartedUnix = 0
	base.DeadlineUnix = 0
	base.Revision = in.ExpectedStateRevision
	d, err := canonDigest(base)
	if err != nil {
		return false, err
	}
	return d == in.BaseStateDigest, nil
}
