package state

import (
	"fmt"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/protocol"
)

// rejects asserts a mutation from prev is refused by validation.
func rejects(t *testing.T, s *Store, prev RunState, label string, mut func(rev uint64, next *RunState)) {
	t.Helper()
	if _, err := s.Mutate(prev.Revision, func(rev uint64, next *RunState) error { mut(rev, next); return nil }); err == nil {
		t.Fatalf("%s: mutation should have been rejected", label)
	}
}

// toTests advances an agreed IMPLEMENT_STEP run to TESTS (cursor at the plan end).
func toTests(t *testing.T, s *Store, impl RunState) RunState {
	t.Helper()
	rs, err := s.Mutate(impl.Revision, func(_ uint64, n *RunState) error {
		idx := 1
		n.StepIndex = &idx
		n.Phase = PhaseTests
		return nil
	})
	if err != nil {
		t.Fatalf("to tests: %v", err)
	}
	return rs
}

// toOwnedVerify advances TESTS to a VERIFY that has a verification turn assigned
// (owned), so a gate raised from it has an accepted source.
func toOwnedVerify(t *testing.T, s *Store, tests RunState) RunState {
	t.Helper()
	verify, err := s.Mutate(tests.Revision, func(_ uint64, n *RunState) error {
		n.Phase = PhaseVerify
		n.Verify = &VerifyRequirement{RequiredGeneration: 1}
		return nil
	})
	if err != nil {
		t.Fatalf("to verify: %v", err)
	}
	return assignAt(t, s, verify, "v-turn")
}

func TestV5GateCoherenceRejected(t *testing.T) {
	s := newStore(t)
	impl := mustAgreedImplement(t, s)
	// Each single incoherent gate field breaks the four-way equivalence.
	rejects(t, s, impl, "gate only", func(rev uint64, n *RunState) { n.Gate = &Ref{ID: "g", IssuedRevision: rev} })
	rejects(t, s, impl, "paused only", func(_ uint64, n *RunState) { n.Lifecycle = LifecyclePaused })
	rejects(t, s, impl, "await only", func(_ uint64, n *RunState) { n.Phase = PhaseAwaitGuidance })
	rejects(t, s, impl, "pause only", func(_ uint64, n *RunState) {
		n.Pause = &PauseContext{Kind: PauseHumanDecision, OriginPhase: PhaseImplementStep, ResumePhase: PhaseImplementStep, Source: EventRef{Digest: hex64("7"), TurnID: "x"}}
	})
}

func TestV5ValidHumanGateFromImplement(t *testing.T) {
	s := newStore(t)
	assigned := assignAt(t, s, mustAgreedImplement(t, s), "t1")
	gated := acceptTurnAdvance(t, s, assigned, "t1", hex64("7"), func(rev uint64, n *RunState) {
		n.Pause = &PauseContext{Kind: PauseHumanDecision, OriginPhase: PhaseImplementStep, ResumePhase: PhaseImplementStep, Source: EventRef{Digest: hex64("7"), TurnID: "t1"}}
		n.Gate = &Ref{ID: "gate-1", IssuedRevision: rev}
		n.Phase = PhaseAwaitGuidance
		n.Lifecycle = LifecyclePaused
	})
	if gated.Pause == nil || gated.Gate == nil || gated.Phase != PhaseAwaitGuidance || gated.Lifecycle != LifecyclePaused {
		t.Fatalf("valid human gate not persisted: %+v", gated)
	}
}

func TestV5PhaseFamilyShapes(t *testing.T) {
	s := newStore(t)
	// A fresh INIT cannot jump straight to an implementation phase (no agreement).
	// A rejected mutation does not consume a generation, so the same init drives on.
	init := mustInit(t, s)
	rejects(t, s, init, "implement without agreement", func(rev uint64, n *RunState) {
		n.Phase = PhaseImplementStep
		n.Assignment = &Ref{ID: "t1", IssuedRevision: rev}
	})

	// TESTS/VERIFY/DONE require the cursor at the plan end.
	impl := driveToAgreedImplement(t, s, init)
	rejects(t, s, impl, "tests before plan end", func(_ uint64, n *RunState) { n.Phase = PhaseTests }) // cursor still 0
	rejects(t, s, impl, "done before plan end", func(_ uint64, n *RunState) {
		n.Phase = PhaseDone
		n.Lifecycle = LifecycleCompleted
	})
}

