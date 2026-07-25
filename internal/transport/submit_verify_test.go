package transport

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

// --- pure ownerless/threshold helpers ---

func TestCurrentPairGeneration(t *testing.T) {
	sess := fmt.Sprintf("sess-%032x", 7)
	reg := state.Registry{
		RunID: "run-a",
		Pair:  &state.RoleSlot{Agent: state.AgentCodex, CurrentSessionID: sess, Sessions: []state.SessionRecord{{SessionID: sess, Generation: 3, IssuedRegistryRevision: 1}}},
	}
	if g, err := currentPairGeneration(reg); err != nil || g != 3 {
		t.Fatalf("g=%d err=%v, want 3", g, err)
	}
	if _, err := currentPairGeneration(state.Registry{RunID: "run-a"}); !errors.Is(err, ErrRunMismatch) {
		t.Fatalf("no pair slot err = %v, want ErrRunMismatch", err)
	}
}

func TestCheckEnterVerifyThreshold(t *testing.T) {
	req := func(g uint64) *state.VerifyRequirement { return &state.VerifyRequirement{RequiredGeneration: g} }
	ownerless := func(v *state.VerifyRequirement) *state.RunState {
		return &state.RunState{Phase: state.PhaseVerify, Verify: v}
	}
	fromTests := state.RunState{Phase: state.PhaseTests}

	// Enters ownerless VERIFY with the exact threshold (pair gen 3 -> required 4).
	if err := checkEnterVerifyThreshold(fromTests, ownerless(req(4)), 3); err != nil {
		t.Fatalf("exact threshold: %v", err)
	}
	// Rejections on entry: wrong threshold, missing requirement, zero/overflow pair gen.
	for name, tc := range map[string]struct {
		next *state.RunState
		gen  uint64
	}{
		"wrong threshold": {ownerless(req(5)), 3},
		"missing":         {ownerless(nil), 3},
		"zero pair gen":   {ownerless(req(1)), 0},
		"overflow":        {ownerless(req(1)), ^uint64(0)},
	} {
		if err := checkEnterVerifyThreshold(fromTests, tc.next, tc.gen); !errors.Is(err, ErrTransitionInvalid) {
			t.Fatalf("%s: err = %v, want ErrTransitionInvalid", name, err)
		}
	}
	// Entering VERIFY WITH an assignment forges past the mandatory ownerless wait.
	assigned := &state.RunState{Phase: state.PhaseVerify, Assignment: &state.Ref{ID: "x"}, Verify: req(4)}
	if err := checkEnterVerifyThreshold(fromTests, assigned, 3); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("assigned entry should be rejected: %v", err)
	}
	// Same-phase ownerless->assigned (a replacement issuing the verifier) is allowed.
	if err := checkEnterVerifyThreshold(state.RunState{Phase: state.PhaseVerify}, assigned, 3); err != nil {
		t.Fatalf("replacement issuance should be allowed: %v", err)
	}
	// Same-phase active->ownerless (clearing the assignment) would strand the run.
	activeOld := state.RunState{Phase: state.PhaseVerify, Assignment: &state.Ref{ID: "x"}}
	if err := checkEnterVerifyThreshold(activeOld, ownerless(req(4)), 3); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("clearing an active VERIFY should be rejected: %v", err)
	}
	// A verifier outcome that leaves VERIFY is not this check's concern.
	if err := checkEnterVerifyThreshold(activeOld, &state.RunState{Phase: state.PhaseDone}, 3); err != nil {
		t.Fatalf("leaving VERIFY should be a no-op: %v", err)
	}
}

func TestRequireLiveOwnerOwnerlessVerify(t *testing.T) {
	if err := requireLiveOwner(&state.RunState{Phase: state.PhaseVerify, Lifecycle: state.LifecycleRunning, Verify: &state.VerifyRequirement{RequiredGeneration: 2}}, "t", 5); err != nil {
		t.Fatalf("ownerless VERIFY: %v", err)
	}
	if err := requireLiveOwner(&state.RunState{Phase: state.PhaseVerify, Lifecycle: state.LifecycleRunning}, "t", 5); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("ownerless VERIFY without a requirement: %v", err)
	}
}

// --- fixtures + fake Prepares ---

func pairSessGen(i uint64) string { return fmt.Sprintf("sess-%032x", 0x100+i) }

