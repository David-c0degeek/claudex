package txn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/genstore"
)

// The ordered durable cuts of a git-shaped transaction.
var canonicalSteps = []string{"result", "commit", "refCAS", "index", "stateCAS", "receipt", "ledger"}

type fakeStep struct {
	name                string
	applied             bool
	forceState          StepStatus
	statusErr           error
	applyErr            error
	applyCalls          int
	doubleApply         bool
	noObserveAfterApply bool
	confirmErr          error // when set, ConfirmDurable fails
	confirmFailUntil    int   // fail the first N ConfirmDurable calls with confirmErr, then succeed (0 = persistent)
	confirmCalls        int
	applyDurErr         error // Apply marks the effect visible (applied), then returns this durability error
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
			if s.applied {
				s.doubleApply = true
			}
			if !s.noObserveAfterApply {
				s.applied = true
			}
			if s.applyDurErr != nil {
				return s.applyDurErr // the effect IS visible (applied) but durability is unconfirmed
			}
			return nil
		},
		ConfirmDurable: func() error {
			s.confirmCalls++
			if s.confirmErr != nil && (s.confirmFailUntil == 0 || s.confirmCalls <= s.confirmFailUntil) {
				return s.confirmErr
			}
			return nil
		},
	}
}

func intent(txnID string) Intent {
	return Intent{Version: IntentVersion, Kind: "gitcommit", TxnID: txnID, ExpectedStateRevision: 1, Payload: json.RawMessage(`{"ref":"refs/x","old":"a","new":"b"}`)}
}

func planFrom(txnID string, steps []*fakeStep) Plan {
	p := Plan{Intent: intent(txnID)}
	for _, s := range steps {
		p.Steps = append(p.Steps, s.toStep())
	}
	return p
}

func makeSteps(names []string) []*fakeStep {
	out := make([]*fakeStep, len(names))
	for i, n := range names {
		out[i] = &fakeStep{name: n}
	}
	return out
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

func TestRunCompletesAllSteps(t *testing.T) {
	j, lock := newJournal(t)
	steps := makeSteps(canonicalSteps)
	withGuard(t, lock, func(g *genstore.Guard) {
		rec, err := j.Run(g, planFrom("t1", steps))
		if err != nil || !rec.Complete || rec.StepsDone != len(canonicalSteps) {
			t.Fatalf("run = %+v err=%v", rec, err)
		}
	})
	for _, s := range steps {
		if !s.applied || s.applyCalls != 1 || s.doubleApply {
			t.Fatalf("step %s applied=%v calls=%d double=%v", s.name, s.applied, s.applyCalls, s.doubleApply)
		}
	}
}

// TestCrashCutMatrix restarts at every step index at three cut points and asserts
// the transaction always completes with every step observably applied and no step
// applied twice.
func TestCrashCutMatrix(t *testing.T) {
	phases := []string{"before-effect", "effect-applied-unrecorded", "after-progress"}
	for i := range canonicalSteps {
		for _, phase := range phases {
			name := fmt.Sprintf("%s_%s", canonicalSteps[i], phase)
			t.Run(name, func(t *testing.T) {
				j, lock := newJournal(t)
				steps := makeSteps(canonicalSteps)

				crashStep := i
				if phase == "after-progress" {
					crashStep = i + 1
				}
				if crashStep < len(steps) {
					steps[crashStep].applyErr = errors.New("crash")
				}
				withGuard(t, lock, func(g *genstore.Guard) { _, _ = j.Run(g, planFrom("t1", steps)) })

				if phase == "effect-applied-unrecorded" {
					steps[i].applied = true // effect landed; only the progress record was lost
				}
				if crashStep < len(steps) {
					steps[crashStep].applyErr = nil // the transient failure clears
				}

				withGuard(t, lock, func(g *genstore.Guard) {
					_, _, err := j.Recover(g, func(Intent) (Plan, error) { return planFrom("t1", steps), nil })
					if err != nil {
						t.Fatalf("recover: %v", err)
					}
				})
				cur, _, _ := j.Latest()
				if !cur.Complete {
					t.Fatalf("transaction did not complete: %+v", cur)
				}
				for _, s := range steps {
					if !s.applied {
						t.Fatalf("step %s not applied after recovery", s.name)
					}
					if s.doubleApply {
						t.Fatalf("step %s effect applied twice", s.name)
					}
				}
			})
		}
	}
}

func TestRecoverIndeterminateFailsClosed(t *testing.T) {
	j, lock := newJournal(t)
	s := []*fakeStep{{name: "a", applyErr: errors.New("stop")}}
	withGuard(t, lock, func(g *genstore.Guard) { _, _ = j.Run(g, planFrom("t1", s)) })
	s[0].applyErr = nil
	s[0].forceState = StatusIndeterminate
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, _, err := j.Recover(g, func(Intent) (Plan, error) { return planFrom("t1", s), nil }); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("err = %v, want ErrRecoveryRequired", err)
		}
	})
	if cur, _, _ := j.Latest(); cur.Terminal() {
		t.Fatalf("indeterminate transaction was terminalized")
	}
}

