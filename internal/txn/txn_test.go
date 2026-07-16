package txn

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

// fakeStep is a controllable idempotent step.
type fakeStep struct {
	name       string
	applied    bool
	forceState StepStatus // "" => derive from applied
	statusErr  error
	applyErr   error
	applyCalls int
}

func (s *fakeStep) toStep() Step {
	return Step{
		Name: s.name,
		Status: func() (StepStatus, error) {
			if s.statusErr != nil {
				return "", s.statusErr
			}
			if s.forceState != "" {
				return s.forceState, nil
			}
			if s.applied {
				return StatusApplied, nil
			}
			return StatusNotApplied, nil
		},
		Apply: func() error {
			s.applyCalls++
			if s.applyErr != nil {
				return s.applyErr
			}
			s.applied = true
			return nil
		},
	}
}

func intent(txnID string) Intent {
	return Intent{Version: IntentVersion, Kind: "test", TxnID: txnID, ExpectedStateRevision: 1, Payload: json.RawMessage(`{"ref":"refs/x","old":"a","new":"b"}`)}
}

func mkPlan(txnID string, steps ...*fakeStep) Plan {
	p := Plan{Intent: intent(txnID)}
	for _, s := range steps {
		p.Steps = append(p.Steps, s.toStep())
	}
	return p
}

func newJournal(t *testing.T) (*Journal, string) {
	t.Helper()
	dir := t.TempDir()
	lock := filepath.Join(dir, "run.lock")
	return Open(filepath.Join(dir, "txn"), lock), lock
}

func withGuard(t *testing.T, lock string, fn func(g *genstore.Guard)) {
	t.Helper()
	g, ok, err := genstore.Acquire(lock)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	fn(g)
}

func TestRunCompletes(t *testing.T) {
	j, lock := newJournal(t)
	s := []*fakeStep{{name: "result"}, {name: "commit"}, {name: "state"}}
	withGuard(t, lock, func(g *genstore.Guard) {
		rec, err := j.Run(g, mkPlan("t1", s...))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if !rec.Complete || rec.StepsDone != 3 {
			t.Fatalf("run result = %+v, want complete/3", rec)
		}
	})
	for _, st := range s {
		if st.applyCalls != 1 || !st.applied {
			t.Fatalf("step %s applyCalls=%d applied=%v", st.name, st.applyCalls, st.applied)
		}
	}
}

func TestRunStopsOnStepFailureNotTerminal(t *testing.T) {
	j, lock := newJournal(t)
	s0 := &fakeStep{name: "s0"}
	s1 := &fakeStep{name: "s1", applyErr: errors.New("boom")}
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, mkPlan("t1", s0, s1)); err == nil {
			t.Fatalf("run should fail at s1")
		}
	})
	cur, ok, _ := j.Latest()
	if !ok || cur.Terminal() || cur.StepsDone != 1 {
		t.Fatalf("after failure = %+v ok=%v, want non-terminal steps_done=1", cur, ok)
	}
}

func TestRecoverForwardAfterFailure(t *testing.T) {
	j, lock := newJournal(t)
	s0 := &fakeStep{name: "s0"}
	s1 := &fakeStep{name: "s1", applyErr: errors.New("boom")}
	withGuard(t, lock, func(g *genstore.Guard) { j.Run(g, mkPlan("t1", s0, s1)) })

	s1.applyErr = nil // the transient failure is gone
	withGuard(t, lock, func(g *genstore.Guard) {
		out, acted, err := j.Recover(g, func(Intent) (Plan, error) { return mkPlan("t1", s0, s1), nil })
		if err != nil || !acted || !out.Complete {
			t.Fatalf("recover = %+v acted=%v err=%v", out, acted, err)
		}
	})
	if !s1.applied {
		t.Fatalf("s1 was not applied on recovery")
	}
}

// The exact cut the redesign fixes: an effect became durable but its progress
// record was not written (crash between apply and progress). Recovery must see the
// step as applied and advance forward WITHOUT re-applying, reaching completion.
func TestRecoverAdvancesAppliedButUnrecordedStep(t *testing.T) {
	j, lock := newJournal(t)
	s0 := &fakeStep{name: "s0"}
	s1 := &fakeStep{name: "s1", applyErr: errors.New("crash-before-progress")}
	withGuard(t, lock, func(g *genstore.Guard) { j.Run(g, mkPlan("t1", s0, s1)) })
	if s1.applyCalls != 1 {
		t.Fatalf("s1 applyCalls = %d, want 1 (the failed attempt)", s1.applyCalls)
	}
	// The effect actually landed; only the progress record was lost.
	s1.applyErr = nil
	s1.applied = true
	withGuard(t, lock, func(g *genstore.Guard) {
		out, _, err := j.Recover(g, func(Intent) (Plan, error) { return mkPlan("t1", s0, s1), nil })
		if err != nil || !out.Complete {
			t.Fatalf("recover = %+v err=%v, want complete", out, err)
		}
	})
	if s1.applyCalls != 1 {
		t.Fatalf("s1 was re-applied on recovery (applyCalls=%d)", s1.applyCalls)
	}
}

