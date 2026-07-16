package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/genstore"
)

func newStoreDir(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	return Open(stateDir, filepath.Join(dir, "run.lock")), stateDir
}

func newStore(t *testing.T) *Store {
	s, _ := newStoreDir(t)
	return s
}

func hex64(c string) string { return strings.Repeat(c, 64) }

func hex40() string { return strings.Repeat("a", 40) }

// initState populates a valid first-generation run state.
func initState(next *RunState) {
	next.RunID = "run-a"
	next.Lifecycle = LifecycleRunning
	next.Phase = PhaseInit
	next.CreatedUnix = 1000
	next.TaskSnapshot = SnapshotRef{RelPath: "inputs/task.json", Digest: hex64("a")}
	next.PolicySnapshot = SnapshotRef{RelPath: "inputs/policy.json", Digest: hex64("b")}
	pol := config.DefaultRunPolicy()
	pol.TestGate = config.TestGate{Disabled: true} // explicit, per state boundary
	next.EffectivePolicy = pol
	next.FS = FSResult{Class: "supported-local", Reason: "local fixed drive"}
	next.Base = pol.BaseBranch // == effective policy base branch
	next.BaseCommit = hex40()
	next.WorktreeRelPath = ".claudex/runs/run-a/worktree"
	next.RunBranch = "claudex/run-a"
}

func mustInit(t *testing.T, s *Store) RunState {
	t.Helper()
	rs, err := s.Mutate(0, func(_ uint64, next *RunState) error { initState(next); return nil })
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return rs
}

// assignAgentTurn drives a run from its pristine INIT (init) through the real plan
// negotiation to a 1-step agreed plan at IMPLEMENT_STEP, then assigns turnID there
// — the pre-transition shape a real submit consumes. The v5 shape requires an
// agreed plan to be at any implementation phase, so this is no longer a single
// generation; callers that care about the resulting revision read it from the
// returned state, not a hardcoded number.
func assignAgentTurn(t *testing.T, s *Store, init RunState, turnID string) RunState {
	t.Helper()
	impl := driveToAgreedImplement(t, s, init)
	return assignAt(t, s, impl, turnID)
}

