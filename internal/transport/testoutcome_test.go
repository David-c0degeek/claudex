package transport

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

const evDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// runAtTests puts the run at a running, ownerless TESTS phase with the pair generation
// set, and returns the store + revision. extraFixes is added to TestFixes.
func runAtTests(t *testing.T, pairGen uint64) (*state.Store, uint64) {
	t.Helper()
	store, rev := newStoreAt(t, func(gen uint64, n *state.RunState) {
		*n.StepIndex = n.AgreedPlan.Plan.StepCount
		n.Phase = state.PhaseTests
		n.Assignment = nil
	})
	registryPairGen(t, store, pairGen)
	return store, rev
}

func testOutcomeDeps(store *state.Store, prep TestPrepare) TestOutcomeDeps {
	return TestOutcomeDeps{Store: store, Registry: openRunRegistry(store), Journal: terminalJournal(store), Prepare: prep}
}

// prepFn wraps an apply into a TestPrepare that returns the given issued ids.
func prepFn(turn, gate string, apply func(gen uint64, next *state.RunState, p PreparedTestOutcome) error) TestPrepare {
	return func(_ state.RunState, p PreparedTestOutcome) (PreparedTransition, error) {
		return NewPreparedTransition(turn, gate, func(gen uint64, next *state.RunState) error { return apply(gen, next, p) }), nil
	}
}

func passToVerify() TestPrepare {
	return prepFn("", "", func(_ uint64, next *state.RunState, p PreparedTestOutcome) error {
		next.Assignment = nil
		next.Phase = state.PhaseVerify
		next.Verify = &state.VerifyRequirement{RequiredGeneration: p.CurrentPairGeneration + 1}
		return nil
	})
}

func passApply(next *state.RunState, p PreparedTestOutcome) {
	next.Assignment = nil
	next.Phase = state.PhaseVerify
	next.Verify = &state.VerifyRequirement{RequiredGeneration: p.CurrentPairGeneration + 1}
}

func failToFixApply(gen uint64, next *state.RunState, _ PreparedTestOutcome) error {
	next.Phase = state.PhaseFix
	next.FixReturn = state.PhaseTests
	next.Counters.TestFixes++
	next.Assignment = &state.Ref{ID: "t-turn", IssuedRevision: gen}
	return nil
}

func failToFix() TestPrepare {
	return prepFn("fix-turn", "", func(gen uint64, next *state.RunState, _ PreparedTestOutcome) error {
		next.Phase = state.PhaseFix
		next.FixReturn = state.PhaseTests
		next.Counters.TestFixes++
		next.Assignment = &state.Ref{ID: "fix-turn", IssuedRevision: gen}
		return nil
	})
}

// runAtTestsBudget puts the run at TESTS with the test budget already at the frozen
// limit (so a fail gates), returning the store + revision.
func runAtTestsBudget(t *testing.T) (*state.Store, uint64) {
	t.Helper()
	store, rev := newStoreAt(t, func(gen uint64, n *state.RunState) {
		*n.StepIndex = n.AgreedPlan.Plan.StepCount
		n.Phase = state.PhaseTests
		n.Assignment = nil
		n.Counters.TestFixes = n.EffectivePolicy.Budgets.TestRounds
	})
	registryPairGen(t, store, 2)
	return store, rev
}

// gatePrep builds a TESTS quality-gate transition whose pause carries the given source.
func gatePrep(source state.EventRef) TestPrepare {
	return prepFn("", "gate-turn", func(gen uint64, next *state.RunState, _ PreparedTestOutcome) error {
		next.Assignment = nil
		next.Phase = state.PhaseAwaitGuidance
		next.Lifecycle = state.LifecyclePaused
		next.Gate = &state.Ref{ID: "gate-turn", IssuedRevision: gen}
		next.Pause = &state.PauseContext{
			Kind: state.PauseQualityBudget, OriginPhase: state.PhaseTests, ResumePhase: state.PhaseFix,
			FixReturn: state.PhaseTests, Source: source, Budget: &state.BudgetPause{Kind: state.BudgetTest},
		}
		return nil
	})
}