func TestRecoverIndeterminateFailsClosed(t *testing.T) {
	j, lock := newJournal(t)
	s0 := &fakeStep{name: "s0", applyErr: errors.New("stop")}
	withGuard(t, lock, func(g *genstore.Guard) { j.Run(g, mkPlan("t1", s0)) })

	s0.applyErr = nil
	s0.forceState = StatusIndeterminate
	withGuard(t, lock, func(g *genstore.Guard) {
		_, _, err := j.Recover(g, func(Intent) (Plan, error) { return mkPlan("t1", s0), nil })
		if !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("indeterminate recover err = %v, want ErrRecoveryRequired", err)
		}
	})
	if cur, _, _ := j.Latest(); cur.Terminal() {
		t.Fatalf("indeterminate transaction was wrongly terminalized")
	}
}

func TestRecoverSkipsWhenTerminal(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		j.Run(g, mkPlan("t1", &fakeStep{name: "s0"}))
		if _, acted, err := j.Recover(g, func(Intent) (Plan, error) { return mkPlan("t1", &fakeStep{name: "s0", applied: true}), nil }); err != nil || acted {
			t.Fatalf("recover on complete: acted=%v err=%v, want no action", acted, err)
		}
	})
}

func TestAbortOnlyBeforeAnyStep(t *testing.T) {
	j, lock := newJournal(t)
	// Prepared with zero applied steps: abort allowed.
	s0 := &fakeStep{name: "s0", applyErr: errors.New("stop")}
	withGuard(t, lock, func(g *genstore.Guard) {
		j.Run(g, mkPlan("t1", s0))
		out, err := j.AbortLocked(g, "t1")
		if err != nil || !out.Aborted {
			t.Fatalf("abort at step 0 = %+v err=%v", out, err)
		}
	})
	// A transaction with an applied step cannot be aborted.
	j2, lock2 := newJournal(t)
	a := &fakeStep{name: "a"}
	b := &fakeStep{name: "b", applyErr: errors.New("stop")}
	withGuard(t, lock2, func(g *genstore.Guard) {
		j2.Run(g, mkPlan("t2", a, b)) // stops with steps_done=1
		if _, err := j2.AbortLocked(g, "t2"); !errors.Is(err, ErrCannotAbort) {
			t.Fatalf("abort after a step err = %v, want ErrCannotAbort", err)
		}
	})
}

func TestPrepareWhilePendingRejected(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		j.Run(g, mkPlan("t1", &fakeStep{name: "s0", applyErr: errors.New("stop")}))
		if _, err := j.Run(g, mkPlan("t2", &fakeStep{name: "x"})); !errors.Is(err, ErrPending) {
			t.Fatalf("run while pending err = %v, want ErrPending", err)
		}
	})
}

func TestNewTransactionAfterCompletion(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		j.Run(g, mkPlan("t1", &fakeStep{name: "s0"}))
		out, err := j.Run(g, mkPlan("t2", &fakeStep{name: "s0"}))
		if err != nil || !out.Complete || out.TxnID() != "t2" {
			t.Fatalf("second txn = %+v err=%v", out, err)
		}
	})
}

func TestPlanMismatchRejected(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		j.Run(g, mkPlan("t1", &fakeStep{name: "s0", applyErr: errors.New("stop")}))
		_, _, err := j.Recover(g, func(Intent) (Plan, error) {
			return mkPlan("t1", &fakeStep{name: "s0"}, &fakeStep{name: "extra"}), nil // wrong step count
		})
		if !errors.Is(err, ErrPlanMismatch) {
			t.Fatalf("plan mismatch err = %v, want ErrPlanMismatch", err)
		}
	})
}

func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "run.lock")
	jdir := filepath.Join(dir, "txn")
	s0 := &fakeStep{name: "s0"}
	s1 := &fakeStep{name: "s1", applyErr: errors.New("crash")}

	withGuard(t, lock, func(g *genstore.Guard) { Open(jdir, lock).Run(g, mkPlan("t1", s0, s1)) })

	s1.applyErr = nil
	j2 := Open(jdir, lock) // fresh handle == restart
	if cur, ok, _ := j2.Latest(); !ok || cur.Terminal() {
		t.Fatalf("pending transaction did not persist across restart")
	}
	withGuard(t, lock, func(g *genstore.Guard) {
		out, acted, err := j2.Recover(g, func(Intent) (Plan, error) { return mkPlan("t1", s0, s1), nil })
		if err != nil || !acted || !out.Complete {
			t.Fatalf("restart recover = %+v acted=%v err=%v", out, acted, err)
		}
	})
}

func TestSecretInPayloadRejected(t *testing.T) {
	j, lock := newJournal(t)
	in := intent("t1")
	in.Payload = json.RawMessage(`{"cmd":"deploy token=sk-ant-abcdefghijklmnopqrstuvwx"}`)
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, Plan{Intent: in, Steps: []Step{(&fakeStep{name: "s0"}).toStep()}}); err == nil {
			t.Fatalf("a secret in the payload should be rejected")
		}
	})
}

func TestValidateTransitionRejectsIllegalMoves(t *testing.T) {
	base := Record{SchemaVersion: RecordVersion, Revision: 1, Intent: intent("t1"), TotalSteps: 3, StepsDone: 1}
	// skipping a step
	skip := base
	skip.StepsDone = 3
	if err := validateTransition(base, skip); err == nil {
		t.Fatalf("skipping steps should be rejected")
	}
	// completing before all steps done
	early := base
	early.StepsDone = 2
	early.Complete = true
	if err := validate(early); err == nil {
		t.Fatalf("completing before all steps should be rejected")
	}
	// advancing from a terminal record
	term := base
	term.StepsDone = 3
	term.Complete = true
	if err := validateTransition(term, base); err == nil {
		t.Fatalf("advancing from a terminal record should be rejected")
	}
}