func TestRecoverPrefixNotAppliedFailsClosed(t *testing.T) {
	j, lock := newJournal(t)
	a := &fakeStep{name: "a"}
	b := &fakeStep{name: "b", applyErr: errors.New("stop")}
	withGuard(t, lock, func(g *genstore.Guard) { _, _ = j.Run(g, planFrom("t1", []*fakeStep{a, b})) }) // steps_done=1
	// The journalled prefix (step a) is now observed NOT applied: fail closed.
	a.applied = false
	b.applyErr = nil
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, _, err := j.Recover(g, func(Intent) (Plan, error) { return planFrom("t1", []*fakeStep{a, b}), nil }); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("prefix regression err = %v, want ErrRecoveryRequired", err)
		}
	})
}

func TestReObserveAfterApplyFailsClosed(t *testing.T) {
	j, lock := newJournal(t)
	s := []*fakeStep{{name: "a", noObserveAfterApply: true}} // Apply returns nil but effect never observable
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, planFrom("t1", s)); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("err = %v, want ErrRecoveryRequired when Apply is not observed", err)
		}
	})
}

func TestAbortObservesStepZero(t *testing.T) {
	// NotApplied step 0 → abort allowed.
	j, lock := newJournal(t)
	s := []*fakeStep{{name: "a", applyErr: errors.New("stop")}}
	withGuard(t, lock, func(g *genstore.Guard) {
		_, _ = j.Run(g, planFrom("t1", s))
		s[0].applyErr = nil // effect is NOT applied (Run failed before setting it)
		out, err := j.Abort(g, planFrom("t1", s))
		if err != nil || !out.Aborted {
			t.Fatalf("abort = %+v err=%v", out, err)
		}
	})

	// Applied step 0 (effect landed) → abort must recover forward, not orphan it.
	j2, lock2 := newJournal(t)
	s2 := []*fakeStep{{name: "a", applyErr: errors.New("stop")}}
	withGuard(t, lock2, func(g *genstore.Guard) {
		_, _ = j2.Run(g, planFrom("t2", s2))
		s2[0].applied = true // effect actually landed
		if _, err := j2.Abort(g, planFrom("t2", s2)); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("abort with applied effect err = %v, want ErrRecoveryRequired", err)
		}
	})
}

func TestPlanMismatchOnRenamedStep(t *testing.T) {
	j, lock := newJournal(t)
	orig := []*fakeStep{{name: "a", applyErr: errors.New("stop")}, {name: "b"}}
	withGuard(t, lock, func(g *genstore.Guard) { _, _ = j.Run(g, planFrom("t1", orig)) })
	renamed := []*fakeStep{{name: "a"}, {name: "RENAMED"}}
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, _, err := j.Recover(g, func(Intent) (Plan, error) { return planFrom("t1", renamed), nil }); !errors.Is(err, ErrPlanMismatch) {
			t.Fatalf("renamed-step recover err = %v, want ErrPlanMismatch", err)
		}
	})
}

func TestZeroStepPlanRejected(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, Plan{Intent: intent("t1")}); err == nil {
			t.Fatalf("a zero-step plan should be rejected")
		}
	})
}

func TestValidateRejectsStuckAllDoneNonTerminal(t *testing.T) {
	r := Record{SchemaVersion: RecordVersion, Revision: 1, Intent: intent("t1"), StepIDs: []string{"a"}, StepsDone: 1}
	if err := validate(r); err == nil {
		t.Fatalf("a non-terminal all-steps-done record should be invalid")
	}
}

