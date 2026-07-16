package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// initState populates a valid first-generation run state.
func initState(next *RunState) {
	next.RunID = "run-a"
	next.Lifecycle = LifecycleRunning
	next.Phase = PhaseInit
	next.CreatedUnix = 1000
	next.TaskSnapshot = SnapshotRef{RelPath: "inputs/task.json", Digest: hex64("a")}
	next.PolicySnapshot = SnapshotRef{RelPath: "inputs/policy.json", Digest: hex64("b")}
	next.EffectivePolicy = config.DefaultRunPolicy()
	next.FS = FSResult{Class: "supported-local", Reason: "local fixed drive"}
	next.Base = "main"
	next.BaseCommit = "deadbeefcafe"
}

func mustInit(t *testing.T, s *Store) RunState {
	t.Helper()
	rs, err := s.Mutate(0, func(_ uint64, next *RunState) error { initState(next); return nil })
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return rs
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
		next.Failure = &Projection{Code: "boom", Reason: "leaked token=" + secret, AtRevision: rev}
		return nil
	})
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if r2.Failure == nil || strings.Contains(r2.Failure.Reason, secret) {
		t.Fatalf("returned failure still has secret: %+v", r2.Failure)
	}
	got, _, _ := s.Load()
	if got.Failure.Reason != r2.Failure.Reason {
		t.Fatalf("returned (%q) != loaded (%q)", r2.Failure.Reason, got.Failure.Reason)
	}
	if !strings.Contains(got.Failure.Reason, "[REDACTED]") {
		t.Fatalf("expected a redaction marker in %q", got.Failure.Reason)
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
	_, err := s.Mutate(r1.Revision, func(_ uint64, next *RunState) error {
		next.EffectivePolicy.BaseBranch = "other"
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
	r2, err := s.Mutate(r1.Revision, func(rev uint64, next *RunState) error {
		next.AcceptedTurns["t1"] = AcceptedTurn{ArtifactDigest: hex64("c"), Receipt: Receipt{TurnID: "t1", Revision: rev, ArtifactDigest: hex64("c")}}
		return nil
	})
	if err != nil {
		t.Fatalf("add turn: %v", err)
	}
	if _, err := s.Mutate(r2.Revision, func(_ uint64, next *RunState) error {
		next.AcceptedTurns["t1"] = AcceptedTurn{ArtifactDigest: hex64("d"), Receipt: Receipt{TurnID: "t1", Revision: 2, ArtifactDigest: hex64("d")}}
		return nil
	}); err == nil {
		t.Fatalf("changing an accepted turn should be rejected")
	}
}

func TestRefBindsToResultingRevisionAcrossGap(t *testing.T) {
	s, stateDir := newStoreDir(t)
	r1 := mustInit(t, s)
	// Occupy generation 2 with a torn file so the next append skips to 3.
	if err := os.WriteFile(filepath.Join(stateDir, fmt.Sprintf("%012d.gen", 2)), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("occupy slot: %v", err)
	}
	r3, err := s.Mutate(r1.Revision, func(rev uint64, next *RunState) error {
		next.Assignment = &Ref{ID: "assign-1", IssuedRevision: rev}
		return nil
	})
	if err != nil {
		t.Fatalf("mutate across gap: %v", err)
	}
	if r3.Revision != 3 || r3.Assignment == nil || r3.Assignment.IssuedRevision != 3 {
		t.Fatalf("gap issuance = rev %d assignment %+v, want rev 3 / issued 3", r3.Revision, r3.Assignment)
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