func TestV5AgreedPlanWriteOnce(t *testing.T) {
	s := newStore(t)
	impl := mustAgreedImplement(t, s)
	rejects(t, s, impl, "mutate agreed plan", func(_ uint64, n *RunState) {
		n.AgreedPlan.Plan.StepCount = 1 // any change to the frozen agreement
		n.AgreedPlan.AgreedRevision = 999
	})
}

func TestV5CursorMonotonicAndBounds(t *testing.T) {
	s := newStore(t)
	impl := mustAgreedImplement(t, s) // cursor 0
	tests := toTests(t, s, impl)      // cursor 1
	rejects(t, s, tests, "cursor rewind", func(_ uint64, n *RunState) {
		idx := 0
		n.StepIndex = &idx
		n.Phase = PhaseImplementStep
	})
	rejects(t, s, impl, "cursor past the plan end", func(_ uint64, n *RunState) {
		idx := 2 // StepCount is 1
		n.StepIndex = &idx
	})
}

func TestV5VerifyPresenceAndTransfer(t *testing.T) {
	s := newStore(t)
	impl := mustAgreedImplement(t, s)
	// A verify requirement outside VERIFY is rejected.
	rejects(t, s, impl, "verify at implement", func(_ uint64, n *RunState) {
		n.Verify = &VerifyRequirement{RequiredGeneration: 1}
	})

	// Ownerless VERIFY: a verify requirement with no assignment is valid.
	tests := toTests(t, s, impl)
	verify, err := s.Mutate(tests.Revision, func(_ uint64, n *RunState) error {
		n.Phase = PhaseVerify
		n.Verify = &VerifyRequirement{RequiredGeneration: 1}
		return nil
	})
	if err != nil {
		t.Fatalf("ownerless verify: %v", err)
	}
	if verify.Verify == nil || verify.Assignment != nil {
		t.Fatalf("ownerless verify shape wrong: %+v", verify)
	}

	// Human gate from VERIFY transfers the requirement into the pause and clears the
	// top-level Verify; restoring brings the same requirement back.
	owned := toOwnedVerify(t, s, verify) // v-turn assigned, Verify still set
	gated := acceptTurnAdvance(t, s, owned, "v-turn", hex64("8"), func(rev uint64, n *RunState) {
		n.Pause = &PauseContext{
			Kind: PauseHumanDecision, OriginPhase: PhaseVerify, ResumePhase: PhaseVerify,
			Source: EventRef{Digest: hex64("8"), TurnID: "v-turn"},
			Verify: &VerifyRequirement{RequiredGeneration: n.Verify.RequiredGeneration},
		}
		n.Verify = nil
		n.Gate = &Ref{ID: "gate-v", IssuedRevision: rev}
		n.Phase = PhaseAwaitGuidance
		n.Lifecycle = LifecyclePaused
	})
	if gated.Verify != nil || gated.Pause == nil || gated.Pause.Verify == nil || gated.Pause.Verify.RequiredGeneration != 1 {
		t.Fatalf("verify not transferred into the pause: %+v", gated)
	}
	// A transfer that drops the requirement is rejected.
	rejects(t, s, owned, "pausing VERIFY without preserving", func(rev uint64, n *RunState) {
		n.Pause = &PauseContext{Kind: PauseHumanDecision, OriginPhase: PhaseVerify, ResumePhase: PhaseVerify, Source: EventRef{Digest: hex64("8"), TurnID: "v-turn"}}
		n.Verify = nil
		n.Gate = &Ref{ID: "gate-v", IssuedRevision: rev}
		n.Phase = PhaseAwaitGuidance
		n.Lifecycle = LifecyclePaused
		n.AcceptedTurns["v-turn"] = AcceptedTurn{ArtifactDigest: hex64("8"), Receipt: Receipt{TurnID: "v-turn", Revision: rev, ArtifactDigest: hex64("8")}, Phase: PhaseVerify}
	})

	// Restore: back to VERIFY with the same requirement, a freshly issued turn.
	restored, err := s.Mutate(gated.Revision, func(rev uint64, n *RunState) error {
		n.Verify = &VerifyRequirement{RequiredGeneration: n.Pause.Verify.RequiredGeneration}
		n.Pause = nil
		n.Gate = nil
		n.Phase = PhaseVerify
		n.Lifecycle = LifecycleRunning
		n.Assignment = &Ref{ID: "v-turn-2", IssuedRevision: rev}
		return nil
	})
	if err != nil {
		t.Fatalf("restore verify: %v", err)
	}
	if restored.Verify == nil || restored.Verify.RequiredGeneration != 1 {
		t.Fatalf("verify not restored: %+v", restored)
	}
}

