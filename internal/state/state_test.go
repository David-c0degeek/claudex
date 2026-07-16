package state

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
}

func TestMutateInitAndLoad(t *testing.T) {
	s := newStore(t)
	if _, ok, err := s.Load(); err != nil || ok {
		t.Fatalf("empty load: ok=%v err=%v", ok, err)
	}
	rs, err := s.Mutate(0, func(rs *RunState) error {
		rs.RunID = "r1"
		rs.Lifecycle = LifecycleRunning
		rs.Phase = "PLAN_DRAFT"
		return nil
	})
	if err != nil {
		t.Fatalf("init mutate: %v", err)
	}
	if rs.Revision != 1 || rs.RunID != "r1" || rs.SchemaVersion != RunStateVersion {
		t.Fatalf("init state = %+v", rs)
	}
	got, ok, err := s.Load()
	if err != nil || !ok || got.Revision != 1 || got.Phase != "PLAN_DRAFT" {
		t.Fatalf("load = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestMutateCASAdvancesRevision(t *testing.T) {
	s := newStore(t)
	r1, _ := s.Mutate(0, func(rs *RunState) error { rs.RunID = "r"; rs.Phase = "PLAN_DRAFT"; return nil })
	r2, err := s.Mutate(r1.Revision, func(rs *RunState) error { rs.Phase = "IMPLEMENT_STEP"; return nil })
	if err != nil {
		t.Fatalf("second mutate: %v", err)
	}
	if r2.Revision != 2 || r2.Phase != "IMPLEMENT_STEP" || r2.RunID != "r" {
		t.Fatalf("r2 = %+v", r2)
	}
}

func TestMutateStaleRevisionConflicts(t *testing.T) {
	s := newStore(t)
	r1, _ := s.Mutate(0, func(rs *RunState) error { rs.RunID = "r"; return nil })
	s.Mutate(r1.Revision, func(rs *RunState) error { rs.Phase = "X"; return nil }) // head is now revision 2
	if _, err := s.Mutate(r1.Revision, func(rs *RunState) error { return nil }); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale mutate err = %v, want ErrRevisionConflict", err)
	}
}

func TestMutateRedactsBeforePersist(t *testing.T) {
	s := newStore(t)
	secret := "sk-ant-abcdefghijklmnopqrstuvwx"
	rs, err := s.Mutate(0, func(rs *RunState) error {
		rs.RunID = "r"
		rs.EffectivePolicy.TestGate.Command = "deploy token=" + secret
		return nil
	})
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	// The returned in-memory value keeps the caller's data; what matters is that
	// the PERSISTED (and reloaded) value is redacted.
	_ = rs
	got, _, err := s.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Contains(got.EffectivePolicy.TestGate.Command, secret) {
		t.Fatalf("secret survived persistence: %q", got.EffectivePolicy.TestGate.Command)
	}
	if !strings.Contains(got.EffectivePolicy.TestGate.Command, "[REDACTED]") {
		t.Fatalf("expected a redaction marker, got %q", got.EffectivePolicy.TestGate.Command)
	}
}

func TestAcceptedTurnsRoundTrip(t *testing.T) {
	s := newStore(t)
	rs, err := s.Mutate(0, func(rs *RunState) error {
		rs.RunID = "r"
		rs.AcceptedTurns = map[string]AcceptedTurn{
			"turn-1": {ArtifactDigest: "d1", Receipt: Receipt{TurnID: "turn-1", Revision: 1, ArtifactDigest: "d1"}},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	_ = rs
	got, _, _ := s.Load()
	at, ok := got.AcceptedTurns["turn-1"]
	if !ok || at.ArtifactDigest != "d1" || at.Receipt.Revision != 1 {
		t.Fatalf("accepted turn round-trip = %+v ok=%v", at, ok)
	}
}

func TestMutateLockedComposes(t *testing.T) {
	s := newStore(t)
	g, ok, err := genstore.Acquire(s.LockPath())
	if err != nil || !ok {
		t.Fatalf("acquire: %v", err)
	}
	defer g.Release()
	r1, err := s.MutateLocked(g, 0, func(rs *RunState) error { rs.RunID = "r"; return nil })
	if err != nil {
		t.Fatalf("mutate 1: %v", err)
	}
	r2, err := s.MutateLocked(g, r1.Revision, func(rs *RunState) error { rs.Phase = "P"; return nil })
	if err != nil {
		t.Fatalf("mutate 2 in same section: %v", err)
	}
	if r2.Revision != 2 {
		t.Fatalf("revision = %d, want 2", r2.Revision)
	}
}