// registryPairGen builds a registry whose pair slot's current generation is gen (one
// initial fill + gen-1 replacements), returning the current pair session id.
func registryPairGen(t *testing.T, store *state.Store, gen uint64) string {
	t.Helper()
	reg := openRunRegistry(store)
	r, err := reg.Mutate(0, func(g uint64, n *state.Registry) error {
		n.RunID = "run-a"
		n.Lead = &state.RoleSlot{Agent: state.AgentClaude, CurrentSessionID: leadSess, Sessions: []state.SessionRecord{{SessionID: leadSess, Generation: 1, IssuedRegistryRevision: g}}}
		return nil
	})
	if err != nil {
		t.Fatalf("registry init: %v", err)
	}
	rev := r.Revision
	for i := uint64(1); i <= gen; i++ {
		gi := i
		r, err = reg.Mutate(rev, func(g uint64, n *state.Registry) error {
			s := state.SessionRecord{SessionID: pairSessGen(gi), Generation: gi, IssuedRegistryRevision: g}
			if n.Pair == nil {
				n.Pair = &state.RoleSlot{Agent: state.AgentCodex, CurrentSessionID: s.SessionID, Sessions: []state.SessionRecord{s}}
			} else {
				n.Pair.Sessions = append(n.Pair.Sessions, s)
				n.Pair.CurrentSessionID = s.SessionID
			}
			return nil
		})
		if err != nil {
			t.Fatalf("registry pair gen %d: %v", i, err)
		}
		rev = r.Revision
	}
	return pairSessGen(gen)
}

func newStoreAt(t *testing.T, mut func(gen uint64, n *state.RunState)) (*state.Store, uint64) {
	t.Helper()
	dir := t.TempDir()
	store := state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
	r1, err := store.Mutate(0, func(_ uint64, n *state.RunState) error { initValid(n); return nil })
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	implRev := driveAgreedImplement(t, store, r1.Revision)
	r2, err := store.Mutate(implRev, func(gen uint64, n *state.RunState) error { mut(gen, n); return nil })
	if err != nil {
		t.Fatalf("shape mutation: %v", err)
	}
	return store, r2.Revision
}

// runAtActiveVerify puts the run at an active VERIFY (a verifier turn issued, the
// requirement retained).
func runAtActiveVerify(t *testing.T, requiredGen uint64) (*state.Store, uint64) {
	return newStoreAt(t, func(gen uint64, n *state.RunState) {
		*n.StepIndex = n.AgreedPlan.Plan.StepCount
		n.Phase = state.PhaseVerify
		n.Verify = &state.VerifyRequirement{RequiredGeneration: requiredGen}
		n.Assignment = &state.Ref{ID: "verify-turn", IssuedRevision: gen}
		bindEvidence(n, gen)
	})
}

// runAtFixReturningVerify puts the run at a FIX whose return target is VERIFY.
func runAtFixReturningVerify(t *testing.T) (*state.Store, uint64) {
	return newStoreAt(t, func(gen uint64, n *state.RunState) {
		*n.StepIndex = n.AgreedPlan.Plan.StepCount
		n.Phase = state.PhaseFix
		n.FixReturn = state.PhaseVerify
		n.Assignment = &state.Ref{ID: "fix-turn", IssuedRevision: gen}
		bindEvidence(n, gen)
	})
}

func verificationRaw(turnID string, rev uint64) []byte {
	return []byte(fmt.Sprintf(`{"protocol_version":1,"message_type":"verification","turn_id":%q,"state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"verdict":"pass","criteria":[],"scope_expansion":[],"tests_meaningful":true,"unsupported_claims":[],"notes":"n"}`, turnID, rev))
}

// capturedPrep records what it received and returns a fixed transition.
type capturedPrep struct {
	called  bool
	pairGen uint64
	turn    string
	gate    string
	apply   func(gen uint64, next *state.RunState) error
}

func (c *capturedPrep) prep() Prepare {
	return func(_ state.RunState, prepared PreparedSubmit) (PreparedTransition, error) {
		c.called = true
		c.pairGen = prepared.CurrentPairGeneration
		return NewPreparedTransition(c.turn, c.gate, c.apply), nil
	}
}

