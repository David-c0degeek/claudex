package transport

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

// --- pure 4b-1 authority helpers ---

func TestCurrentPairGeneration(t *testing.T) {
	sess := "sess-" + fmt.Sprintf("%032x", 7)
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
	fromTests := state.RunState{Phase: state.PhaseTests}

	// Enters ownerless VERIFY with the exact threshold (pair gen 3 -> required 4).
	if err := checkEnterVerifyThreshold(fromTests, &state.RunState{Phase: state.PhaseVerify, Verify: req(4)}, 3); err != nil {
		t.Fatalf("exact threshold: %v", err)
	}
	// A wrong threshold is rejected.
	if err := checkEnterVerifyThreshold(fromTests, &state.RunState{Phase: state.PhaseVerify, Verify: req(5)}, 3); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("wrong threshold: %v", err)
	}
	// A missing requirement is rejected.
	if err := checkEnterVerifyThreshold(fromTests, &state.RunState{Phase: state.PhaseVerify}, 3); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("missing requirement: %v", err)
	}
	// An overflowing pair generation is rejected.
	if err := checkEnterVerifyThreshold(fromTests, &state.RunState{Phase: state.PhaseVerify, Verify: req(1)}, ^uint64(0)); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("overflow: %v", err)
	}
	// NOT entering VERIFY (already there) is a no-op — replacement's later same-phase
	// issuance is not this check's concern.
	if err := checkEnterVerifyThreshold(state.RunState{Phase: state.PhaseVerify}, &state.RunState{Phase: state.PhaseVerify, Verify: req(5)}, 3); err != nil {
		t.Fatalf("same-phase should be a no-op: %v", err)
	}
	// An assignment present means an issued verifier, not a fresh entry — no-op.
	if err := checkEnterVerifyThreshold(fromTests, &state.RunState{Phase: state.PhaseVerify, Assignment: &state.Ref{ID: "x"}, Verify: req(5)}, 3); err != nil {
		t.Fatalf("with an assignment should be a no-op: %v", err)
	}
}

func TestRequireLiveOwnerOwnerlessVerify(t *testing.T) {
	// Ownerless VERIFY: running, no assignment, a requirement present.
	if err := requireLiveOwner(&state.RunState{Phase: state.PhaseVerify, Lifecycle: state.LifecycleRunning, Verify: &state.VerifyRequirement{RequiredGeneration: 2}}, "t", 5); err != nil {
		t.Fatalf("ownerless VERIFY: %v", err)
	}
	// Ownerless VERIFY without a requirement is invalid.
	if err := requireLiveOwner(&state.RunState{Phase: state.PhaseVerify, Lifecycle: state.LifecycleRunning}, "t", 5); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("ownerless VERIFY without a requirement: %v", err)
	}
}

// --- the active-VERIFY fresh-generation submit authorization ---

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
		gen := i
		r, err = reg.Mutate(rev, func(g uint64, n *state.Registry) error {
			s := state.SessionRecord{SessionID: pairSessGen(gen), Generation: gen, IssuedRegistryRevision: g}
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

// runAtActiveVerify drives a one-step agreed plan to an active VERIFY (a verifier turn
// issued, the requirement retained) and returns the store and the VERIFY revision.
func runAtActiveVerify(t *testing.T, requiredGen uint64) (*state.Store, uint64) {
	t.Helper()
	dir := t.TempDir()
	store := state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
	r1, err := store.Mutate(0, func(_ uint64, n *state.RunState) error { initValid(n); return nil })
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	implRev := driveAgreedImplement(t, store, r1.Revision)
	rv, err := store.Mutate(implRev, func(gen uint64, n *state.RunState) error {
		*n.StepIndex = n.AgreedPlan.Plan.StepCount // the cursor sits at the plan end
		n.Phase = state.PhaseVerify
		n.Verify = &state.VerifyRequirement{RequiredGeneration: requiredGen}
		n.Assignment = &state.Ref{ID: "verify-turn", IssuedRevision: gen}
		return nil
	})
	if err != nil {
		t.Fatalf("to active VERIFY: %v", err)
	}
	return store, rv.Revision
}

func verificationRaw(turnID string, rev uint64) []byte {
	return []byte(fmt.Sprintf(`{"protocol_version":1,"message_type":"verification","turn_id":%q,"state_revision":%d,"human_context":null,"requires_human_decision":false,"decision_question":null,"verdict":"pass","criteria":[],"scope_expansion":[],"tests_meaningful":true,"unsupported_claims":[],"notes":"n"}`, turnID, rev))
}

// The incumbent pair generation is a fresh-session rejection before schema/Prepare/sink;
// a generation at or above the retained threshold passes the fresh-session gate.
func TestVerifyFreshGenerationAuth(t *testing.T) {
	const required = 3
	for _, tc := range []struct {
		name    string
		pairGen uint64
		fresh   bool
	}{
		{"required-1 (incumbent)", required - 1, true},
		{"exactly required", required, false},
		{"above required", required + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, rev := runAtActiveVerify(t, required)
			pairSession := registryPairGen(t, store, tc.pairGen)
			adv := &advancer{}
			_, err := Submit(context.Background(), submitDeps(store, newMemSink(), adv.prep()), pairSession, verificationRaw("verify-turn", rev))
			if tc.fresh {
				if !errors.Is(err, ErrFreshSessionRequired) {
					t.Fatalf("incumbent err = %v, want ErrFreshSessionRequired", err)
				}
			} else if errors.Is(err, ErrFreshSessionRequired) {
				t.Fatalf("a qualifying generation must pass the fresh-session gate, got %v", err)
			}
		})
	}
}
