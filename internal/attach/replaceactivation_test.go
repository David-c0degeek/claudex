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

	cases := map[string]func(rs *state.RunState){
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
		"wrong verifier turn": func(rs *state.RunState) {
			rs.Assignment = &state.Ref{ID: "turn-" + strings.Repeat("f", 32), IssuedRevision: 6}
		},
		"wrong issued revision":  func(rs *state.RunState) { rs.Assignment = &state.Ref{ID: verifierTurn, IssuedRevision: 5} },
		"wrong applied revision": func(rs *state.RunState) { rs.Revision = 8 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
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