func TestNextStepProjection(t *testing.T) {
	j, lock := newJournal(t)
	s := []*fakeStep{{name: "a"}, {name: "b", applyErr: errors.New("stop")}}
	withGuard(t, lock, func(g *genstore.Guard) { _, _ = j.Run(g, planFrom("t1", s)) })
	cur, _, _ := j.Latest()
	if cur.NextStep() != "b" {
		t.Fatalf("next step = %q, want b", cur.NextStep())
	}
}

func TestPrepareWhilePendingRejected(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		_, _ = j.Run(g, planFrom("t1", []*fakeStep{{name: "a", applyErr: errors.New("stop")}}))
		if _, err := j.Run(g, planFrom("t2", []*fakeStep{{name: "a"}})); !errors.Is(err, ErrPending) {
			t.Fatalf("run while pending err = %v, want ErrPending", err)
		}
	})
}

func TestNewTransactionAfterCompletion(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		_, _ = j.Run(g, planFrom("t1", []*fakeStep{{name: "a"}}))
		out, err := j.Run(g, planFrom("t2", []*fakeStep{{name: "a"}}))
		if err != nil || !out.Complete || out.TxnID() != "t2" {
			t.Fatalf("second txn = %+v err=%v", out, err)
		}
	})
}

func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "run.lock")
	jdir := filepath.Join(dir, "txn")
	s := makeSteps(canonicalSteps)
	s[3].applyErr = errors.New("crash")
	withGuard(t, lock, func(g *genstore.Guard) { _, _ = Open(jdir, lock).Run(g, planFrom("t1", s)) })

	s[3].applyErr = nil
	j2 := Open(jdir, lock)
	if cur, ok, _ := j2.Latest(); !ok || cur.Terminal() {
		t.Fatalf("pending transaction did not persist across restart")
	}
	withGuard(t, lock, func(g *genstore.Guard) {
		out, acted, err := j2.Recover(g, func(Intent) (Plan, error) { return planFrom("t1", s), nil })
		if err != nil || !acted || !out.Complete {
			t.Fatalf("restart recover = %+v acted=%v err=%v", out, acted, err)
		}
	})
}

func TestAbortIntentMismatchStaysPending(t *testing.T) {
	j, lock := newJournal(t)
	s := []*fakeStep{{name: "a", applyErr: errors.New("stop")}}
	withGuard(t, lock, func(g *genstore.Guard) { _, _ = j.Run(g, planFrom("t1", s)) })

	// Same txn id and step names, but a different payload.
	badIntent := intent("t1")
	badIntent.Payload = json.RawMessage(`{"ref":"refs/DIFFERENT"}`)
	badPlan := Plan{Intent: badIntent, Steps: []Step{(&fakeStep{name: "a"}).toStep()}}
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Abort(g, badPlan); !errors.Is(err, ErrPlanMismatch) {
			t.Fatalf("abort with changed payload err = %v, want ErrPlanMismatch", err)
		}
	})
	if cur, _, _ := j.Latest(); cur.Terminal() {
		t.Fatalf("the transaction was terminalized by a mismatched abort")
	}
}

func TestGuardValidatedBeforeAnyEffect(t *testing.T) {
	poison := func() Step {
		return Step{
			Name:           "p",
			Status:         func() (StepStatus, error) { t.Fatal("Status called under an invalid guard"); return "", nil },
			Apply:          func() error { t.Fatal("Apply called under an invalid guard"); return nil },
			ConfirmDurable: func() error { t.Fatal("ConfirmDurable called under an invalid guard"); return nil },
		}
	}

	// nil guard.
	j, _ := newJournal(t)
	if _, err := j.Run(nil, Plan{Intent: intent("t1"), Steps: []Step{poison()}}); err == nil {
		t.Fatalf("Run with a nil guard should error")
	}

	// released guard.
	j2, lock2 := newJournal(t)
	g, ok, _ := genstore.Acquire(lock2)
	if !ok {
		t.Fatalf("acquire")
	}
	g.Release()
	if _, err := j2.Run(g, Plan{Intent: intent("t1"), Steps: []Step{poison()}}); err == nil {
		t.Fatalf("Run with a released guard should error")
	}

	// wrong-lock guard on Recover (which reaches step callbacks).
	j3, lock3 := newJournal(t)
	s := []*fakeStep{{name: "a", applyErr: errors.New("stop")}}
	withGuard(t, lock3, func(gg *genstore.Guard) { _, _ = j3.Run(gg, planFrom("t1", s)) })
	other, ok, _ := genstore.Acquire(filepath.Join(t.TempDir(), "other.lock"))
	if !ok {
		t.Fatalf("acquire other")
	}
	defer other.Release()
	if _, _, err := j3.Recover(other, func(Intent) (Plan, error) {
		return Plan{Intent: intent("t1"), Steps: []Step{poison()}}, nil
	}); err == nil {
		t.Fatalf("Recover with a wrong-lock guard should error")
	}
}