// toCheckpoint moves an agreed IMPLEMENT_STEP run to CHECKPOINT (cursor unchanged)
// with a checkpoint turn assigned; opt lets a caller adjust counters in the move.
func toCheckpoint(t *testing.T, s *Store, impl RunState, opt func(n *RunState)) RunState {
	t.Helper()
	chk, err := s.Mutate(impl.Revision, func(_ uint64, n *RunState) error {
		n.Phase = PhaseCheckpoint
		if opt != nil {
			opt(n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("to checkpoint: %v", err)
	}
	return assignAt(t, s, chk, "c1")
}

// checkpointBudgetGate raises a checkpoint quality-budget gate, accepting c1 as the
// CHECKPOINT-phase source.
func checkpointBudgetGate(rev uint64, n *RunState) {
	n.Pause = &PauseContext{Kind: PauseQualityBudget, OriginPhase: PhaseCheckpoint, ResumePhase: PhaseFix, FixReturn: PhaseCheckpoint, Budget: &BudgetPause{Kind: BudgetCheckpoint}, Source: EventRef{Digest: hex64("7"), TurnID: "c1"}}
	n.Gate = &Ref{ID: "g", IssuedRevision: rev}
	n.Phase = PhaseAwaitGuidance
	n.Lifecycle = LifecyclePaused
}

func TestV5QualityBudgetPauseHonesty(t *testing.T) {
	// A checkpoint quality-budget pause claims the step's fixes reached the frozen
	// limit; an honest label requires StepFixes[cursor] == CheckpointRounds.
	lim := config.DefaultRunPolicy().Budgets.CheckpointRounds

	// Dishonest: the step counter has not reached the limit.
	s1 := newStore(t)
	assignedLow := toCheckpoint(t, s1, mustAgreedImplement(t, s1), nil) // StepFixes[0] == 0
	rejects(t, s1, assignedLow, "dishonest checkpoint budget", func(rev uint64, n *RunState) {
		n.AcceptedTurns["c1"] = AcceptedTurn{ArtifactDigest: hex64("7"), Receipt: Receipt{TurnID: "c1", Revision: rev, ArtifactDigest: hex64("7")}, Phase: PhaseCheckpoint}
		checkpointBudgetGate(rev, n)
	})

	// Honest: the step counter is exactly at the frozen limit.
	s2 := newStore(t)
	assignedAtLimit := toCheckpoint(t, s2, mustAgreedImplement(t, s2), func(n *RunState) { n.Counters.StepFixes[0] = lim })
	gated := acceptTurnAdvance(t, s2, assignedAtLimit, "c1", hex64("7"), checkpointBudgetGate)
	if gated.Pause == nil || gated.Pause.Kind != PauseQualityBudget {
		t.Fatalf("honest quality-budget pause not persisted: %+v", gated)
	}
}

func TestV5EventProvenanceRejected(t *testing.T) {
	s := newStore(t)
	// A candidate plan whose source turn is not accepted is rejected.
	init := mustInit(t, s)
	draft := issueFirstTurn(t, s, init, planTurnID)
	rejects(t, s, draft, "candidate source not accepted", func(_ uint64, n *RunState) {
		n.Phase = PhasePlanCritique
		n.CandidatePlan = &PlanRef{Source: EventRef{Digest: hex64("1"), TurnID: "ghost"}, Digest: hex64("2"), StepCount: 1}
		n.CandidateChecks = candidateChecks()
		n.Assignment = nil
	})
}

func TestV5CheckSetEncoding(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	draft := issueFirstTurn(t, s, init, planTurnID)
	// Unsorted keys are rejected.
	rejects(t, s, draft, "unsorted check keys", func(_ uint64, n *RunState) {
		n.Phase = PhasePlanCritique
		n.CandidatePlan = candidatePlan()
		n.CandidateChecks = &CheckSetRef{Keys: []string{"b-key", "a-key"}, Digest: hex64("e")}
	})
	// An empty key set with a non-canonical digest is rejected.
	rejects(t, s, draft, "empty checks wrong digest", func(_ uint64, n *RunState) {
		n.Phase = PhasePlanCritique
		n.CandidatePlan = candidatePlan()
		n.CandidateChecks = &CheckSetRef{Keys: []string{}, Digest: hex64("e")}
	})
}

func TestV5CounterCeiling(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	rejects(t, s, r1, "counter above ceiling", func(_ uint64, n *RunState) {
		n.Counters.PlanRevisions = config.MaxBudget + 1
	})
}

func TestV5SecretRejectedInControlField(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	draft := issueFirstTurn(t, s, init, planTurnID)
	secret := "sk-ant-abcdefghijklmnopqrstuvwx"
	// A secret in a v5 control field (a check key) is rejected, never rewritten.
	rejects(t, s, draft, "secret in check key", func(_ uint64, n *RunState) {
		n.Phase = PhasePlanCritique
		n.CandidatePlan = candidatePlan()
		n.CandidateChecks = &CheckSetRef{Keys: []string{secret}, Digest: hex64("e")}
	})
	if strings.Contains(secret, "REDACTED") { // guard against an accidental rewrite path
		t.Fatal("secret constant unexpectedly redacted")
	}
}

// A secret placed in an enum control field must be rejected too — redaction runs
// before validation, so an error message would otherwise echo it.
func TestV5SecretRejectedInEnumField(t *testing.T) {
	s := newStore(t)
	secret := "sk-ant-abcdefghijklmnopqrstuvwx"
	assigned := assignAt(t, s, mustAgreedImplement(t, s), "t1")
	rejects(t, s, assigned, "secret in pause kind", func(rev uint64, n *RunState) {
		n.AcceptedTurns["t1"] = AcceptedTurn{ArtifactDigest: hex64("7"), Receipt: Receipt{TurnID: "t1", Revision: rev, ArtifactDigest: hex64("7")}, Phase: PhaseImplementStep}
		n.Pause = &PauseContext{Kind: PauseKind(secret), OriginPhase: PhaseImplementStep, ResumePhase: PhaseImplementStep, Source: EventRef{Digest: hex64("7"), TurnID: "t1"}}
		n.Gate = &Ref{ID: "g", IssuedRevision: rev}
		n.Phase = PhaseAwaitGuidance
		n.Lifecycle = LifecyclePaused
	})
}

// An empty check set persists as a non-nil empty array and survives cloning into the
// next generation (append([]string(nil),...) would collapse it to null and fail the
// non-nil requirement on the following mutation).
func TestV5EmptyCheckSetSurvivesClone(t *testing.T) {
	s := newStore(t)
	critique := driveToCritique(t, s, mustInit(t, s)) // empty candidate checks
	// The next generation clones the empty check set; if it collapsed to nil the
	// mutation would be rejected.
	assignAt(t, s, critique, critTurnID)
	loaded, _, _ := s.Load()
	if loaded.CandidateChecks == nil || loaded.CandidateChecks.Keys == nil || len(loaded.CandidateChecks.Keys) != 0 {
		t.Fatalf("empty check set did not survive as a non-nil empty array: %+v", loaded.CandidateChecks)
	}
}

func TestV5CandidateChecksLifecycle(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	draft := issueFirstTurn(t, s, init, planTurnID)

	// The first candidate must initialize the canonical empty check set.
	rejects(t, s, draft, "initial non-empty checks", func(_ uint64, n *RunState) {
		n.Phase = PhasePlanCritique
		n.CandidatePlan = candidatePlan()
		n.CandidateChecks = &CheckSetRef{Keys: []string{"chk-a"}, Digest: hex64("e")}
	})

	// From a valid PLAN_CRITIQUE (built from the same draft), the check set cannot
	// change without a fresh critique.
	critique := acceptTurnAdvance(t, s, draft, planTurnID, planSrcDigest(), func(_ uint64, n *RunState) {
		n.Phase = PhasePlanCritique
		n.CandidatePlan = candidatePlan()
		n.CandidateChecks = candidateChecks()
	})
	rejects(t, s, critique, "checks change without a critique", func(_ uint64, n *RunState) {
		n.CandidateChecks = &CheckSetRef{Keys: []string{"chk-x"}, Digest: hex64("e")}
	})
}

func TestV5PausedImmutability(t *testing.T) {
	s := newStore(t)
	assigned := assignAt(t, s, mustAgreedImplement(t, s), "t1")
	gated := acceptTurnAdvance(t, s, assigned, "t1", hex64("7"), func(rev uint64, n *RunState) {
		n.Pause = &PauseContext{Kind: PauseHumanDecision, OriginPhase: PhaseImplementStep, ResumePhase: PhaseImplementStep, Source: EventRef{Digest: hex64("7"), TurnID: "t1"}}
		n.Gate = &Ref{ID: "gate-1", IssuedRevision: rev}
		n.Phase = PhaseAwaitGuidance
		n.Lifecycle = LifecyclePaused
	})
	// A counter cannot advance while the run stays paused.
	rejects(t, s, gated, "counter changed while paused", func(_ uint64, n *RunState) {
		n.Counters.PlanRevisions++
	})
	// The pause record itself is frozen while paused.
	rejects(t, s, gated, "pause resume target changed", func(_ uint64, n *RunState) {
		n.Pause.ResumePhase = PhaseCheckpoint
	})
}

func TestV5SamePhaseVerifyPreserved(t *testing.T) {
	s := newStore(t)
	impl := mustAgreedImplement(t, s)
	tests := toTests(t, s, impl)
	verify, err := s.Mutate(tests.Revision, func(_ uint64, n *RunState) error {
		n.Phase = PhaseVerify
		n.Verify = &VerifyRequirement{RequiredGeneration: 1}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Staying in VERIFY must preserve the requirement and the fix counter.
	rejects(t, s, verify, "verify requirement changed in VERIFY", func(_ uint64, n *RunState) {
		n.Verify.RequiredGeneration = 9
	})
	rejects(t, s, verify, "verify_fixes changed in VERIFY", func(_ uint64, n *RunState) {
		n.Counters.VerifyFixes++
	})
}

// cloneForNext must produce a fully independent copy: mutating any nested
// pointer/slice of the clone must not touch the original. The value need not be a
// valid phase snapshot; only the aliasing is under test.
func TestV5CloneForNextNoAlias(t *testing.T) {
	idx := 3
	orig := &RunState{
		AcceptedTurns:   map[string]AcceptedTurn{},
		Counters:        Counters{StepFixes: []int{1, 2}},
		CandidatePlan:   &PlanRef{Source: EventRef{Digest: hex64("1"), TurnID: "p"}, Digest: hex64("2"), StepCount: 2},
		CandidateChecks: &CheckSetRef{Keys: []string{"a", "b"}, Digest: hex64("e")},
		PendingFindings: &FindingObligations{Source: EventRef{Digest: hex64("3"), TurnID: "c"}, Keys: []string{"f"}},
		AgreedPlan:      &PlanAgreement{Plan: PlanRef{StepCount: 1}, Critique: EventRef{TurnID: "c"}, Checks: CheckSetRef{Keys: []string{"x"}, Digest: hex64("e")}, AgreedRevision: 1},
		StepIndex:       &idx,
		Verify:          &VerifyRequirement{RequiredGeneration: 2},
		Pause:           &PauseContext{Kind: PauseQualityBudget, Budget: &BudgetPause{Kind: BudgetCheckpoint}, Verify: &VerifyRequirement{RequiredGeneration: 4}},
	}
	cl := cloneForNext(orig)
	cl.Counters.StepFixes[0] = 99
	cl.CandidatePlan.StepCount = 99
	cl.CandidateChecks.Keys[0] = "zzz"
	cl.PendingFindings.Keys[0] = "zzz"
	cl.AgreedPlan.Checks.Keys[0] = "zzz"
	cl.AgreedPlan.AgreedRevision = 99
	*cl.StepIndex = 99
	cl.Verify.RequiredGeneration = 99
	cl.Pause.Budget.Kind = "zzz"
	cl.Pause.Verify.RequiredGeneration = 99

	if orig.Counters.StepFixes[0] != 1 || orig.CandidatePlan.StepCount != 2 ||
		orig.CandidateChecks.Keys[0] != "a" || orig.PendingFindings.Keys[0] != "f" ||
		orig.AgreedPlan.Checks.Keys[0] != "x" || orig.AgreedPlan.AgreedRevision != 1 ||
		*orig.StepIndex != 3 || orig.Verify.RequiredGeneration != 2 ||
		orig.Pause.Budget.Kind != BudgetCheckpoint || orig.Pause.Verify.RequiredGeneration != 4 {
		t.Fatalf("cloneForNext aliased a nested field: %+v", orig)
	}
}

// The empty-set biconditional rejects the opposite direction too: non-empty keys
// carrying the canonical empty digest.
func TestV5NonEmptyChecksWithEmptyDigestRejected(t *testing.T) {
	s := newStore(t)
	draft := issueFirstTurn(t, s, mustInit(t, s), planTurnID)
	rejects(t, s, draft, "non-empty keys with empty digest", func(_ uint64, n *RunState) {
		n.Phase = PhasePlanCritique
		n.CandidatePlan = candidatePlan()
		n.CandidateChecks = &CheckSetRef{Keys: []string{"chk-a"}, Digest: emptyCheckSetDigest}
	})
}

// Opening a gate must not charge the counter or move the cursor in the same
// generation, so a quality label proves the counter was already at the limit and a
// checkpoint FIX parks against the current step.
func TestV5GateOpeningFreezesCounterAndCursor(t *testing.T) {
	// Bump-to-limit while opening a checkpoint gate is rejected (would let budget
	// honesty accept a limit the third fix should still have been allowed).
	s1 := newStore(t)
	impl1 := mustAgreedImplement(t, s1)
	lim := impl1.EffectivePolicy.Budgets.CheckpointRounds
	chk1 := toCheckpoint(t, s1, impl1, func(n *RunState) { n.Counters.StepFixes[0] = lim - 1 })
	rejects(t, s1, chk1, "bump-to-limit while opening gate", func(rev uint64, n *RunState) {
		n.AcceptedTurns["c1"] = AcceptedTurn{ArtifactDigest: hex64("7"), Receipt: Receipt{TurnID: "c1", Revision: rev, ArtifactDigest: hex64("7")}, Phase: PhaseCheckpoint}
		n.Assignment = nil
		n.Counters.StepFixes[0] = lim // the illegal in-transition charge
		checkpointBudgetGate(rev, n)
	})

	// Moving a two-step cursor while opening a human gate is rejected (would park a
	// checkpoint FIX against the wrong step). A human gate avoids budget honesty.
	s2 := newStore(t)
	impl2 := driveToAgreedImplementN(t, s2, mustInit(t, s2), 2)
	chk2 := toCheckpoint(t, s2, impl2, nil) // cursor 0, c1 assigned
	rejects(t, s2, chk2, "move cursor while opening gate", func(rev uint64, n *RunState) {
		n.AcceptedTurns["c1"] = AcceptedTurn{ArtifactDigest: hex64("7"), Receipt: Receipt{TurnID: "c1", Revision: rev, ArtifactDigest: hex64("7")}, Phase: PhaseCheckpoint}
		n.Assignment = nil
		idx := 1
		n.StepIndex = &idx // the illegal in-transition cursor move
		n.Pause = &PauseContext{Kind: PauseHumanDecision, OriginPhase: PhaseCheckpoint, ResumePhase: PhaseCheckpoint, Source: EventRef{Digest: hex64("7"), TurnID: "c1"}}
		n.Gate = &Ref{ID: "g", IssuedRevision: rev}
		n.Phase = PhaseAwaitGuidance
		n.Lifecycle = LifecyclePaused
	})
}

// A gated run must have no outstanding assignment.
func TestV5GateRejectsLingeringAssignment(t *testing.T) {
	s := newStore(t)
	chk := toCheckpoint(t, s, mustAgreedImplement(t, s), nil) // c1 assigned
	rejects(t, s, chk, "lingering assignment at a gate", func(rev uint64, n *RunState) {
		n.AcceptedTurns["c1"] = AcceptedTurn{ArtifactDigest: hex64("7"), Receipt: Receipt{TurnID: "c1", Revision: rev, ArtifactDigest: hex64("7")}, Phase: PhaseCheckpoint}
		// Assignment deliberately left set.
		n.Pause = &PauseContext{Kind: PauseHumanDecision, OriginPhase: PhaseCheckpoint, ResumePhase: PhaseCheckpoint, Source: EventRef{Digest: hex64("7"), TurnID: "c1"}}
		n.Gate = &Ref{ID: "g", IssuedRevision: rev}
		n.Phase = PhaseAwaitGuidance
		n.Lifecycle = LifecyclePaused
	})
}

// A no-op mutation while a planning-phase human gate remains paused must keep the
// empty step-fix vector (a clone that collapses [] to nil would fail paused
// immutability's counter comparison).
func TestV5PlanningGateNoOpRetainsEmptyStepFixes(t *testing.T) {
	s := newStore(t)
	assigned := assignAt(t, s, driveToCritique(t, s, mustInit(t, s)), "c2")
	gated := acceptTurnAdvance(t, s, assigned, "c2", hex64("9"), func(rev uint64, n *RunState) {
		n.Pause = &PauseContext{Kind: PauseHumanDecision, OriginPhase: PhasePlanCritique, ResumePhase: PhasePlanCritique, Source: EventRef{Digest: hex64("9"), TurnID: "c2"}}
		n.Gate = &Ref{ID: "g", IssuedRevision: rev}
		n.Phase = PhaseAwaitGuidance
		n.Lifecycle = LifecyclePaused
	})
	if _, err := s.Mutate(gated.Revision, func(_ uint64, _ *RunState) error { return nil }); err != nil {
		t.Fatalf("no-op while a planning gate is paused should be allowed: %v", err)
	}
	loaded, _, _ := s.Load()
	if loaded.Counters.StepFixes == nil || len(loaded.Counters.StepFixes) != 0 {
		t.Fatalf("step_fixes did not stay a non-nil empty array: %+v", loaded.Counters.StepFixes)
	}
}

func TestV5KeySetCountBound(t *testing.T) {
	s := newStore(t)
	draft := issueFirstTurn(t, s, mustInit(t, s), planTurnID)
	rejects(t, s, draft, "too many check keys", func(_ uint64, n *RunState) {
		n.Phase = PhasePlanCritique
		n.CandidatePlan = candidatePlan()
		keys := make([]string, protocol.MaxKeySetItems+1)
		for i := range keys {
			keys[i] = fmt.Sprintf("k-%05d", i) // canonical, sorted, distinct
		}
		n.CandidateChecks = &CheckSetRef{Keys: keys, Digest: hex64("e")}
	})
}
