package state

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

func opid(c string) string { return "op-" + strings.Repeat(c, 32) }

func newCurrentRun(t *testing.T) (*CurrentRunStore, string) {
	t.Helper()
	dir := t.TempDir()
	return OpenCurrentRun(filepath.Join(dir, "active-run"), filepath.Join(dir, "run.lock")), filepath.Join(dir, "run.lock")
}

func withGuard(t *testing.T, lock string, fn func(g *genstore.Guard)) {
	t.Helper()
	g, ok, err := genstore.Acquire(lock)
	if err != nil || !ok {
		t.Fatalf("acquire: %v", err)
	}
	defer g.Release()
	fn(g)
}

func TestCurrentRunActivateLoad(t *testing.T) {
	s, lock := newCurrentRun(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := s.Activate(g, 0, "run-a", RunDirRelFor("run-a"), opid("a")); err != nil {
			t.Fatalf("activate: %v", err)
		}
	})
	cur, ok, err := s.Load()
	if err != nil || !ok || !cur.Active || cur.RunID != "run-a" || cur.OperationID != opid("a") {
		t.Fatalf("load = %+v ok=%v err=%v", cur, ok, err)
	}
}

// active -> inactive (clear) then inactive -> active (a new run) is legal; a
// double-set (activate over active) and a mismatched/absent clear are rejected.
func TestCurrentRunTransitions(t *testing.T) {
	s, lock := newCurrentRun(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		r1, err := s.Activate(g, 0, "run-a", RunDirRelFor("run-a"), opid("a"))
		if err != nil {
			t.Fatalf("activate A: %v", err)
		}
		// Activating over an active run is rejected.
		if _, err := s.Activate(g, r1.Revision, "run-b", RunDirRelFor("run-b"), opid("b")); err == nil {
			t.Fatalf("activating over an active run should be rejected")
		}
		// Clearing a run other than the active one is rejected.
		if _, err := s.Clear(g, r1.Revision, "run-b"); !errors.Is(err, ErrCurrentRunMismatch) {
			t.Fatalf("mismatched clear err = %v, want ErrCurrentRunMismatch", err)
		}
		// Clear the active run.
		r2, err := s.Clear(g, r1.Revision, "run-a")
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		// Clearing an already-inactive pointer is rejected.
		if _, err := s.Clear(g, r2.Revision, "run-a"); !errors.Is(err, ErrCurrentRunMismatch) {
			t.Fatalf("double clear err = %v, want ErrCurrentRunMismatch", err)
		}
		// A new run activates after the clear.
		if _, err := s.Activate(g, r2.Revision, "run-b", RunDirRelFor("run-b"), opid("b")); err != nil {
			t.Fatalf("activate B after clear: %v", err)
		}
	})
}

func TestCurrentRunActivateRejects(t *testing.T) {
	cases := map[string]struct {
		runID, relDir, op string
	}{
		"non-derived rel dir":  {"run-a", ".claudex/runs/other", opid("a")},
		"non-minted operation": {"run-a", RunDirRelFor("run-a"), "op-short"},
		"secret run id":        {"sk-ant-abcdefghijklmnopqrstuvwx", RunDirRelFor("sk-ant-abcdefghijklmnopqrstuvwx"), opid("a")},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, lock := newCurrentRun(t)
			withGuard(t, lock, func(g *genstore.Guard) {
				if _, err := s.Activate(g, 0, c.runID, c.relDir, c.op); err == nil {
					t.Fatalf("%s should be rejected", name)
				}
			})
		})
	}
}

// A stale expected revision conflicts (CAS).
func TestCurrentRunCAS(t *testing.T) {
	s, lock := newCurrentRun(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := s.Activate(g, 0, "run-a", RunDirRelFor("run-a"), opid("a")); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if _, err := s.Clear(g, 0, "run-a"); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("stale CAS err = %v, want ErrRevisionConflict", err)
		}
	})
}

// An older/unknown schema fails decode with version remediation, and an inactive
// pointer carrying run identity is rejected at decode.
func TestCurrentRunDecodeRejects(t *testing.T) {
	old := []byte(`{"schema_version":0,"revision":1,"active":false}`)
	if _, err := decodeCurrentRun(genstore.Record{Generation: 1, Payload: old}); !errors.Is(err, ErrUnsupportedCurrentRunSchema) {
		t.Fatalf("older schema err = %v, want ErrUnsupportedCurrentRunSchema", err)
	}
	inactiveWithID := []byte(`{"schema_version":1,"revision":1,"active":false,"run_id":"run-a","rel_dir":"","operation_id":""}`)
	if _, err := decodeCurrentRun(genstore.Record{Generation: 1, Payload: inactiveWithID}); err == nil {
		t.Fatalf("an inactive pointer with run identity should be rejected")
	}
}