// verifyToDone is a valid VERIFY -> DONE transition.
func verifyToDone(_ uint64, next *state.RunState) error {
	next.Assignment = nil
	next.Evidence = nil // the binding is consumed with the turn it authorized
	next.Phase = state.PhaseDone
	next.Lifecycle = state.LifecycleCompleted
	next.Verify = nil
	return nil
}

// The incumbent pair generation is rejected before Prepare/sink; a qualifying
// generation reaches Prepare with the locked pair generation and commits.
func TestVerifyFreshGenerationAuth(t *testing.T) {
	const required = 3

	t.Run("incumbent rejected before Prepare and sink", func(t *testing.T) {
		store, rev := runAtActiveVerify(t, required)
		registryPairGen(t, store, required-1)
		cp := &capturedPrep{apply: verifyToDone}
		sink := newMemSink()
		_, err := Submit(context.Background(), submitDeps(store, sink, cp.prep()), pairSessGen(required-1), verificationRaw("verify-turn", rev))
		if !errors.Is(err, ErrFreshSessionRequired) {
			t.Fatalf("err = %v, want ErrFreshSessionRequired", err)
		}
		if cp.called {
			t.Fatal("Prepare was called for an incumbent submit")
		}
		if len(sink.m) != 0 {
			t.Fatal("the sink was touched for an incumbent submit")
		}
	})

	for _, gen := range []uint64{required, required + 1} {
		t.Run(fmt.Sprintf("generation %d reaches Prepare with the locked fact", gen), func(t *testing.T) {
			store, rev := runAtActiveVerify(t, required)
			sess := registryPairGen(t, store, gen)
			cp := &capturedPrep{apply: verifyToDone}
			res, err := Submit(context.Background(), submitDeps(store, newMemSink(), cp.prep()), sess, verificationRaw("verify-turn", rev))
			if err != nil {
				t.Fatalf("qualifying submit: %v", err)
			}
			if !cp.called {
				t.Fatal("Prepare was not reached")
			}
			if cp.pairGen != gen {
				t.Fatalf("PreparedSubmit.CurrentPairGeneration = %d, want the locked %d", cp.pairGen, gen)
			}
			if res.Receipt.TurnID != "verify-turn" {
				t.Fatalf("receipt = %+v", res.Receipt)
			}
		})
	}
}

// A forged Prepare cannot enter VERIFY with an assignment (bypassing the ownerless
// fresh-session wait), nor clear an active VERIFY back to the ownerless wait.
func TestVerifyForgedTransitionsRejected(t *testing.T) {
	t.Run("entering VERIFY with an assignment", func(t *testing.T) {
		// A CHECKPOINT submit forges the VERIFY entry (a FIX submit would be rejected
		// earlier — the standalone path takes no implementation turn under schema v6).
		store, rev := newRunWithActiveTurn(t)
		cp := &capturedPrep{turn: "forged-verify", apply: func(gen uint64, next *state.RunState) error {
			*next.StepIndex = next.AgreedPlan.Plan.StepCount
			next.Phase = state.PhaseVerify
			next.FixReturn = ""
			next.Verify = &state.VerifyRequirement{RequiredGeneration: 3}
			next.Assignment = &state.Ref{ID: "forged-verify", IssuedRevision: gen}
			bindEvidence(next, gen)
			return nil
		}}
		_, err := Submit(context.Background(), submitDeps(store, newMemSink(), cp.prep()), pairSess, report("turn-1", rev, "fixed"))
		if !errors.Is(err, ErrTransitionInvalid) {
			t.Fatalf("assigned entry err = %v, want ErrTransitionInvalid", err)
		}
	})

	t.Run("clearing an active VERIFY to ownerless", func(t *testing.T) {
		const required = 3
		store, rev := runAtActiveVerify(t, required)
		sess := registryPairGen(t, store, required)
		cp := &capturedPrep{apply: func(_ uint64, next *state.RunState) error {
			next.Assignment = nil // strand the run back at the ownerless wait
			return nil
		}}
		_, err := Submit(context.Background(), submitDeps(store, newMemSink(), cp.prep()), sess, verificationRaw("verify-turn", rev))
		if !errors.Is(err, ErrTransitionInvalid) {
			t.Fatalf("strand err = %v, want ErrTransitionInvalid", err)
		}
	})
}