// --- M1 durability crash-cut witnesses (seam-driven, platform-independent) ---

// failCountingSyncDir returns a directory-sync seam that succeeds for the first
// (failFrom-1) calls and fails from the failFrom-th call onward.
func failCountingSyncDir(failFrom int) func(string) error {
	calls := 0
	return func(string) error {
		calls++
		if calls >= failFrom {
			return errors.New("injected dir sync failure")
		}
		return nil
	}
}

// 1. A prepare-journal durability failure halts before any step Apply.
func TestDurabilityPrepareJournalHaltsBeforeApply(t *testing.T) {
	j, lock := newJournal(t)
	j.gs.WithSyncDir(failCountingSyncDir(1)) // every journal dir-sync fails
	step := &fakeStep{name: "a"}
	withGuard(t, lock, func(g *genstore.Guard) {
		_, err := j.Run(g, planFrom("t1", []*fakeStep{step}))
		if !errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Run err = %v, want ErrDurabilityUnconfirmed", err)
		}
	})
	if step.applyCalls != 0 {
		t.Fatalf("step applied %d times, want 0 (halt before any effect)", step.applyCalls)
	}
}

// 2. A participant durability failure halts with no progress append, effect not repeated.
func TestDurabilityParticipantConfirmHaltsNoProgress(t *testing.T) {
	j, lock := newJournal(t)
	step := &fakeStep{name: "a", confirmErr: errors.New("participant dir sync")} // persistent
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, planFrom("t1", []*fakeStep{step})); !errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Run err = %v, want ErrDurabilityUnconfirmed", err)
		}
	})
	if step.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1 (effect applied once, not repeated)", step.applyCalls)
	}
	rec, ok, err := j.Latest()
	if err != nil || !ok || rec.StepsDone != 0 || rec.Terminal() {
		t.Fatalf("head = %+v ok=%v err=%v, want the non-terminal prepare record (StepsDone=0)", rec, ok, err)
	}
}

// 3 + 6. After a participant confirm halt, Recover re-confirms the participant directory
// and completes WITHOUT reapplying the effect (safe resume, apply count still one).
func TestDurabilityRetrySucceedsOnRecoverWithoutReapply(t *testing.T) {
	j, lock := newJournal(t)
	step := &fakeStep{name: "a", confirmErr: errors.New("transient"), confirmFailUntil: 1} // confirm fails once
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, planFrom("t1", []*fakeStep{step})); !errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Run err = %v, want ErrDurabilityUnconfirmed", err)
		}
	})
	if step.applyCalls != 1 {
		t.Fatalf("apply calls after halt = %d, want 1", step.applyCalls)
	}
	withGuard(t, lock, func(g *genstore.Guard) {
		out, recovered, err := j.Recover(g, func(Intent) (Plan, error) { return planFrom("t1", []*fakeStep{step}), nil })
		if err != nil || !recovered || !out.Complete {
			t.Fatalf("recover out=%+v recovered=%v err=%v, want complete", out, recovered, err)
		}
	})
	if step.applyCalls != 1 {
		t.Fatalf("apply calls after recover = %d, want 1 (no reapply)", step.applyCalls)
	}
	if step.confirmCalls < 2 {
		t.Fatalf("confirm calls = %d, want >= 2 (recovery re-confirmed the participant)", step.confirmCalls)
	}
}

// 4. A terminal/progress journal durability failure is not clean success: Run reports the
// typed durability error even though the effect applied and the record is visible.
func TestDurabilityTerminalJournalConfirmNoCleanSuccess(t *testing.T) {
	j, lock := newJournal(t)
	// The prepare confirm (calls 1-2) succeeds; the terminal progress confirm (calls 3+) fails.
	j.gs.WithSyncDir(failCountingSyncDir(2)) // ConfirmDurable now syncs s.dir once per confirm: prepare = call 1, terminal = call 2+
	step := &fakeStep{name: "a"}
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, planFrom("t1", []*fakeStep{step})); !errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Run err = %v, want ErrDurabilityUnconfirmed (terminal record not confirmed durable)", err)
		}
	})
	if step.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", step.applyCalls)
	}
}