// --- happy outcomes ---

func TestTestOutcomePassToVerify(t *testing.T) {
	store, rev := runAtTests(t, 2)
	before, _, _ := store.Load()
	res, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, passToVerify()), rev, evDigest)
	if err != nil {
		t.Fatalf("pass -> verify: %v", err)
	}
	if res.Revision != rev+1 {
		t.Fatalf("revision = %d, want %d", res.Revision, rev+1)
	}
	rs, _, _ := store.Load()
	if rs.Phase != state.PhaseVerify || rs.Assignment != nil || rs.Verify == nil || rs.Verify.RequiredGeneration != 3 {
		t.Fatalf("not ownerless VERIFY at threshold 3: %+v", rs)
	}
	// The outcome adds no accepted turn (the ledger is unchanged) and no receipt.
	if len(rs.AcceptedTurns) != len(before.AcceptedTurns) {
		t.Fatalf("a test outcome changed the accepted-turns ledger: %d -> %d", len(before.AcceptedTurns), len(rs.AcceptedTurns))
	}
}

func TestTestOutcomeFailToFix(t *testing.T) {
	store, rev := runAtTests(t, 2)
	before, _, _ := store.Load()
	res, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, failToFix()), rev, evDigest)
	if err != nil {
		t.Fatalf("fail -> fix: %v", err)
	}
	rs, _, _ := store.Load()
	if rs.Phase != state.PhaseFix || rs.FixReturn != state.PhaseTests || rs.Assignment == nil || rs.Assignment.ID != "fix-turn" || rs.Counters.TestFixes != 1 {
		t.Fatalf("not at FIX: %+v", rs)
	}
	if res.Revision != rev+1 || len(rs.AcceptedTurns) != len(before.AcceptedTurns) {
		t.Fatalf("result/ledger wrong: rev=%d turns=%d", res.Revision, len(rs.AcceptedTurns))
	}
}

func TestTestOutcomeFailToQualityGate(t *testing.T) {
	store, rev := runAtTestsBudget(t)
	// The gate carries the ACCEPTED empty-turn evidence source.
	if _, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, gatePrep(state.EventRef{Digest: evDigest})), rev, evDigest); err != nil {
		t.Fatalf("fail -> quality gate: %v", err)
	}
	rs, _, _ := store.Load()
	if rs.Lifecycle != state.LifecyclePaused || rs.Pause == nil || rs.Pause.Budget == nil || rs.Pause.Budget.Kind != state.BudgetTest {
		t.Fatalf("not a test-quality gate: %+v", rs)
	}
	if rs.Pause.Source != (state.EventRef{Digest: evDigest}) {
		t.Fatalf("gate source = %+v, want the empty-turn evidence {Digest: %s}", rs.Pause.Source, evDigest)
	}
}

// A gate whose pause carries a different (still-valid) evidence digest than the public
// operation accepted is rejected.
func TestTestOutcomeSubstitutedEvidenceRejected(t *testing.T) {
	store, rev := runAtTestsBudget(t)
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, gatePrep(state.EventRef{Digest: other})), rev, evDigest); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("substituted evidence err = %v, want ErrTransitionInvalid", err)
	}
	if rs, _, _ := store.Load(); rs.Revision != rev {
		t.Fatalf("a rejected outcome advanced the run: %d -> %d", rev, rs.Revision)
	}
}

