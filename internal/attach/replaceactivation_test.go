package attach

import (
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

const verifierTurn = "turn-" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

// sampleOwnerlessVerify is a coherent running ownerless VERIFY RunState VALUE for the
// pure digest/classify authority tests (it is never persisted, so it need not satisfy
// the full state validator — only canonical digesting).
func sampleOwnerlessVerify(runID string, rev, reqGen uint64) state.RunState {
	sc := 1
	return state.RunState{
		RunID:         runID,
		Revision:      rev,
		Lifecycle:     state.LifecycleRunning,
		Phase:         state.PhaseVerify,
		Verify:        &state.VerifyRequirement{RequiredGeneration: reqGen},
		StepIndex:     &sc,
		AcceptedTurns: map[string]state.AcceptedTurn{"plan-turn": {ArtifactDigest: strings.Repeat("a", 64)}},
	}
}

func sampleActivatedIntent(runID string, stateRev uint64, baseline string) ReplaceIntent {
	return ReplaceIntent{
		RunID: runID, TxnID: "run-" + strings.Repeat("c", 32), OperationID: opID("d"),
		Role: state.SlotPair, Agent: state.AgentCodex,
		SupersededGeneration: 1, NewSessionID: "sess-" + strings.Repeat("7", 32), NewGeneration: 2,
		ExpectedRegistryRevision: 3,
		Activation: &ReplaceActivation{
			ExpectedStateRevision: stateRev, StateBaselineDigest: baseline,
			VerifierTurnID: verifierTurn, RequiredGeneration: 2,
		},
	}
}

func cloneTurns(m map[string]state.AcceptedTurn) map[string]state.AcceptedTurn {
	c := make(map[string]state.AcceptedTurn, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// The activation participant status is exact: NotApplied at the frozen ownerless
// baseline, Applied only for the appended revision + verifier assignment whose REST of
// the state normalizes back to the baseline, and Indeterminate for every collateral
// mutation — so recovery can never bless a drifted ledger/counter/requirement.
func TestClassifyActivation(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	base := sampleOwnerlessVerify(runID, 5, 2)
	baseline, err := canonDigest(base)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	in := sampleActivatedIntent(runID, 5, baseline)
	if err := in.validate(); err != nil {
		t.Fatalf("intent invalid: %v", err)
	}

	if st, _ := classifyActivation(base, true, in); st != txn.StatusNotApplied {
		t.Fatalf("frozen baseline status = %q, want not-applied", st)
	}

	applied := base
	applied.Revision = 6
	applied.Assignment = &state.Ref{ID: verifierTurn, IssuedRevision: 6}
	if st, _ := classifyActivation(applied, true, in); st != txn.StatusApplied {
		t.Fatalf("applied status = %q, want applied", st)
	}

	// A GAP append: genstore.AppendLocked skipped a torn/occupied slot, so a VALID
	// activation binds at expected+2 (revision 7, assignment issued at 7). It is still
	// Applied — recognized by the assignment freshness + the normalized full-state digest,
	// not a fixed +1.
	gap := base
	gap.Revision = 7
	gap.Assignment = &state.Ref{ID: verifierTurn, IssuedRevision: 7}
	if st, _ := classifyActivation(gap, true, in); st != txn.StatusApplied {
		t.Fatalf("gap-append status = %q, want applied", st)
	}

	// Applied-side drift is Indeterminate — including the participant-enforced authority
	// (a wrong phase, a recovery projection, or a mismatched threshold), not only ledger
	// or counter drift.
	appliedCases := map[string]func(rs *state.RunState){
		"unknown run": func(rs *state.RunState) { rs.RunID = "run-" + strings.Repeat("9", 32) },
		"ledger add": func(rs *state.RunState) {
			rs.AcceptedTurns = cloneTurns(rs.AcceptedTurns)
			rs.AcceptedTurns["extra"] = state.AcceptedTurn{ArtifactDigest: strings.Repeat("b", 64)}
		},
		"ledger delete": func(rs *state.RunState) {
			rs.AcceptedTurns = cloneTurns(rs.AcceptedTurns)
			delete(rs.AcceptedTurns, "plan-turn")
		},
		"ledger nil":          func(rs *state.RunState) { rs.AcceptedTurns = nil },
		"unrelated counter":   func(rs *state.RunState) { rs.Counters.PlanRevisions = 1 },
		"requirement changed": func(rs *state.RunState) { rs.Verify = &state.VerifyRequirement{RequiredGeneration: 3} },
		"wrong phase":         func(rs *state.RunState) { rs.Phase = state.PhasePlanDraft },
		"recovery projection": func(rs *state.RunState) {
			rs.Recovery = &state.Projection{Code: "torn", Reason: "torn", NextAction: "recover", AtRevision: rs.Revision}
		},
		"wrong verifier turn": func(rs *state.RunState) {
			rs.Assignment = &state.Ref{ID: "turn-" + strings.Repeat("f", 32), IssuedRevision: 6}
		},
		"stale issued revision": func(rs *state.RunState) { rs.Assignment = &state.Ref{ID: verifierTurn, IssuedRevision: 5} },
		"torn assignment revision": func(rs *state.RunState) {
			rs.Revision = 8 // advanced without rebinding the assignment (still issued at 6)
		},
	}
	for name, mut := range appliedCases {
		t.Run("applied/"+name, func(t *testing.T) {
			rs := applied
			mut(&rs)
			if st, _ := classifyActivation(rs, true, in); st != txn.StatusIndeterminate {
				t.Fatalf("%s status = %q, want indeterminate", name, st)
			}
		})
	}

	// A drifted baseline at the NotApplied revision is Indeterminate, not NotApplied.
	drift := base
	drift.Counters.TestFixes = 1
	if st, _ := classifyActivation(drift, true, in); st != txn.StatusIndeterminate {
		t.Fatalf("drifted baseline status = %q, want indeterminate", st)
	}

	// NotApplied-side authority: the frozen baseline revision with no assignment is
	// NotApplied ONLY when the live state is a clean ownerless VERIFY at the retained
	// threshold. A wrong phase, a recovery projection, or an intent whose threshold does
	// not match the digested state fails closed to Indeterminate — the participant never
	// issues (and never wedges the after-status).
	t.Run("not-applied/wrong phase", func(t *testing.T) {
		wp := base
		wp.Phase = state.PhasePlanDraft
		if st, _ := classifyActivation(wp, true, in); st != txn.StatusIndeterminate {
			t.Fatalf("status = %q, want indeterminate", st)
		}
	})
	t.Run("not-applied/recovery projection", func(t *testing.T) {
		rc := base
		rc.Recovery = &state.Projection{Code: "torn", Reason: "torn", NextAction: "recover", AtRevision: rc.Revision}
		if st, _ := classifyActivation(rc, true, in); st != txn.StatusIndeterminate {
			t.Fatalf("status = %q, want indeterminate", st)
		}
	})
	t.Run("not-applied/lower threshold intent", func(t *testing.T) {
		low := in
		act := *in.Activation
		act.RequiredGeneration = 1 // NewGeneration 2 still validates, but the state's threshold is 2
		low.Activation = &act
		if err := low.validate(); err != nil {
			t.Fatalf("lower-threshold intent should still validate: %v", err)
		}
		if st, _ := classifyActivation(base, true, low); st != txn.StatusIndeterminate {
			t.Fatalf("status = %q, want indeterminate", st)
		}
	})
}

// applyActivation issues the verifier assignment only onto the EXACT frozen baseline,
// binding the assignment to the appended revision and rejecting any drift.
func TestApplyActivation(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	base := sampleOwnerlessVerify(runID, 5, 2)
	baseline, _ := canonDigest(base)
	a := &ReplaceActivation{ExpectedStateRevision: 5, StateBaselineDigest: baseline, VerifierTurnID: verifierTurn, RequiredGeneration: 2}

	next := base
	if err := applyActivation(&next, a, 6); err != nil {
		t.Fatalf("valid activation: %v", err)
	}
	if next.Assignment == nil || next.Assignment.ID != verifierTurn || next.Assignment.IssuedRevision != 6 {
		t.Fatalf("verifier assignment not issued: %+v", next.Assignment)
	}

	drift := base
	drift.Counters.TestFixes = 1
	if err := applyActivation(&drift, a, 6); err == nil {
		t.Fatal("a drifted pre-state must be rejected")
	}
	assigned := base
	assigned.Assignment = &state.Ref{ID: "x", IssuedRevision: 5}
	if err := applyActivation(&assigned, a, 6); err == nil {
		t.Fatal("a non-ownerless pre-state must be rejected")
	}
	// The participant independently enforces the ownerless-VERIFY authority: a wrong phase,
	// a recovery projection, or an intent whose threshold does not match the live state is
	// rejected at apply (never issued, so the after-status can never wedge).
	wrongPhase := base
	wrongPhase.Phase = state.PhasePlanDraft
	if err := applyActivation(&wrongPhase, a, 6); err == nil {
		t.Fatal("a non-VERIFY pre-state must be rejected")
	}
	recovering := base
	recovering.Recovery = &state.Projection{Code: "torn", Reason: "torn", NextAction: "recover", AtRevision: 5}
	if err := applyActivation(&recovering, a, 6); err == nil {
		t.Fatal("a recovering pre-state must be rejected")
	}
	lower := &ReplaceActivation{ExpectedStateRevision: 5, StateBaselineDigest: baseline, VerifierTurnID: verifierTurn, RequiredGeneration: 1}
	clean := base
	if err := applyActivation(&clean, lower, 6); err == nil {
		t.Fatal("an intent whose threshold undercuts the live state must be rejected")
	}
}

// The activation-lineage authority (shared by the same-op retry and the different-op
// step-over) is exact, and specific to THIS activation. STILL-CURRENT reuses the full-state
// classifyActivation == Applied authority (a stale same-id assignment at an earlier revision
// does NOT qualify). ACCEPTED-DESCENDANT requires the verifier ledger entry to be a
// VERIFY-phase acceptance past the frozen revision (not mere map-key presence — a foreign
// accepted id from an older phase does not qualify).
func TestActivationLineageIntact(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	base := sampleOwnerlessVerify(runID, 5, 2)
	baseline, err := canonDigest(base)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	in := sampleActivatedIntent(runID, 5, baseline)

	// STILL CURRENT: the applied activation (assignment bound to the resulting revision, rest
	// normalizes to the baseline) is intact lineage.
	current := base
	current.Revision = 6
	current.Assignment = &state.Ref{ID: verifierTurn, IssuedRevision: 6}
	if !activationLineageIntact(current, true, in) {
		t.Fatal("an applied (still-current) activation is intact lineage")
	}

	// ACCEPTED DESCENDANT: the verifier submitted from VERIFY at a receipt revision past the
	// frozen activation; the turn moved to the ledger and the run advanced.
	accepted := base
	accepted.Phase = state.PhaseDone
	accepted.Revision = 7
	accepted.Assignment = nil
	accepted.AcceptedTurns = cloneTurns(accepted.AcceptedTurns)
	accepted.AcceptedTurns[verifierTurn] = state.AcceptedTurn{
		ArtifactDigest: strings.Repeat("c", 64), Phase: state.PhaseVerify,
		Receipt: state.Receipt{TurnID: verifierTurn, Revision: 7, ArtifactDigest: strings.Repeat("c", 64)},
	}
	if !activationLineageIntact(accepted, true, in) {
		t.Fatal("a VERIFY-phase accepted verifier descendant is intact lineage")
	}

	negatives := map[string]state.RunState{
		"stale issued revision": func() state.RunState {
			rs := base
			rs.Revision = 7
			rs.Assignment = &state.Ref{ID: verifierTurn, IssuedRevision: 6} // a later collateral append left it stale
			return rs
		}(),
		"wrong id": func() state.RunState {
			rs := base
			rs.Revision = 6
			rs.Assignment = &state.Ref{ID: "turn-" + strings.Repeat("f", 32), IssuedRevision: 6}
			return rs
		}(),
		"accepted at wrong phase": func() state.RunState {
			rs := base
			rs.AcceptedTurns = cloneTurns(rs.AcceptedTurns)
			rs.AcceptedTurns[verifierTurn] = state.AcceptedTurn{
				Phase: state.PhasePlanDraft, Receipt: state.Receipt{Revision: 7},
			}
			return rs
		}(),
		"accepted at stale revision": func() state.RunState {
			rs := base
			rs.AcceptedTurns = cloneTurns(rs.AcceptedTurns)
			rs.AcceptedTurns[verifierTurn] = state.AcceptedTurn{
				Phase: state.PhaseVerify, Receipt: state.Receipt{Revision: 5}, // not PAST the frozen revision
			}
			return rs
		}(),
		"wrong run": func() state.RunState {
			rs := sampleOwnerlessVerify("run-"+strings.Repeat("9", 32), 6, 2)
			rs.Assignment = &state.Ref{ID: verifierTurn, IssuedRevision: 6}
			return rs
		}(),
	}
	for name, rs := range negatives {
		t.Run(name, func(t *testing.T) {
			if activationLineageIntact(rs, true, in) {
				t.Fatalf("%s must NOT be intact lineage", name)
			}
		})
	}
	if activationLineageIntact(state.RunState{}, false, in) {
		t.Fatal("a missing state is not intact lineage")
	}
}

// A self-consistent activation intent that names an ALREADY-ACCEPTED turn id (carrying the
// exact baseline digest + threshold) must fail closed at the participant: the shared
// baseline predicate requires the verifier turn to be genuinely fresh, so apply rejects and
// classify never returns NotApplied — recovery cannot issue an assignment reusing an
// accepted id belonging to an older phase.
func TestActivationRejectsAcceptedVerifierID(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	base := sampleOwnerlessVerify(runID, 5, 2) // already carries AcceptedTurns["plan-turn"]
	baseline, err := canonDigest(base)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	in := sampleActivatedIntent(runID, 5, baseline)
	in.Activation.VerifierTurnID = "plan-turn" // a pre-existing accepted id (validRunID accepts it)
	if err := in.validate(); err != nil {
		t.Fatalf("the reuse intent should still validate structurally: %v", err)
	}

	if st, _ := classifyActivation(base, true, in); st != txn.StatusIndeterminate {
		t.Fatalf("accepted-id reuse classify = %q, want indeterminate (not not-applied)", st)
	}
	next := base
	if err := applyActivation(&next, in.Activation, 6); err == nil {
		t.Fatal("apply must reject an activation reusing an accepted verifier id")
	}
	// The same guard rejects a collision with the write-once FirstTurn.
	ft := base
	ft.FirstTurn = &state.Ref{ID: "turn-" + strings.Repeat("1", 32), IssuedRevision: 1}
	ftBaseline, _ := canonDigest(ft)
	ftIntent := sampleActivatedIntent(runID, 5, ftBaseline)
	ftIntent.Activation.VerifierTurnID = ft.FirstTurn.ID
	if st, _ := classifyActivation(ft, true, ftIntent); st != txn.StatusIndeterminate {
		t.Fatalf("first-turn reuse classify = %q, want indeterminate", st)
	}
}

// The activation-readiness decision is pure: a below-threshold pair replacement stays an
// ordinary Registry-only supersession, an at/above-threshold clean waiter activates, and a
// VERIFY waiter that is mid-recovery is BLOCKED (never silently downgraded to ordinary).
func TestClassifyActivationReadiness(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	base := sampleOwnerlessVerify(runID, 5, 2) // threshold 2

	if got := classifyActivationReadiness(base, true, runID, 2); got != activateVerify {
		t.Fatalf("at-threshold readiness = %d, want activateVerify", got)
	}
	if got := classifyActivationReadiness(base, true, runID, 3); got != activateVerify {
		t.Fatalf("above-threshold readiness = %d, want activateVerify", got)
	}
	below := sampleOwnerlessVerify(runID, 5, 3) // threshold 3
	if got := classifyActivationReadiness(below, true, runID, 2); got != activateNone {
		t.Fatalf("below-threshold readiness = %d, want activateNone", got)
	}
	recovering := base
	recovering.Recovery = &state.Projection{Code: "torn", Reason: "torn", NextAction: "recover", AtRevision: 5}
	if got := classifyActivationReadiness(recovering, true, runID, 2); got != activateBlockedRecovery {
		t.Fatalf("recovering readiness = %d, want activateBlockedRecovery", got)
	}
	notVerify := sampleOwnerlessVerify(runID, 5, 2)
	notVerify.Phase = state.PhaseFix
	if got := classifyActivationReadiness(notVerify, true, runID, 2); got != activateNone {
		t.Fatalf("non-VERIFY readiness = %d, want activateNone", got)
	}
	if got := classifyActivationReadiness(base, true, "run-"+strings.Repeat("9", 32), 2); got != activateNone {
		t.Fatalf("wrong-run readiness = %d, want activateNone", got)
	}
	if got := classifyActivationReadiness(state.RunState{}, false, runID, 2); got != activateNone {
		t.Fatalf("missing-state readiness = %d, want activateNone", got)
	}
}

// The activation XOR and threshold are validated in the frozen intent.
func TestReplaceIntentActivationValidation(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	base := sampleActivatedIntent(runID, 5, strings.Repeat("d", 64))
	if err := base.validate(); err != nil {
		t.Fatalf("valid activated intent rejected: %v", err)
	}
	for name, mut := range map[string]func(*ReplaceIntent){
		"activation on lead":  func(in *ReplaceIntent) { in.Role = state.SlotLead },
		"below threshold":     func(in *ReplaceIntent) { in.Activation.RequiredGeneration = 3 }, // NewGeneration 2 < 3
		"zero threshold":      func(in *ReplaceIntent) { in.Activation.RequiredGeneration = 0 },
		"zero state revision": func(in *ReplaceIntent) { in.Activation.ExpectedStateRevision = 0 },
		"bad baseline digest": func(in *ReplaceIntent) { in.Activation.StateBaselineDigest = "not-hex" },
		"bad verifier turn":   func(in *ReplaceIntent) { in.Activation.VerifierTurnID = "turn/nope" }, // '/' is not a valid id char
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			act := *base.Activation
			in.Activation = &act
			mut(&in)
			if err := in.validate(); err == nil {
				t.Fatalf("%s should be rejected", name)
			}
		})
	}
}

// An activation head binds to the two-step plan and the frozen state revision; a forged
// envelope (wrong step order/list, or a zero state revision for an activation payload)
// fails closed.
func TestBindReplaceHeadActivation(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	in := sampleActivatedIntent(runID, 5, strings.Repeat("d", 64))
	valid := txn.Record{
		SchemaVersion: txn.RecordVersion, Revision: 1, Intent: in.txnIntent(),
		StepIDs: []string{stepRegistryReplace, stepVerifyActivate}, StepsDone: 2, Complete: true,
	}
	if got, err := bindReplaceHead(valid, runID); err != nil || !got.activated() {
		t.Fatalf("valid activation head: got=%+v err=%v", got, err)
	}
	for name, mut := range map[string]func(*txn.Record){
		"zero state revision": func(r *txn.Record) { r.Intent.ExpectedStateRevision = 0 },
		"ordinary step list":  func(r *txn.Record) { r.StepIDs = []string{stepRegistryReplace}; r.StepsDone = 1 },
		"reversed step order": func(r *txn.Record) { r.StepIDs = []string{stepVerifyActivate, stepRegistryReplace} },
		"forged extra step":   func(r *txn.Record) { r.StepIDs = []string{stepRegistryReplace, stepVerifyActivate, "extra"} },
	} {
		t.Run(name, func(t *testing.T) {
			rec := valid
			mut(&rec)
			if _, err := bindReplaceHead(rec, runID); err == nil {
				t.Fatalf("%s should be rejected", name)
			}
		})
	}
}