func TestInitAndLoad(t *testing.T) {
	s := newStore(t)
	rs := mustInit(t, s)
	if rs.Revision != 1 || rs.RunID != "run-a" {
		t.Fatalf("init state = %+v", rs)
	}
	got, ok, err := s.Load()
	if err != nil || !ok || got.Revision != 1 {
		t.Fatalf("load = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestCASAdvancesRevision(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	r2, err := s.Mutate(r1.Revision, func(_ uint64, next *RunState) error { next.Phase = PhasePlanDraft; return nil })
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if r2.Revision != 2 || r2.Phase != PhasePlanDraft {
		t.Fatalf("r2 = %+v", r2)
	}
}

func TestStaleRevisionConflicts(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	s.Mutate(r1.Revision, func(_ uint64, next *RunState) error { next.Phase = PhasePlanDraft; return nil })
	if _, err := s.Mutate(r1.Revision, func(_ uint64, next *RunState) error { return nil }); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale err = %v, want ErrRevisionConflict", err)
	}
}

func TestFreeTextRedactedAndReturnEqualsLoad(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	secret := "sk-ant-abcdefghijklmnopqrstuvwx"
	r2, err := s.Mutate(r1.Revision, func(rev uint64, next *RunState) error {
		next.Failure = &Projection{Code: "boom", Reason: "leaked token=" + secret, NextAction: "inspect", AtRevision: rev}
		return nil
	})
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if r2.Failure == nil || strings.Contains(r2.Failure.Reason, secret) {
		t.Fatalf("returned failure still has secret: %+v", r2.Failure)
	}
	got, _, _ := s.Load()
	if !reflect.DeepEqual(r2, got) {
		t.Fatalf("returned state != loaded state:\n%+v\n%+v", r2, got)
	}
	if !strings.Contains(got.Failure.Reason, "[REDACTED]") {
		t.Fatalf("expected a redaction marker in %q", got.Failure.Reason)
	}
}

// A callback that changes the revision or schema version is rejected before any
// generation is written (it would otherwise brick the head post-commit).
func TestMutateRejectsRevisionPoisoning(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	if _, err := s.Mutate(r1.Revision, func(_ uint64, next *RunState) error {
		next.Revision = 999
		return nil
	}); err == nil {
		t.Fatalf("a callback that changes the revision should be rejected")
	}
	if got, _, _ := s.Load(); got.Revision != r1.Revision {
		t.Fatalf("a generation was written despite revision poisoning (rev=%d)", got.Revision)
	}
}

func TestNewReceiptMustBindToRevision(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	if _, err := s.Mutate(r1.Revision, func(rev uint64, next *RunState) error {
		next.AcceptedTurns["t1"] = AcceptedTurn{ArtifactDigest: hex64("c"), Receipt: Receipt{TurnID: "t1", Revision: 1, ArtifactDigest: hex64("c")}, Phase: PhaseInit}
		return nil
	}); err == nil {
		t.Fatalf("a new receipt with a stale revision should be rejected")
	}
}

func TestDefaultPolicyWithoutTestGateRejected(t *testing.T) {
	s := newStore(t)
	_, err := s.Mutate(0, func(_ uint64, next *RunState) error {
		initState(next)
		next.EffectivePolicy.TestGate = config.TestGate{}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "test_gate") {
		t.Fatalf("err = %v, want a test-gate rejection", err)
	}
}

func TestUnknownFSWithoutAckRejected(t *testing.T) {
	s := newStore(t)
	if _, err := s.Mutate(0, func(_ uint64, next *RunState) error {
		initState(next)
		next.FS = FSResult{Class: "unknown", Reason: "possible sync root", Acknowledged: false}
		return nil
	}); err == nil {
		t.Fatalf("unknown FS without acknowledge should be rejected")
	}
}

func TestSecretTurnIDRejected(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	secretID := "sk-ant-abcdefghijklmnopqrstuvwx"
	if _, err := s.Mutate(r1.Revision, func(rev uint64, next *RunState) error {
		next.AcceptedTurns[secretID] = AcceptedTurn{ArtifactDigest: hex64("c"), Receipt: Receipt{TurnID: secretID, Revision: rev, ArtifactDigest: hex64("c")}, Phase: PhaseInit}
		return nil
	}); err == nil {
		t.Fatalf("a secret-shaped turn id should be rejected")
	}
	if got, _, _ := s.Load(); got.Revision != r1.Revision {
		t.Fatalf("a generation was written for a secret turn id")
	}
}

func TestPresentZeroProjectionRejected(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	if _, err := s.Mutate(r1.Revision, func(rev uint64, next *RunState) error {
		next.Recovery = &Projection{}
		return nil
	}); err == nil {
		t.Fatalf("a present all-zero projection should be rejected")
	}
}

func TestIncoherentTimesRejected(t *testing.T) {
	s := newStore(t)
	if _, err := s.Mutate(0, func(_ uint64, next *RunState) error {
		initState(next)
		next.StartedUnix = 5
		next.DeadlineUnix = 3
		return nil
	}); err == nil {
		t.Fatalf("deadline before start should be rejected")
	}
}

func TestBasePolicyMismatchRejected(t *testing.T) {
	s := newStore(t)
	if _, err := s.Mutate(0, func(_ uint64, next *RunState) error {
		initState(next)
		next.Base = "different"
		return nil
	}); err == nil {
		t.Fatalf("base/base_branch mismatch should be rejected")
	}
}

func TestSecretInControlFieldRejected(t *testing.T) {
	s := newStore(t)
	_, err := s.Mutate(0, func(_ uint64, next *RunState) error {
		initState(next)
		next.EffectivePolicy.TestGate.Command = "deploy token=sk-ant-abcdefghijklmnopqrstuvwx"
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want a secret-in-control rejection", err)
	}
	if _, ok, _ := s.Load(); ok {
		t.Fatalf("a generation was written despite the rejection")
	}
}

func TestImmutablePolicyRejected(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	// Change a policy field that is still individually valid, so the transition
	// immutability check is what rejects it.
	_, err := s.Mutate(r1.Revision, func(_ uint64, next *RunState) error {
		next.EffectivePolicy.Limits.MaxWallSeconds = 999
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("err = %v, want an immutability rejection", err)
	}
}

func TestCountersMustNotDecrease(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	r2, err := s.Mutate(r1.Revision, func(_ uint64, next *RunState) error { next.Counters.PlanRevisions = 2; return nil })
	if err != nil {
		t.Fatalf("increase: %v", err)
	}
	if _, err := s.Mutate(r2.Revision, func(_ uint64, next *RunState) error { next.Counters.PlanRevisions = 1; return nil }); err == nil {
		t.Fatalf("decreasing a counter should be rejected")
	}
}

func TestAcceptedTurnImmutable(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	r1b := assignAgentTurn(t, s, r1, "t1")
	r2, err := s.Mutate(r1b.Revision, func(rev uint64, next *RunState) error {
		next.AcceptedTurns["t1"] = AcceptedTurn{ArtifactDigest: hex64("c"), Receipt: Receipt{TurnID: "t1", Revision: rev, ArtifactDigest: hex64("c")}, Phase: PhaseImplementStep}
		next.Assignment = nil
		return nil
	})
	if err != nil {
		t.Fatalf("add turn: %v", err)
	}
	if _, err := s.Mutate(r2.Revision, func(_ uint64, next *RunState) error {
		next.AcceptedTurns["t1"] = AcceptedTurn{ArtifactDigest: hex64("d"), Receipt: Receipt{TurnID: "t1", Revision: 2, ArtifactDigest: hex64("d")}, Phase: PhaseImplementStep}
		return nil
	}); err == nil {
		t.Fatalf("changing an accepted turn should be rejected")
	}
}

func TestRefBindsToResultingRevisionAcrossGap(t *testing.T) {
	s, stateDir := newStoreDir(t)
	r1 := mustInit(t, s)
	r1b := assignAgentTurn(t, s, r1, "t1") // IMPLEMENT_STEP, t1 assigned
	// Occupy the next generation with a torn file so the append skips over it.
	torn := r1b.Revision + 1
	skippedTo := r1b.Revision + 2
	if err := os.WriteFile(filepath.Join(stateDir, fmt.Sprintf("%012d.gen", torn)), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("occupy slot: %v", err)
	}
	r4, err := s.Mutate(r1b.Revision, func(rev uint64, next *RunState) error {
		next.Assignment = &Ref{ID: "assign-1", IssuedRevision: rev} // issued in the skipped-to gen
		next.AcceptedTurns["t1"] = AcceptedTurn{ArtifactDigest: hex64("c"), Receipt: Receipt{TurnID: "t1", Revision: rev, ArtifactDigest: hex64("c")}, Phase: PhaseImplementStep}
		return nil
	})
	if err != nil {
		t.Fatalf("mutate across gap: %v", err)
	}
	if r4.Revision != skippedTo || r4.Assignment == nil || r4.Assignment.IssuedRevision != skippedTo {
		t.Fatalf("gap issuance = rev %d assignment %+v, want rev %d / issued %d", r4.Revision, r4.Assignment, skippedTo, skippedTo)
	}
	if r4.AcceptedTurns["t1"].Receipt.Revision != skippedTo {
		t.Fatalf("accepted-turn receipt revision = %d, want the skipped-to generation %d", r4.AcceptedTurns["t1"].Receipt.Revision, skippedTo)
	}
}

func TestStrictDecodeRejectsUnknownField(t *testing.T) {
	if _, err := strictDecodeRunState([]byte(`{"schema_version":1,"nope":1}`)); err == nil {
		t.Fatalf("unknown field should be rejected")
	}
	if _, err := strictDecodeRunState([]byte(`{"schema_version":1} trailing`)); err == nil {
		t.Fatalf("trailing content should be rejected")
	}
}

// A pre-pivot Python state.json is never reinterpreted as an attach run: its
// legacy-only fields fail the strict attach decoder. The friendly
// operator-facing refusal lives in internal/legacy.
func TestStrictDecodeRejectsLegacyPythonState(t *testing.T) {
	legacy := []byte(`{"run_id":"r","repo":"/p","lead":"claude","driver":"headless","phase":"init"}`)
	if _, err := strictDecodeRunState(legacy); err == nil {
		t.Fatalf("a legacy Python state must be rejected by the attach decoder")
	}
}

// An attach state carrying an unknown/future schema version fails closed.
func TestValidateRejectsUnknownSchemaVersion(t *testing.T) {
	s := newStore(t)
	rs := mustInit(t, s)
	rs.SchemaVersion = RunStateVersion + 1
	if err := validate(&rs); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("unknown schema_version err = %v, want a schema_version rejection", err)
	}
}

// A generation written by the older v1 schema (accepted turns recorded only the
// artifact digest and receipt, with no authoritative phase) fails on decode with
// clear version remediation, not a vague missing-field corruption error.
func TestDecodeRejectsOlderSchemaVersion(t *testing.T) {
	old := []byte(`{"schema_version":1,"run_id":"r","revision":2,"accepted_turns":{"t1":{"artifact_digest":"` + hex64("c") + `","receipt":{"turn_id":"t1","revision":2,"artifact_digest":"` + hex64("c") + `"}}}}`)
	if _, err := decodeRunState(genstore.Record{Generation: 2, Payload: old}); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("older schema decode err = %v, want ErrUnsupportedSchema", err)
	}
}

// Acceptance requires a live run: a cancelled/failed/paused or recovering run
// cannot accept a turn even if it still names an assigned agent turn.
func TestAcceptRequiresLiveRun(t *testing.T) {
	acceptT1 := func(rev uint64, next *RunState) error {
		next.AcceptedTurns["t1"] = AcceptedTurn{ArtifactDigest: hex64("c"), Receipt: Receipt{TurnID: "t1", Revision: rev, ArtifactDigest: hex64("c")}, Phase: PhaseImplementStep}
		return nil
	}

	t.Run("cancelled", func(t *testing.T) {
		s := newStore(t)
		r1b := assignAgentTurn(t, s, mustInit(t, s), "t1")
		// Force a non-running lifecycle while the assignment lingers (state allows
		// it; the transport owner check would not).
		rc, err := s.Mutate(r1b.Revision, func(_ uint64, next *RunState) error {
			next.Lifecycle = LifecycleCancelled
			return nil
		})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := s.Mutate(rc.Revision, acceptT1); err == nil {
			t.Fatalf("accepting on a cancelled run should be rejected")
		}
	})

	t.Run("recovering", func(t *testing.T) {
		s := newStore(t)
		r1b := assignAgentTurn(t, s, mustInit(t, s), "t1")
		rr, err := s.Mutate(r1b.Revision, func(rev uint64, next *RunState) error {
			next.Recovery = &Projection{Code: "resync", Reason: "reattached", NextAction: "await", AtRevision: rev}
			return nil
		})
		if err != nil {
			t.Fatalf("set recovery: %v", err)
		}
		if _, err := s.Mutate(rr.Revision, acceptT1); err == nil {
			t.Fatalf("accepting on a recovering run should be rejected")
		}
	})

	t.Run("stale assignment", func(t *testing.T) {
		s := newStore(t)
		r1b := assignAgentTurn(t, s, mustInit(t, s), "t1") // t1 issued at r1b.Revision
		// Advance the revision with an unrelated change, leaving t1 assigned but now
		// issued at a stale revision — no valid pull could have produced this.
		r2, err := s.Mutate(r1b.Revision, func(_ uint64, next *RunState) error {
			next.Counters.PlanRevisions++
			return nil
		})
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if _, err := s.Mutate(r2.Revision, acceptT1); err == nil {
			t.Fatalf("accepting a stale (old-revision) assignment should be rejected")
		}
	})
}

// Acceptance can only record a real outstanding turn: never in INIT (no
// assignment), never a turn other than the assigned one.
func TestAcceptRequiresAssignedAgentTurn(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	// INIT with no assignment: acceptance is rejected.
	if _, err := s.Mutate(r1.Revision, func(rev uint64, next *RunState) error {
		next.AcceptedTurns["t1"] = AcceptedTurn{ArtifactDigest: hex64("c"), Receipt: Receipt{TurnID: "t1", Revision: rev, ArtifactDigest: hex64("c")}, Phase: PhaseInit}
		return nil
	}); err == nil {
		t.Fatalf("accepting a turn in INIT with no assignment should be rejected")
	}
	// A different turn than the one assigned: rejected.
	r1b := assignAgentTurn(t, s, r1, "t1")
	if _, err := s.Mutate(r1b.Revision, func(rev uint64, next *RunState) error {
		next.AcceptedTurns["other"] = AcceptedTurn{ArtifactDigest: hex64("c"), Receipt: Receipt{TurnID: "other", Revision: rev, ArtifactDigest: hex64("c")}, Phase: PhaseImplementStep}
		return nil
	}); err == nil {
		t.Fatalf("accepting a turn other than the assigned one should be rejected")
	}
}

func TestMutateLockedComposes(t *testing.T) {
	s := newStore(t)
	g, ok, err := genstore.Acquire(s.LockPath())
	if err != nil || !ok {
		t.Fatalf("acquire: %v", err)
	}
	defer g.Release()
	r1, err := s.MutateLocked(g, 0, func(_ uint64, next *RunState) error { initState(next); return nil })
	if err != nil {
		t.Fatalf("mutate 1: %v", err)
	}
	r2, err := s.MutateLocked(g, r1.Revision, func(_ uint64, next *RunState) error { next.Phase = PhasePlanDraft; return nil })
	if err != nil {
		t.Fatalf("mutate 2: %v", err)
	}
	if r2.Revision != 2 {
		t.Fatalf("revision = %d, want 2", r2.Revision)
	}
}

// FirstTurn is write-once issuance history: it may only appear on the exact
// INIT->PLAN_DRAFT transition equal to the issued assignment, and is immutable.
func TestFirstTurnIssuance(t *testing.T) {
	s := newStore(t)
	r1 := mustInit(t, s)
	r2, err := s.Mutate(r1.Revision, func(gen uint64, next *RunState) error {
		next.Phase = PhasePlanDraft
		next.Assignment = &Ref{ID: "turn-1", IssuedRevision: gen}
		next.FirstTurn = &Ref{ID: "turn-1", IssuedRevision: gen}
		next.StartedUnix = 2000
		next.DeadlineUnix = 2000 + next.EffectivePolicy.Limits.MaxWallSeconds
		return nil
	})
	if err != nil {
		t.Fatalf("valid first-turn issuance: %v", err)
	}
	if r2.FirstTurn == nil || r2.FirstTurn.ID != "turn-1" {
		t.Fatalf("first_turn not recorded: %+v", r2.FirstTurn)
	}
	// Immutable: cannot change or clear it.
	if _, err := s.Mutate(r2.Revision, func(_ uint64, next *RunState) error {
		next.FirstTurn = &Ref{ID: "turn-2", IssuedRevision: 2}
		return nil
	}); err == nil {
		t.Fatalf("changing first_turn should be rejected")
	}
	if _, err := s.Mutate(r2.Revision, func(_ uint64, next *RunState) error { next.FirstTurn = nil; return nil }); err == nil {
		t.Fatalf("clearing first_turn should be rejected")
	}
}

func TestFirstTurnIssuanceRejections(t *testing.T) {
	cases := map[string]func(gen uint64, next *RunState){
		"wrong phase": func(gen uint64, next *RunState) {
			next.Phase = PhaseImplementStep
			next.Assignment = &Ref{ID: "turn-1", IssuedRevision: gen}
			next.FirstTurn = &Ref{ID: "turn-1", IssuedRevision: gen}
			next.StartedUnix = 2000
			next.DeadlineUnix = 2000 + next.EffectivePolicy.Limits.MaxWallSeconds
		},
		"first_turn != assignment": func(gen uint64, next *RunState) {
			next.Phase = PhasePlanDraft
			next.Assignment = &Ref{ID: "turn-1", IssuedRevision: gen}
			next.FirstTurn = &Ref{ID: "turn-other", IssuedRevision: gen}
			next.StartedUnix = 2000
			next.DeadlineUnix = 2000 + next.EffectivePolicy.Limits.MaxWallSeconds
		},
		"first_turn without clock": func(gen uint64, next *RunState) {
			next.Phase = PhasePlanDraft
			next.Assignment = &Ref{ID: "turn-1", IssuedRevision: gen}
			next.FirstTurn = &Ref{ID: "turn-1", IssuedRevision: gen}
		},
		"first_turn in INIT": func(gen uint64, next *RunState) {
			next.FirstTurn = &Ref{ID: "turn-1", IssuedRevision: gen}
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			r1 := mustInit(t, s)
			if _, err := s.Mutate(r1.Revision, func(gen uint64, next *RunState) error { mut(gen, next); return nil }); err == nil {
				t.Fatalf("%s should be rejected", name)
			}
		})
	}
}

// FirstTurn may only be issued from a pristine, unassigned, running INIT — a
// pre-set clock, a pre-existing assignment, or a non-running INIT is rejected.
func TestFirstTurnRequiresPristineInit(t *testing.T) {
	issue := func(gen uint64, next *RunState) {
		next.Phase = PhasePlanDraft
		next.Assignment = &Ref{ID: "turn-1", IssuedRevision: gen}
		next.FirstTurn = &Ref{ID: "turn-1", IssuedRevision: gen}
		next.StartedUnix = 2000
		next.DeadlineUnix = 2000 + next.EffectivePolicy.Limits.MaxWallSeconds
	}
	cases := map[string]func(rev uint64, next *RunState){
		"pre-set clock": func(rev uint64, next *RunState) {
			next.StartedUnix = 2000
			next.DeadlineUnix = 2000 + next.EffectivePolicy.Limits.MaxWallSeconds
		},
		"pre-existing assignment": func(rev uint64, next *RunState) {
			next.Assignment = &Ref{ID: "turn-early", IssuedRevision: rev}
		},
		"non-running init": func(_ uint64, next *RunState) {
			next.Lifecycle = LifecycleCancelled
		},
	}
	for name, taint := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			r1 := mustInit(t, s)
			// Taint the INIT state (staying INIT), then attempt the issuance.
			rt, err := s.Mutate(r1.Revision, func(rev uint64, next *RunState) error { taint(rev, next); return nil })
			if err != nil {
				t.Fatalf("taint: %v", err)
			}
			if _, err := s.Mutate(rt.Revision, func(gen uint64, next *RunState) error { issue(gen, next); return nil }); err == nil {
				t.Fatalf("first_turn issuance from a %s should be rejected", name)
			}
		})
	}
}
