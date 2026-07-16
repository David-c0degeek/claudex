package state

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

func opid(c string) string   { return "op-" + strings.Repeat(c, 32) }
func sessid(c string) string { return "sess-" + strings.Repeat(c, 32) }

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

func activate(next *CurrentRun, runID, op, sess string) {
	next.Active = true
	next.RunID = runID
	next.RelDir = RunDirRelFor(runID)
	next.OperationID = op
	next.LeadSessionID = sess
	next.LeadAgent = AgentClaude
}

func TestCurrentRunActivateLoad(t *testing.T) {
	s, lock := newCurrentRun(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := s.MutateLocked(g, 0, func(n *CurrentRun) error { activate(n, "run-a", opid("a"), sessid("a")); return nil }); err != nil {
			t.Fatalf("activate: %v", err)
		}
	})
	cur, ok, err := s.Load()
	if err != nil || !ok || !cur.Active || cur.RunID != "run-a" || cur.OperationID != opid("a") {
		t.Fatalf("load = %+v ok=%v err=%v", cur, ok, err)
	}
}

// active -> inactive (clear) then inactive -> active (a new run) is legal; a
// double-set or double-clear is rejected.
func TestCurrentRunTransitions(t *testing.T) {
	s, lock := newCurrentRun(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		r1, err := s.MutateLocked(g, 0, func(n *CurrentRun) error { activate(n, "run-a", opid("a"), sessid("a")); return nil })
		if err != nil {
			t.Fatalf("activate A: %v", err)
		}
		// Activating over an active run is rejected.
		if _, err := s.MutateLocked(g, r1.Revision, func(n *CurrentRun) error { activate(n, "run-b", opid("b"), sessid("b")); return nil }); err == nil {
			t.Fatalf("activating over an active run should be rejected")
		}
		// Clear is legal.
		r2, err := s.MutateLocked(g, r1.Revision, func(n *CurrentRun) error { n.Active = false; return nil })
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		// Double clear is rejected.
		if _, err := s.MutateLocked(g, r2.Revision, func(n *CurrentRun) error { n.Active = false; return nil }); err == nil {
			t.Fatalf("double clear should be rejected")
		}
		// A new run activates after the clear.
		if _, err := s.MutateLocked(g, r2.Revision, func(n *CurrentRun) error { activate(n, "run-b", opid("b"), sessid("b")); return nil }); err != nil {
			t.Fatalf("activate B after clear: %v", err)
		}
	})
}

func TestCurrentRunRejects(t *testing.T) {
	cases := map[string]func(n *CurrentRun){
		"non-derived rel dir":  func(n *CurrentRun) { activate(n, "run-a", opid("a"), sessid("a")); n.RelDir = ".claudex/runs/other" },
		"non-minted operation": func(n *CurrentRun) { activate(n, "run-a", "op-short", sessid("a")) },
		"non-minted session":   func(n *CurrentRun) { activate(n, "run-a", opid("a"), "sess-UPPER") },
		"secret run id": func(n *CurrentRun) {
			activate(n, "run-a", opid("a"), sessid("a"))
			n.RunID = "sk-ant-abcdefghijklmnopqrstuvwx"
		},
		"inactive with identity": func(n *CurrentRun) {
			n.Active = false
			n.RunID = "run-a"
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s, lock := newCurrentRun(t)
			withGuard(t, lock, func(g *genstore.Guard) {
				if _, err := s.MutateLocked(g, 0, func(n *CurrentRun) error { mut(n); return nil }); err == nil {
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
		if _, err := s.MutateLocked(g, 0, func(n *CurrentRun) error { activate(n, "run-a", opid("a"), sessid("a")); return nil }); err != nil {
			t.Fatalf("activate: %v", err)
		}
		if _, err := s.MutateLocked(g, 0, func(n *CurrentRun) error { n.Active = false; return nil }); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("stale CAS err = %v, want ErrRevisionConflict", err)
		}
	})
}

// An older/unknown schema fails decode with version remediation.
func TestCurrentRunRejectsOlderSchema(t *testing.T) {
	old := []byte(`{"schema_version":0,"revision":1,"active":false}`)
	if _, err := decodeCurrentRun(genstore.Record{Generation: 1, Payload: old}); !errors.Is(err, ErrUnsupportedCurrentRunSchema) {
		t.Fatalf("older schema err = %v, want ErrUnsupportedCurrentRunSchema", err)
	}
}
