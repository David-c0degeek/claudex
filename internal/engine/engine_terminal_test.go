package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/protocol"
	"github.com/David-c0degeek/claudex/internal/state"
)

// --- terminal-graph fixtures + drivers ---

var acceptanceCriteria = []string{"criterion one", "criterion two"}

func oneStepPlan() planFixture {
	return planFixture{
		Markdown: "architecture prose",
		Steps:    []PlanStep{{Title: "only step", Description: "do it", Files: []string{"a.go"}, Tests: []string{"a_test.go"}}},
		Risks:    []string{},
		OpenQ:    []string{},
	}
}

// reachTests drives a one-step plan until the final CHECKPOINT AGREE has entered
// ownerless TESTS, using the real agent edges.
func reachTests(t *testing.T, store *state.Store) state.RunState {
	t.Helper()
	f := oneStepPlan()
	draft := bootstrapPlanDraft(t, store, "t-plan")
	rs := mustStep(t, store, ProjectionFacts{}, planArtifact(t, "t-plan", draft.Revision, false, f), assign("t-crit"))
	rs = mustStep(t, store, ProjectionFacts{CandidatePlan: f.canonicalPlan()}, critiqueArtifact(t, "t-crit", rs.Revision, "AGREE", false, nil, nil, nil), assign("t-impl"))
	rs = mustStep(t, store, ProjectionFacts{}, implReport(t, "t-impl", rs.Revision, false), assign("t-chk"))
	rs = mustStep(t, store, ProjectionFacts{}, checkpointArtifact(t, "t-chk", rs.Revision, "AGREE", false, true, nil, nil), Ids{})
	if rs.Phase != state.PhaseTests {
		t.Fatalf("reachTests: phase = %s, want TESTS", rs.Phase)
	}
	return rs
}

// taskFacts are the frozen task facts the loader supplies for VERIFY, bound to the
// bootstrap task-snapshot digest.
func taskFacts() TaskFacts {
	return TaskFacts{Digest: strings.Repeat("a", 64), AcceptanceCriteria: acceptanceCriteria}
}

// applyTests drives a coordinator-authored TESTS outcome (Evaluate -> Apply) with an
// ownerless empty-turn evidence source; ids carry the fix assignment or gate id when
// the route issues one.
func applyTests(t *testing.T, store *state.Store, pass bool, rt RuntimeFacts, ids Ids) (state.RunState, error) {
	t.Helper()
	cur, _, _ := store.Load()
	ev := Event{Kind: EvTestsOutcome, Source: state.EventRef{Digest: strings.Repeat("e", 64)}, Pass: pass}
	dec, err := Evaluate(cur, ev, rt)
	if err != nil {
		return state.RunState{}, err
	}
	return store.Mutate(cur.Revision, func(gen uint64, next *state.RunState) error {
		return Apply(dec, ev.Source, ids, gen, next) // ownerless: no AcceptedTurn
	})
}

// issueVerifier simulates the replacement issuing the verifier turn at ownerless VERIFY.
func issueVerifier(t *testing.T, store *state.Store, turnID string) state.RunState {
	t.Helper()
	cur, _, _ := store.Load()
	next, err := store.Mutate(cur.Revision, func(gen uint64, n *state.RunState) error {
		n.Assignment = &state.Ref{ID: turnID, IssuedRevision: gen}
		return nil
	})
	if err != nil {
		t.Fatalf("issue verifier: %v", err)
	}
	return next
}

// stepRT drives one agent submit through Project->Evaluate(rt)->Apply, validating the
// artifact against msgType and recording the accepted turn.
func stepRT(t *testing.T, store *state.Store, facts ProjectionFacts, rt RuntimeFacts, msgType string, canonical []byte, ids Ids) (state.RunState, error) {
	t.Helper()
	cur, _, _ := store.Load()
	canon, err := protocol.Validate(msgType, canonical)
	if err != nil {
		return state.RunState{}, err
	}
	ev, err := Project(cur, canon, facts)
	if err != nil {
		return state.RunState{}, err
	}
	dec, err := Evaluate(cur, ev, rt)
	if err != nil {
		return state.RunState{}, err
	}
	return store.Mutate(cur.Revision, func(gen uint64, next *state.RunState) error {
		if aerr := Apply(dec, ev.Source, ids, gen, next); aerr != nil {
			return aerr
		}
		next.AcceptedTurns[ev.Source.TurnID] = state.AcceptedTurn{
			ArtifactDigest: ev.Source.Digest,
			Receipt:        state.Receipt{TurnID: ev.Source.TurnID, Revision: gen, ArtifactDigest: ev.Source.Digest},
			Phase:          cur.Phase,
		}
		return nil
	})
}