// realWriteThenSyncError writes the record for real (so it is VISIBLE), then returns a
// real *atomicfile.PostCommitSyncError — the genuine post-rename durability-unconfirmed
// path, so reconcile emits a real *genstore.PostCommitSyncError.
func realWriteThenSyncError() func(string, []byte, os.FileMode) error {
	return func(path string, data []byte, perm os.FileMode) error {
		if werr := atomicfile.Write(path, data, perm); werr != nil {
			return werr
		}
		return &atomicfile.PostCommitSyncError{Path: path, Err: errors.New("dir sync")}
	}
}

// Real *genstore.PostCommitSyncError on the PREPARE append: the record is visible (not
// discarded by append) and the mandatory confirmJournal, unable to confirm, halts before
// any step Apply.
func TestDurabilityRealPreparePostCommitSyncErrorHalts(t *testing.T) {
	j, lock := newJournal(t)
	j.gs.WithWrite(realWriteThenSyncError()).WithSyncDir(failCountingSyncDir(1))
	step := &fakeStep{name: "a"}
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, planFrom("t1", []*fakeStep{step})); !errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Run err = %v, want ErrDurabilityUnconfirmed", err)
		}
	})
	if step.applyCalls != 0 {
		t.Fatalf("apply calls = %d, want 0 (halt at the prepare confirm)", step.applyCalls)
	}
	if rec, ok, err := j.Latest(); err != nil || !ok || rec.StepsDone != 0 {
		t.Fatalf("prepare record head = %+v ok=%v err=%v, want visible StepsDone=0 (not discarded)", rec, ok, err)
	}
}

// The same real prepare durability error, but confirmJournal CAN confirm (real syncDir):
// the record is not discarded and Run completes.
func TestDurabilityRealPrepareCompletesWhenConfirmSucceeds(t *testing.T) {
	j, lock := newJournal(t)
	j.gs.WithWrite(realWriteThenSyncError()) // syncDir seam stays real → ConfirmDurable succeeds
	step := &fakeStep{name: "a"}
	withGuard(t, lock, func(g *genstore.Guard) {
		out, err := j.Run(g, planFrom("t1", []*fakeStep{step}))
		if err != nil || !out.Complete {
			t.Fatalf("Run out=%+v err=%v, want complete (visible record not discarded)", out, err)
		}
	})
}

// Real *genstore.PostCommitSyncError on the PROGRESS/terminal append whose confirm cannot
// succeed: not clean success.
func TestDurabilityRealProgressPostCommitSyncErrorHalts(t *testing.T) {
	j, lock := newJournal(t)
	writes := 0
	j.gs.WithWrite(func(path string, data []byte, perm os.FileMode) error {
		writes++
		if werr := atomicfile.Write(path, data, perm); werr != nil {
			return werr
		}
		if writes >= 2 { // the progress/terminal append (the prepare is the first write)
			return &atomicfile.PostCommitSyncError{Path: path, Err: errors.New("dir sync")}
		}
		return nil
	}).WithSyncDir(failCountingSyncDir(2)) // prepare confirm = syncDir call 1 (ok); terminal confirm = call 2+ (fails)
	step := &fakeStep{name: "a"}
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, planFrom("t1", []*fakeStep{step})); !errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Run err = %v, want ErrDurabilityUnconfirmed (terminal record not confirmed)", err)
		}
	})
	if step.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", step.applyCalls)
	}
}

// A participant Apply that returns a real *genstore.PostCommitSyncError (via a store's
// decode-and-pair) halts wrapping ErrDurabilityUnconfirmed WHILE preserving errors.As to
// the genstore type; Recover then completes with the effect applied exactly once.
func TestDurabilityParticipantApplyRealGenstoreErrorWraps(t *testing.T) {
	j, lock := newJournal(t)
	step := &fakeStep{name: "a", applyDurErr: &genstore.PostCommitSyncError{Generation: 1, Err: errors.New("participant sync")}}
	withGuard(t, lock, func(g *genstore.Guard) {
		_, err := j.Run(g, planFrom("t1", []*fakeStep{step}))
		if !errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Run err = %v, want errors.Is ErrDurabilityUnconfirmed", err)
		}
		var pse *genstore.PostCommitSyncError
		if !errors.As(err, &pse) {
			t.Fatalf("Run err = %v, want errors.As *genstore.PostCommitSyncError preserved", err)
		}
	})
	if step.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", step.applyCalls)
	}
	withGuard(t, lock, func(g *genstore.Guard) {
		out, recovered, err := j.Recover(g, func(Intent) (Plan, error) { return planFrom("t1", []*fakeStep{step}), nil })
		if err != nil || !recovered || !out.Complete {
			t.Fatalf("recover out=%+v recovered=%v err=%v, want complete", out, recovered, err)
		}
	})
	if step.applyCalls != 1 {
		t.Fatalf("apply calls after recover = %d, want 1 (no reapply)", step.applyCalls)
	}
}

