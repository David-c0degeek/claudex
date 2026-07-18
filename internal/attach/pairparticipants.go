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
	// Clock/wall-cap coherence against the FROZEN run, so an invalid time can never
	// be committed (leaving a permanently-rejected transaction). Overflow-safe.
	if in.StartedUnix < rs.CreatedUnix {
		return txn.Plan{}, fmt.Errorf("attach: pair intent started_unix precedes the run's creation")
	}
	if in.DeadlineUnix <= in.StartedUnix || in.DeadlineUnix-in.StartedUnix != rs.EffectivePolicy.Limits.MaxWallSeconds {
		return txn.Plan{}, fmt.Errorf("attach: pair intent deadline does not follow the run's wall cap")
	}
	// The state must be EITHER the exact frozen INIT baseline OR a valid applied
	// descendant BEFORE any effect, so a wrong BaseStateDigest can never let
	// registry-pair-fill commit and then poison state-plan-draft on recovery.
	baseline, err := isBaseline(rs, in)
	if err != nil {
		return txn.Plan{}, err
	}
	if !baseline {
		applied, aerr := planDraftApplied(rs, in)
		if aerr != nil {
			return txn.Plan{}, aerr
		}
		if !applied {
			return txn.Plan{}, fmt.Errorf("attach: run state is neither the frozen baseline nor a valid applied descendant")
		}
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
		ConfirmDurable: func() error { return store.ConfirmDurable(g) },
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

// planDraftStep advances INIT -> PLAN_DRAFT, issuing the lead's first turn as both
// the mutable Assignment and the write-once FirstTurn record. NotApplied is the
// EXACT frozen INIT baseline (by digest); Applied is DURABLY MONOTONIC (see
// planDraftApplied): the write-once FirstTurn proves THIS turn was issued and the
// normalized baseline proves the immutable remainder is intact, so the observation
// survives ANY downstream RunState transition — Submit, cancel, or operator action
// — and none of them need to know about this attach journal.
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
				next.FirstTurn = &state.Ref{ID: in.FirstTurnID, IssuedRevision: gen} // write-once issuance proof
				next.StartedUnix = in.StartedUnix
				next.DeadlineUnix = in.DeadlineUnix
				return nil
			})
			return err
		},
		ConfirmDurable: func() error { return store.ConfirmDurable(g) },
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

// planDraftApplied is the DURABLE, monotonic applied proof. RunState.FirstTurn is
// a write-once record of which turn this plan-draft issued: it is set only by the
// exact INIT->PLAN_DRAFT transition (with Assignment and the clock, enforced by
// state), and can never change or be cleared — so it proves THIS FirstTurnID was
// issued even after cancel/Submit/operator actions have moved the mutable
// Assignment on. Combined with the frozen clock and the normalized baseline digest
// (which proves the immutable remainder is intact), the observation is monotonic
// through every downstream RunState transition.
func planDraftApplied(rs state.RunState, in PairAttachIntent) (bool, error) {
	if rs.FirstTurn == nil || rs.FirstTurn.ID != in.FirstTurnID ||
		rs.FirstTurn.IssuedRevision <= in.ExpectedStateRevision || rs.FirstTurn.IssuedRevision > rs.Revision {
		return false, nil // issuance is after the frozen baseline and at/before now
	}
	if rs.StartedUnix != in.StartedUnix || rs.DeadlineUnix != in.DeadlineUnix {
		return false, nil
	}
	d, err := canonDigest(normalizeToBaseline(rs, in))
	if err != nil {
		return false, err
	}
	return d == in.BaseStateDigest, nil
}

// normalizeToBaseline resets every field a legitimate downstream transition may
// change back to the pristine INIT baseline value, so its digest can be compared
// to the frozen BaseStateDigest to prove the immutable remainder is intact.
func normalizeToBaseline(rs state.RunState, in PairAttachIntent) state.RunState {
	rs.Revision = in.ExpectedStateRevision
	rs.Lifecycle = state.LifecycleRunning
	rs.Phase = state.PhaseInit
	rs.Assignment = nil
	rs.FirstTurn = nil
	rs.Gate = nil
	rs.Recovery = nil
	rs.Failure = nil
	rs.StartedUnix = 0
	rs.DeadlineUnix = 0
	rs.PendingTxnID = ""
	rs.Counters = state.Counters{StepFixes: []int{}}
	rs.AcceptedTurns = map[string]state.AcceptedTurn{}
	// The v5 phase-engine working set is empty at the pristine baseline; a
	// downstream transition (plan negotiation onward) introduces it, so strip it
	// before comparing the immutable remainder.
	rs.CandidatePlan = nil
	rs.CandidateChecks = nil
	rs.PendingFindings = nil
	rs.AgreedPlan = nil
	rs.StepIndex = nil
	rs.FixReturn = ""
	rs.Verify = nil
	rs.Pause = nil
	return rs
}