func TestClassifyTestOutcome(t *testing.T) {
	// Committed, clean release.
	if res, err := classifyTestOutcome(7, nil, nil); err != nil || res.Revision != 7 || res.ReleaseWarning != nil {
		t.Fatalf("committed: res=%+v err=%v", res, err)
	}
	// Committed, release failed -> a post-commit warning over the revision.
	res, err := classifyTestOutcome(7, nil, errors.New("release-x"))
	var pce *genstore.PostCommitError
	if err != nil || res.Revision != 7 || !errors.As(res.ReleaseWarning, &pce) {
		t.Fatalf("committed+release-fail: res=%+v err=%v", res, err)
	}
	// Ambiguous -> zero result, both ErrOutcomeUnknown and ErrAmbiguous, NOT
	// mislabeled proven-uncommitted.
	res, err = classifyTestOutcome(0, fmt.Errorf("%w: x", genstore.ErrAmbiguous), nil)
	if res != (TestOutcomeResult{}) || !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, genstore.ErrAmbiguous) {
		t.Fatalf("ambiguous: res=%+v err=%v", res, err)
	}
	// A proven-uncommitted failure -> zero result, the error, NOT outcome-unknown.
	boom := errors.New("boom")
	res, err = classifyTestOutcome(0, boom, nil)
	if res != (TestOutcomeResult{}) || !errors.Is(err, boom) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("proven-uncommitted: res=%+v err=%v", res, err)
	}
}

// The journal recovery check runs before the registry: an absent Registry (which would
// be a run mismatch) plus a nonterminal journal yields recovery-required.
func TestTestOutcomeJournalBeforeRegistry(t *testing.T) {
	store, rev := newStoreAt(t, func(gen uint64, n *state.RunState) {
		*n.StepIndex = n.AgreedPlan.Plan.StepCount
		n.Phase = state.PhaseTests
		n.Assignment = nil
	})
	d := testOutcomeDeps(store, passToVerify())
	d.Journal = fakeJournal{lockPath: store.LockPath(), head: JournalNonterminal}
	if _, err := SubmitTestOutcome(context.Background(), d, rev, evDigest); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("err = %v, want ErrRecoveryRequired (journal before registry)", err)
	}
}

// --- validation before acquisition ---

func TestTestOutcomeInputValidation(t *testing.T) {
	store, rev := runAtTests(t, 2)
	deps := testOutcomeDeps(store, passToVerify())
	if _, err := SubmitTestOutcome(context.Background(), deps, 0, evDigest); !errors.Is(err, ErrBadEvidence) {
		t.Fatalf("zero revision: %v", err)
	}
	if _, err := SubmitTestOutcome(context.Background(), deps, rev, "not-hex"); !errors.Is(err, ErrBadEvidence) {
		t.Fatalf("bad digest: %v", err)
	}
	if _, err := SubmitTestOutcome(context.Background(), TestOutcomeDeps{}, rev, evDigest); !errors.Is(err, ErrMissingSeam) {
		t.Fatalf("missing seams: %v", err)
	}
	bad := deps
	bad.Journal = fakeJournal{lockPath: "/elsewhere"}
	if _, err := SubmitTestOutcome(context.Background(), bad, rev, evDigest); !errors.Is(err, ErrLockMismatch) {
		t.Fatalf("lock mismatch: %v", err)
	}
}

func TestTestOutcomeNoRunMismatchNoPair(t *testing.T) {
	t.Run("no run state", func(t *testing.T) {
		dir := t.TempDir()
		store := state.Open(filepath.Join(dir, "state"), filepath.Join(dir, "run.lock"))
		if _, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, passToVerify()), 1, evDigest); !errors.Is(err, ErrNoRun) {
			t.Fatalf("err = %v, want ErrNoRun", err)
		}
	})
	t.Run("no registry", func(t *testing.T) {
		// A run at TESTS with no Registry initialized (the run identity cannot bind).
		store, rev := newStoreAt(t, func(gen uint64, n *state.RunState) {
			*n.StepIndex = n.AgreedPlan.Plan.StepCount
			n.Phase = state.PhaseTests
			n.Assignment = nil
		})
		if _, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, passToVerify()), rev, evDigest); !errors.Is(err, ErrRunMismatch) {
			t.Fatalf("err = %v, want ErrRunMismatch", err)
		}
	})
	t.Run("no pair slot", func(t *testing.T) {
		store, rev := newStoreAt(t, func(gen uint64, n *state.RunState) {
			*n.StepIndex = n.AgreedPlan.Plan.StepCount
			n.Phase = state.PhaseTests
			n.Assignment = nil
		})
		// A lead-only registry: the pair generation cannot be derived.
		if _, err := openRunRegistry(store).Mutate(0, func(gen uint64, n *state.Registry) error {
			n.RunID = "run-a"
			n.Lead = &state.RoleSlot{Agent: state.AgentClaude, CurrentSessionID: leadSess, Sessions: []state.SessionRecord{{SessionID: leadSess, Generation: 1, IssuedRegistryRevision: gen}}}
			return nil
		}); err != nil {
			t.Fatalf("lead-only registry: %v", err)
		}
		if _, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, passToVerify()), rev, evDigest); !errors.Is(err, ErrRunMismatch) {
			t.Fatalf("err = %v, want ErrRunMismatch (no pair)", err)
		}
	})
}