// A participant Apply that returns a RAW *atomicfile.PostCommitSyncError (a rooted
// snapshot dir/file publish) halts wrapping ErrDurabilityUnconfirmed while preserving
// errors.As to the atomicfile type; Recover completes with the effect applied exactly once.
func TestDurabilityParticipantApplyRawAtomicfileErrorWraps(t *testing.T) {
	j, lock := newJournal(t)
	step := &fakeStep{name: "a", applyDurErr: &atomicfile.PostCommitSyncError{Path: "x", Err: errors.New("rooted dir publish")}}
	withGuard(t, lock, func(g *genstore.Guard) {
		_, err := j.Run(g, planFrom("t1", []*fakeStep{step}))
		if !errors.Is(err, ErrDurabilityUnconfirmed) {
			t.Fatalf("Run err = %v, want errors.Is ErrDurabilityUnconfirmed", err)
		}
		var ae *atomicfile.PostCommitSyncError
		if !errors.As(err, &ae) {
			t.Fatalf("Run err = %v, want errors.As *atomicfile.PostCommitSyncError preserved", err)
		}
	})
	if step.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1", step.applyCalls)
	}
	withGuard(t, lock, func(g *genstore.Guard) {
		out, recovered, err := j.Recover(g, func(Intent) (Plan, error) { return planFrom("t1", []*fakeStep{step}), nil })
		if err != nil || !recovered || !out.Complete {
			t.Fatalf("recover out=%+v recovered=%v err=%v, want complete", out, recovered, err)
		}
	})
	if step.applyCalls != 1 {
		t.Fatalf("apply calls after recover = %d, want 1 (no reapply)", step.applyCalls)
	}
}

// The next-transaction preflight prune keeps the journal bounded across many transactions
// without breaking recovery: every transaction completes and the last terminal recovers.
func TestJournalPreflightPruneManyTransactions(t *testing.T) {
	j, lock := newJournal(t)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("t%02d", i)
		withGuard(t, lock, func(g *genstore.Guard) {
			out, err := j.Run(g, planFrom(id, []*fakeStep{{name: "a"}}))
			if err != nil || !out.Complete {
				t.Fatalf("run %s: out=%+v err=%v", id, out, err)
			}
		})
	}
	rec, ok, err := j.Latest()
	if err != nil || !ok || !rec.Complete || rec.TxnID() != "t19" {
		t.Fatalf("Latest = %+v ok=%v err=%v, want terminal of t19", rec, ok, err)
	}
}

func TestNilCallbackRejected(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, Plan{Intent: intent("t1"), Steps: []Step{{Name: "a"}}}); err == nil {
			t.Fatalf("nil Status/Apply should be rejected")
		}
	})
	if _, ok, _ := j.Latest(); ok {
		t.Fatalf("a journal record was written for a nil-callback plan")
	}

	j2, lock2 := newJournal(t)
	s := []*fakeStep{{name: "a", applyErr: errors.New("stop")}}
	withGuard(t, lock2, func(g *genstore.Guard) { _, _ = j2.Run(g, planFrom("t1", s)) })
	withGuard(t, lock2, func(g *genstore.Guard) {
		if _, _, err := j2.Recover(g, nil); err == nil {
			t.Fatalf("a nil planFor should be rejected")
		}
	})
}

func TestSecretInPayloadRejected(t *testing.T) {
	j, lock := newJournal(t)
	in := intent("t1")
	in.Payload = json.RawMessage(`{"cmd":"deploy token=sk-ant-abcdefghijklmnopqrstuvwx"}`)
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.Run(g, Plan{Intent: in, Steps: []Step{(&fakeStep{name: "a"}).toStep()}}); err == nil {
			t.Fatalf("a secret in the payload should be rejected")
		}
	})
}