func verificationArtifact(t *testing.T, turnID string, rev uint64, verdict string, rhd bool, criteria []string, met, testsMeaningful bool, scope, unsupported []string) []byte {
	crit := make([]any, len(criteria))
	for i, c := range criteria {
		crit[i] = map[string]any{"criterion": c, "met": met, "evidence": "e " + c}
	}
	return mustJSON(t, map[string]any{
		"protocol_version": 1, "message_type": "verification", "turn_id": turnID, "state_revision": rev,
		"human_context": nil, "requires_human_decision": rhd, "decision_question": decisionQuestion(rhd),
		"verdict": verdict, "criteria": crit, "scope_expansion": arr(scope),
		"tests_meaningful": testsMeaningful, "unsupported_claims": arr(unsupported), "notes": "n",
	})
}

func fixReport(t *testing.T, store *state.Store, turnID string) []byte {
	cur, _, _ := store.Load()
	return implReport(t, turnID, cur.Revision, false)
}

func verify(t *testing.T, store *state.Store, turnID, verdict string, met, testsMeaningful bool, scope, unsupported []string, ids Ids) (state.RunState, error) {
	cur, _, _ := store.Load()
	art := verificationArtifact(t, turnID, cur.Revision, verdict, false, acceptanceCriteria, met, testsMeaningful, scope, unsupported)
	return stepRT(t, store, ProjectionFacts{Task: taskFacts()}, RuntimeFacts{}, "verification", art, ids)
}

// --- the full pure terminal dry run ---

// TESTS fail -> FIX -> TESTS -> pass -> ownerless VERIFY -> verifier fail -> FIX ->
// VERIFY (a fresh threshold) -> verifier pass -> DONE.
func TestTerminalDryRun(t *testing.T) {
	store := newStore(t)
	rs := reachTests(t, store)
	if *rs.StepIndex != rs.AgreedPlan.Plan.StepCount || rs.Assignment != nil {
		t.Fatalf("TESTS is ownerless at the plan end: %+v", rs)
	}

	// TESTS fail -> FIX (TestFixes 0 -> 1), returning to TESTS.
	rs, err := applyTests(t, store, false, RuntimeFacts{}, assign("t-fix1"))
	if err != nil {
		t.Fatalf("tests fail: %v", err)
	}
	if rs.Phase != state.PhaseFix || rs.FixReturn != state.PhaseTests || rs.Counters.TestFixes != 1 {
		t.Fatalf("after tests fail: %+v", rs)
	}

	// FIX report -> back to ownerless TESTS (no runtime facts needed for a TESTS return).
	rs, err = stepRT(t, store, ProjectionFacts{}, RuntimeFacts{}, "implementation_report", fixReport(t, store, "t-fix1"), Ids{})
	if err != nil {
		t.Fatalf("fix -> tests: %v", err)
	}
	if rs.Phase != state.PhaseTests || rs.Assignment != nil {
		t.Fatalf("fix -> tests: %+v", rs)
	}

	// TESTS pass -> ownerless VERIFY, RequiredGeneration = pair gen 3 + 1.
	rs, err = applyTests(t, store, true, RuntimeFacts{CurrentPairGeneration: 3}, Ids{})
	if err != nil {
		t.Fatalf("tests pass: %v", err)
	}
	if rs.Phase != state.PhaseVerify || rs.Assignment != nil || rs.Verify == nil || rs.Verify.RequiredGeneration != 4 {
		t.Fatalf("after tests pass: %+v", rs)
	}

	// A replacement issues the verifier; the verifier FAILS -> FIX (VerifyFixes 1).
	rs = issueVerifier(t, store, "t-verify1")
	rs, err = verify(t, store, "t-verify1", "fail", false, true, nil, nil, assign("t-fix2"))
	if err != nil {
		t.Fatalf("verify fail: %v", err)
	}
	if rs.Phase != state.PhaseFix || rs.FixReturn != state.PhaseVerify || rs.Counters.VerifyFixes != 1 || rs.Verify != nil {
		t.Fatalf("after verify fail: %+v", rs)
	}

	// FIX report -> VERIFY with a FRESH threshold (pair gen 5 + 1 = 6), newer than the
	// verifier that failed.
	rs, err = stepRT(t, store, ProjectionFacts{}, RuntimeFacts{CurrentPairGeneration: 5}, "implementation_report", fixReport(t, store, "t-fix2"), Ids{})
	if err != nil {
		t.Fatalf("fix -> verify: %v", err)
	}
	if rs.Phase != state.PhaseVerify || rs.Verify == nil || rs.Verify.RequiredGeneration != 6 {
		t.Fatalf("fix -> verify with a fresh threshold: %+v", rs)
	}

	// A newer replacement issues the verifier; it PASSES -> DONE.
	rs = issueVerifier(t, store, "t-verify2")
	rs, err = verify(t, store, "t-verify2", "pass", true, true, nil, nil, Ids{})
	if err != nil {
		t.Fatalf("verify pass: %v", err)
	}
	if rs.Phase != state.PhaseDone || rs.Lifecycle != state.LifecycleCompleted || rs.Assignment != nil || rs.Verify != nil {
		t.Fatalf("after verify pass (DONE): %+v", rs)
	}
}