func TestTestOutcomeBusyAndContext(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		store, rev := runAtTests(t, 2)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := SubmitTestOutcome(ctx, testOutcomeDeps(store, passToVerify()), rev, evDigest); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("lock held by another holder", func(t *testing.T) {
		store, rev := runAtTests(t, 2)
		g, ok, err := genstore.Acquire(store.LockPath())
		if err != nil || !ok {
			t.Fatalf("hold lock: ok=%v err=%v", ok, err)
		}
		defer g.Release()
		if _, err := SubmitTestOutcome(context.Background(), testOutcomeDeps(store, passToVerify()), rev, evDigest); !errors.Is(err, genstore.ErrBusy) {
			t.Fatalf("err = %v, want ErrBusy", err)
		}
	})
}

// --- guarded rejections (no state change) ---

func TestTestOutcomeGuardedRejections(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (TestOutcomeDeps, uint64)
		want  error
	}{
		{"stale revision", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, passToVerify()), rev + 1
		}, nil}, // *StaleError, checked separately
		{"not TESTS (implement phase)", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := newStoreAt(t, func(gen uint64, n *state.RunState) {}) // stays IMPLEMENT
			registryPairGen(t, store, 2)
			return testOutcomeDeps(store, passToVerify()), rev
		}, ErrNotTestsPhase},
		{"assignment present at TESTS", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := newStoreAt(t, func(gen uint64, n *state.RunState) {
				*n.StepIndex = n.AgreedPlan.Plan.StepCount
				n.Phase = state.PhaseTests
				n.Assignment = &state.Ref{ID: "stray", IssuedRevision: gen}
			})
			registryPairGen(t, store, 2)
			return testOutcomeDeps(store, passToVerify()), rev
		}, ErrNotTestsPhase},
		{"nonterminal journal", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			d := testOutcomeDeps(store, passToVerify())
			d.Journal = fakeJournal{lockPath: store.LockPath(), head: JournalNonterminal}
			return d, rev
		}, ErrRecoveryRequired},
		{"unknown journal", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			d := testOutcomeDeps(store, passToVerify())
			d.Journal = fakeJournal{lockPath: store.LockPath(), err: errors.New("corrupt")}
			return d, rev
		}, ErrRecoveryRequired},
		{"prepare error", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, func(state.RunState, PreparedTestOutcome) (PreparedTransition, error) {
				return PreparedTransition{}, errors.New("prep boom")
			}), rev
		}, nil},
		{"nil apply", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, func(state.RunState, PreparedTestOutcome) (PreparedTransition, error) {
				return PreparedTransition{}, nil
			}), rev
		}, ErrTransitionInvalid},
		{"prepare mutates accepted turns", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("", "", func(_ uint64, next *state.RunState, p PreparedTestOutcome) error {
				next.Phase = state.PhaseVerify
				next.Verify = &state.VerifyRequirement{RequiredGeneration: p.CurrentPairGeneration + 1}
				next.AcceptedTurns["forged"] = state.AcceptedTurn{ArtifactDigest: evDigest}
				return nil
			})), rev
		}, ErrTransitionInvalid},
		{"stays at TESTS (no advance)", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("", "", func(_ uint64, next *state.RunState, _ PreparedTestOutcome) error {
				return nil // no-op: still TESTS
			})), rev
		}, ErrTransitionInvalid},
		{"direct assigned VERIFY entry", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("v-turn", "", func(gen uint64, next *state.RunState, p PreparedTestOutcome) error {
				next.Phase = state.PhaseVerify
				next.Verify = &state.VerifyRequirement{RequiredGeneration: p.CurrentPairGeneration + 1}
				next.Assignment = &state.Ref{ID: "v-turn", IssuedRevision: gen}
				return nil
			})), rev
		}, ErrTransitionInvalid},
		{"wrong verify threshold", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("", "", func(_ uint64, next *state.RunState, p PreparedTestOutcome) error {
				next.Phase = state.PhaseVerify
				next.Verify = &state.VerifyRequirement{RequiredGeneration: p.CurrentPairGeneration + 9}
				return nil
			})), rev
		}, ErrTransitionInvalid},
		{"forged TESTS -> DONE bypass", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("", "", func(_ uint64, next *state.RunState, _ PreparedTestOutcome) error {
				next.Assignment = nil
				next.Phase = state.PhaseDone
				next.Lifecycle = state.LifecycleCompleted
				return nil
			})), rev
		}, ErrTransitionInvalid},
		{"declared id collides with an accepted turn", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("plan-turn", "", func(gen uint64, next *state.RunState, _ PreparedTestOutcome) error {
				next.Phase = state.PhaseFix
				next.FixReturn = state.PhaseTests
				next.Counters.TestFixes++
				next.Assignment = &state.Ref{ID: "plan-turn", IssuedRevision: gen}
				return nil
			})), rev
		}, ErrTransitionInvalid},
		{"both an assignment and a gate declared", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("t-turn", "g-gate", failToFixApply)), rev
		}, ErrTransitionInvalid},
		{"declared id does not match the applied assignment", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("declared", "", func(gen uint64, next *state.RunState, _ PreparedTestOutcome) error {
				next.Phase = state.PhaseFix
				next.FixReturn = state.PhaseTests
				next.Counters.TestFixes++
				next.Assignment = &state.Ref{ID: "applied-other", IssuedRevision: gen}
				return nil
			})), rev
		}, ErrTransitionInvalid},
		{"accepted turns nil'd", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("", "", func(_ uint64, next *state.RunState, p PreparedTestOutcome) error {
				passApply(next, p)
				next.AcceptedTurns = nil
				return nil
			})), rev
		}, ErrTransitionInvalid},
		{"accepted turn deleted", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("", "", func(_ uint64, next *state.RunState, p PreparedTestOutcome) error {
				passApply(next, p)
				delete(next.AcceptedTurns, "plan-turn")
				return nil
			})), rev
		}, ErrTransitionInvalid},
		{"accepted turn altered", func(t *testing.T) (TestOutcomeDeps, uint64) {
			store, rev := runAtTests(t, 2)
			return testOutcomeDeps(store, prepFn("", "", func(_ uint64, next *state.RunState, p PreparedTestOutcome) error {
				passApply(next, p)
				at := next.AcceptedTurns["plan-turn"]
				at.Phase = state.PhaseFix
				next.AcceptedTurns["plan-turn"] = at
				return nil
			})), rev
		}, ErrTransitionInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps, rev := c.setup(t)
			before, _, _ := deps.Store.Load()
			_, err := SubmitTestOutcome(context.Background(), deps, rev, evDigest)
			if c.name == "stale revision" {
				var se *StaleError
				if !errors.As(err, &se) {
					t.Fatalf("err = %v, want StaleError", err)
				}
			} else if c.want == nil {
				if err == nil {
					t.Fatalf("expected an error")
				}
			} else if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			// No transition on a rejected outcome.
			after, _, _ := deps.Store.Load()
			if after.Revision != before.Revision {
				t.Fatalf("a rejected outcome advanced the run: %d -> %d", before.Revision, after.Revision)
			}
		})
	}
}