// The test budget exhausts into a test-quality gate (resuming to FIX) with no counter change.
func TestTestsBudgetGate(t *testing.T) {
	store := newStore(t)
	reachTests(t, store)
	// TestRounds default is 2; drive two real fixes, then the third fail gates.
	for i := 0; i < 2; i++ {
		rs, err := applyTests(t, store, false, RuntimeFacts{}, assign("t-fix"+string(rune('a'+i))))
		if err != nil {
			t.Fatalf("tests fail %d: %v", i, err)
		}
		if rs.Phase != state.PhaseFix {
			t.Fatalf("fail %d not at FIX: %s", i, rs.Phase)
		}
		if _, err := stepRT(t, store, ProjectionFacts{}, RuntimeFacts{}, "implementation_report", fixReport(t, store, "t-fix"+string(rune('a'+i))), Ids{}); err != nil {
			t.Fatalf("fix %d: %v", i, err)
		}
	}
	rs, err := applyTests(t, store, false, RuntimeFacts{}, gate("g-test"))
	if err != nil {
		t.Fatalf("tests budget gate: %v", err)
	}
	if rs.Lifecycle != state.LifecyclePaused || rs.Pause == nil || rs.Pause.Kind != state.PauseQualityBudget || rs.Pause.Budget == nil || rs.Pause.Budget.Kind != state.BudgetTest {
		t.Fatalf("not a test-quality gate: %+v", rs)
	}
	if rs.Counters.TestFixes != 2 {
		t.Fatalf("the gate must not advance the counter: %d", rs.Counters.TestFixes)
	}
}

// A VERIFY human decision transfers the verify requirement into the pause.
func TestVerifyHumanGate(t *testing.T) {
	store := newStore(t)
	reachTests(t, store)
	rs, _ := applyTests(t, store, true, RuntimeFacts{CurrentPairGeneration: 2}, Ids{})
	want := *rs.Verify
	rs = issueVerifier(t, store, "t-verify")
	cur, _, _ := store.Load()
	art := verificationArtifact(t, "t-verify", cur.Revision, "pass", true, acceptanceCriteria, true, true, nil, nil)
	rs, err := stepRT(t, store, ProjectionFacts{Task: taskFacts()}, RuntimeFacts{}, "verification", art, gate("g-v"))
	if err != nil {
		t.Fatalf("verify human gate: %v", err)
	}
	if rs.Pause == nil || rs.Pause.Kind != state.PauseHumanDecision || rs.Pause.ResumePhase != state.PhaseVerify {
		t.Fatalf("not a VERIFY human gate: %+v", rs)
	}
	if rs.Verify != nil || rs.Pause.Verify == nil || *rs.Pause.Verify != want {
		t.Fatalf("the verify requirement was not transferred into the pause: %+v", rs)
	}
}

// The verify budget exhausts into a verify-quality gate.
func TestVerifyBudgetGate(t *testing.T) {
	store := newStore(t)
	reachTests(t, store)
	rs, _ := applyTests(t, store, true, RuntimeFacts{CurrentPairGeneration: 1}, Ids{})
	// VerifyRounds default is 2; two real fixes, then the third fail gates.
	for i := 0; i < 2; i++ {
		rs = issueVerifier(t, store, "t-vfail"+string(rune('a'+i)))
		var err error
		rs, err = verify(t, store, "t-vfail"+string(rune('a'+i)), "fail", false, true, nil, nil, assign("t-vfix"+string(rune('a'+i))))
		if err != nil {
			t.Fatalf("verify fail %d: %v", i, err)
		}
		rs, err = stepRT(t, store, ProjectionFacts{}, RuntimeFacts{CurrentPairGeneration: uint64(2 + i)}, "implementation_report", fixReport(t, store, "t-vfix"+string(rune('a'+i))), Ids{})
		if err != nil {
			t.Fatalf("verify fix %d: %v", i, err)
		}
	}
	rs = issueVerifier(t, store, "t-vfinal")
	rs, err := verify(t, store, "t-vfinal", "fail", false, true, nil, nil, gate("g-verify"))
	if err != nil {
		t.Fatalf("verify budget gate: %v", err)
	}
	if rs.Pause == nil || rs.Pause.Kind != state.PauseQualityBudget || rs.Pause.Budget == nil || rs.Pause.Budget.Kind != state.BudgetVerify {
		t.Fatalf("not a verify-quality gate: %+v", rs)
	}
	if rs.Counters.VerifyFixes != 2 {
		t.Fatalf("the gate must not advance the counter: %d", rs.Counters.VerifyFixes)
	}
}

// The verification's criteria must cover the frozen acceptance criteria exactly; the
// verdict must be consistent with its findings; a scope expansion is a blocker (D020).
func TestVerifyProjectionRejects(t *testing.T) {
	reach := func(t *testing.T) *state.Store {
		store := newStore(t)
		reachTests(t, store)
		applyTests(t, store, true, RuntimeFacts{CurrentPairGeneration: 1}, Ids{})
		issueVerifier(t, store, "t-v")
		return store
	}

	cases := []struct {
		name    string
		verdict string
		crit    []string
		met     bool
		meaning bool
		scope   []string
		unsup   []string
	}{
		{"partial criteria", "pass", acceptanceCriteria[:1], true, true, nil, nil},
		{"reordered criteria", "pass", []string{acceptanceCriteria[1], acceptanceCriteria[0]}, true, true, nil, nil},
		{"pass with an unmet criterion", "pass", acceptanceCriteria, false, true, nil, nil},
		{"fail with all met", "fail", acceptanceCriteria, true, true, nil, nil},
		{"pass with a scope expansion", "pass", acceptanceCriteria, true, true, []string{"extra work"}, nil},
		{"pass with unsupported claims", "pass", acceptanceCriteria, true, true, nil, []string{"a claim"}},
		{"pass without meaningful tests", "pass", acceptanceCriteria, true, false, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := reach(t)
			cur, _, _ := store.Load()
			art := verificationArtifact(t, "t-v", cur.Revision, c.verdict, false, c.crit, c.met, c.meaning, c.scope, c.unsup)
			if _, err := stepRT(t, store, ProjectionFacts{Task: taskFacts()}, RuntimeFacts{}, "verification", art, Ids{}); !errors.Is(err, ErrSemantic) {
				t.Fatalf("err = %v, want ErrSemantic", err)
			}
		})
	}

	// A digest mismatch (wrong task facts) is rejected.
	t.Run("task digest mismatch", func(t *testing.T) {
		store := reach(t)
		cur, _, _ := store.Load()
		art := verificationArtifact(t, "t-v", cur.Revision, "pass", false, acceptanceCriteria, true, true, nil, nil)
		bad := ProjectionFacts{Task: TaskFacts{Digest: strings.Repeat("f", 64), AcceptanceCriteria: acceptanceCriteria}}
		if _, err := stepRT(t, store, bad, RuntimeFacts{}, "verification", art, Ids{}); !errors.Is(err, ErrSemantic) {
			t.Fatalf("err = %v, want ErrSemantic", err)
		}
	})

	// A verdict-fail WITH a scope expansion is a legitimate FIX route (not a contradiction).
	t.Run("fail with scope expansion routes to FIX", func(t *testing.T) {
		store := reach(t)
		rs, err := verify(t, store, "t-v", "fail", true, true, []string{"extra"}, nil, assign("t-vfix"))
		if err != nil {
			t.Fatalf("verify fail+scope: %v", err)
		}
		if rs.Phase != state.PhaseFix || rs.FixReturn != state.PhaseVerify {
			t.Fatalf("fail+scope should route to FIX: %+v", rs)
		}
	})
}

// Apply rejects a forged ownerless decision that supplies an identity, and a RouteToVerify
// that omits its verify requirement.
func TestTerminalApplyAdversarial(t *testing.T) {
	t.Run("an ownerless route rejects a supplied id", func(t *testing.T) {
		store := newStore(t)
		reachTests(t, store)
		// TESTS pass -> RouteToVerify (IDNone); an assignment id is a malformed decision.
		if _, err := applyTests(t, store, true, RuntimeFacts{CurrentPairGeneration: 1}, assign("t-x")); !errors.Is(err, ErrBadDecision) {
			t.Fatalf("err = %v, want ErrBadDecision", err)
		}
	})

	t.Run("to-verify without a verify requirement", func(t *testing.T) {
		store := newStore(t)
		reachTests(t, store)
		cur, _, _ := store.Load()
		dec := Decision{
			FromPhase: state.PhaseTests, ExpectedStateRevision: cur.Revision,
			Source: state.EventRef{Digest: strings.Repeat("e", 64)}, Next: state.PhaseVerify, Route: RouteToVerify,
		}
		_, err := store.Mutate(cur.Revision, func(gen uint64, next *state.RunState) error {
			return Apply(dec, dec.Source, Ids{}, gen, next)
		})
		if !errors.Is(err, ErrBadDecision) {
			t.Fatalf("err = %v, want ErrBadDecision", err)
		}
	})
}
