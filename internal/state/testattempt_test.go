package state

import (
	"sort"

	"github.com/David-c0degeek/claudex/internal/config"
	"strings"
	"testing"
)

// The attempt ledger is the run's only record of what the mechanical gate actually did. These tests are
// about one property: the record cannot be made to say something that did not happen.
//
// An earlier version of this file created an active attempt in a pristine INIT state and appended
// finalized entries with no attempt behind them, and called both legitimate. They were not — they were
// the fabrications the transition rules exist to refuse, written as fixtures. Everything here now goes
// through the same path a real run takes: reach ownerless TESTS, start an attempt, finalize that exact
// attempt.

// sha256Hex builds a distinct, well-formed digest per seed. It must be LOWER HEX: a helper that
// produced 64 arbitrary characters made every case fail on the digest check instead of the rule it
// named.
func sha256Hex(seed byte) string {
	const hex = "0123456789abcdef"
	return strings.Repeat(string(hex[seed%16]), 63) + string(hex[(seed+1)%16])
}

// atTests drives a fresh run to the ownerless TESTS phase, which is where an attempt may exist.
func atTests(t *testing.T, s *Store) RunState {
	t.Helper()
	return toTests(t, s, mustAgreedImplement(t, s))
}

func attemptRef(id string, rev uint64) *TestAttemptRef {
	return &TestAttemptRef{
		AttemptID:     id,
		StartRevision: rev,
		TestedCommit:  hex40(),
		TestedTree:    hex40(),
		IntentDigest:  sha256Hex(0),
	}
}

// start begins an attempt the way the coordinator will: one CAS at ownerless TESTS.
func start(t *testing.T, s *Store, prev RunState, id string) RunState {
	t.Helper()
	rs, err := s.Mutate(prev.Revision, func(rev uint64, n *RunState) error {
		n.ActiveTestAttempt = attemptRef(id, rev)
		return nil
	})
	if err != nil {
		t.Fatalf("start attempt %s: %v", id, err)
	}
	return rs
}

// finalize moves the exact active ref into the ledger, which is the only legal way an entry appears.
func finalize(t *testing.T, s *Store, prev RunState, digest string, tweak func(*FinalizedAttempt)) (RunState, error) {
	t.Helper()
	return s.Mutate(prev.Revision, func(rev uint64, n *RunState) error {
		a := n.ActiveTestAttempt
		e := FinalizedAttempt{
			AttemptID:      a.AttemptID,
			StartRevision:  a.StartRevision,
			BoundRevision:  rev,
			TestedCommit:   a.TestedCommit,
			TestedTree:     a.TestedTree,
			ResultDigest:   digest,
			Execution:      TestExecutionOK,
			Identity:       TestIdentityUnchanged,
			TerminalReason: "exited 0", TerminalAuthor: TerminalByRunner,
		}
		if tweak != nil {
			tweak(&e)
		}
		n.TestAttempts = append(n.TestAttempts, e)
		n.ActiveTestAttempt = nil
		return nil
	})
}

func mustFinalize(t *testing.T, s *Store, prev RunState, digest string) RunState {
	t.Helper()
	rs, err := finalize(t, s, prev, digest, nil)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return rs
}

// runAttempt is one complete legal cycle: start, then finalize.
func runAttempt(t *testing.T, s *Store, prev RunState, id string, digest string) RunState {
	t.Helper()
	return mustFinalize(t, s, start(t, s, prev, id), digest)
}

func TestAnAttemptCanBeStartedAndFinalized(t *testing.T) {
	s := newStore(t)
	tests := atTests(t, s)
	done := runAttempt(t, s, tests, "attempt-0001", sha256Hex(1))
	if done.ActiveTestAttempt != nil {
		t.Fatal("finalization did not consume the active ref")
	}
	if len(done.TestAttempts) != 1 || done.TestAttempts[0].AttemptID != "attempt-0001" {
		t.Fatalf("ledger = %+v, want one entry for the finalized attempt", done.TestAttempts)
	}
}

// TestWhereAnAttemptMayBeActive. The ref belongs to an ownerless TESTS phase, or to a run already
// marked cancelled whose in-flight attempt still has to be bound — the second arm is what lets a cancel
// take effect immediately without discarding the only identity the runner can finalize.
func TestWhereAnAttemptMayBeActive(t *testing.T) {
	t.Run("refused in INIT", func(t *testing.T) {
		s := newStore(t)
		init := mustInit(t, s)
		_, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
			n.ActiveTestAttempt = attemptRef("attempt-0001", rev)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "an active test attempt requires") {
			t.Fatalf("err = %v, want a placement refusal", err)
		}
	})
	t.Run("refused while a turn is assigned", func(t *testing.T) {
		s := newStore(t)
		tests := atTests(t, s)
		_, err := s.Mutate(tests.Revision, func(rev uint64, n *RunState) error {
			n.Assignment = &Ref{ID: "turn-1", IssuedRevision: rev}
			n.Evidence = testBinding("turn-1", rev)
			n.ActiveTestAttempt = attemptRef("attempt-0001", rev)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "an active test attempt requires") {
			t.Fatalf("err = %v, want a placement refusal", err)
		}
	})
	t.Run("permitted for a cancelled run awaiting its terminal binding", func(t *testing.T) {
		s := newStore(t)
		started := start(t, s, atTests(t, s), "attempt-0001")
		if _, err := s.Mutate(started.Revision, func(_ uint64, n *RunState) error {
			n.Lifecycle = LifecycleCancelled
			n.ActiveTestAttempt.CancelPending = true
			return nil
		}); err != nil {
			t.Fatalf("cancelling a run with an in-flight attempt was refused: %v", err)
		}
	})
}

func TestActiveAttemptTransitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mutet runs against a state that already has attempt-0001 active.
		mutet func(uint64, *RunState)
		want  string
	}{
		{"back-dated start", func(rev uint64, n *RunState) {
			n.ActiveTestAttempt = attemptRef("attempt-0002", rev)
			n.ActiveTestAttempt.StartRevision = 1
		}, "was replaced by"},
		{"tested tree swapped mid-flight", func(_ uint64, n *RunState) {
			n.ActiveTestAttempt.TestedTree = strings.Repeat("b", 40)
		}, "immutable while it is active"},
		{"tested commit swapped mid-flight", func(_ uint64, n *RunState) {
			n.ActiveTestAttempt.TestedCommit = strings.Repeat("c", 40)
		}, "immutable while it is active"},
		{"intent digest swapped mid-flight", func(_ uint64, n *RunState) {
			n.ActiveTestAttempt.IntentDigest = sha256Hex(7)
		}, "immutable while it is active"},
		{"start revision rewritten", func(_ uint64, n *RunState) {
			n.ActiveTestAttempt.StartRevision = 1
		}, "immutable while it is active"},
		{"cleared without finalizing", func(_ uint64, n *RunState) {
			n.ActiveTestAttempt = nil
		}, "cleared without being finalized"},
		{"replaced without finalizing", func(rev uint64, n *RunState) {
			n.ActiveTestAttempt = attemptRef("attempt-0002", rev)
		}, "was replaced by"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			started := start(t, s, atTests(t, s), "attempt-0001")
			_, err := s.Mutate(started.Revision, func(rev uint64, n *RunState) error {
				tc.mutet(rev, n)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestCancelIsMonotone. Un-cancelling would let a run that had already told status and wait it was
// cancelled quietly continue.
func TestCancelIsMonotone(t *testing.T) {
	s := newStore(t)
	started := start(t, s, atTests(t, s), "attempt-0001")
	cancelled, err := s.Mutate(started.Revision, func(_ uint64, n *RunState) error {
		n.Lifecycle = LifecycleCancelled
		n.ActiveTestAttempt.CancelPending = true
		return nil
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, err = s.Mutate(cancelled.Revision, func(_ uint64, n *RunState) error {
		n.ActiveTestAttempt.CancelPending = false
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "cannot un-cancel") {
		t.Fatalf("err = %v, want an un-cancel refusal", err)
	}
}

// TestALedgerEntryMustDescribeTheAttemptItFinalizes is the fabrication guard.
//
// Structural validity proves an entry is well FORMED; it does not prove it describes anything that
// happened. Each case below is well formed and still refused.
func TestALedgerEntryMustDescribeTheAttemptItFinalizes(t *testing.T) {
	t.Run("no active attempt at all", func(t *testing.T) {
		s := newStore(t)
		tests := atTests(t, s)
		_, err := s.Mutate(tests.Revision, func(rev uint64, n *RunState) error {
			n.TestAttempts = append(n.TestAttempts, FinalizedAttempt{
				AttemptID: "attempt-0001", StartRevision: rev, BoundRevision: rev,
				TestedCommit: hex40(), TestedTree: hex40(), ResultDigest: sha256Hex(2),
				Execution: TestExecutionOK, Identity: TestIdentityUnchanged, TerminalReason: "exited 0", TerminalAuthor: TerminalByRunner,
			})
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "no active attempt to finalize") {
			t.Fatalf("err = %v, want a fabrication refusal", err)
		}
	})
	for _, tc := range []struct {
		name  string
		tweak func(*FinalizedAttempt)
		want  string
	}{
		{"a different attempt id", func(e *FinalizedAttempt) { e.AttemptID = "attempt-9999" }, "does not match the active attempt"},
		{"a different tested tree", func(e *FinalizedAttempt) { e.TestedTree = strings.Repeat("d", 40) }, "does not match the active attempt"},
		{"a different tested commit", func(e *FinalizedAttempt) { e.TestedCommit = strings.Repeat("e", 40) }, "does not match the active attempt"},
		{"a different start revision", func(e *FinalizedAttempt) { e.StartRevision = 1 }, "does not match the active attempt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			started := start(t, s, atTests(t, s), "attempt-0001")
			_, err := finalize(t, s, started, sha256Hex(3), tc.tweak)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	t.Run("finalized but left active", func(t *testing.T) {
		s := newStore(t)
		started := start(t, s, atTests(t, s), "attempt-0001")
		_, err := s.Mutate(started.Revision, func(rev uint64, n *RunState) error {
			a := *n.ActiveTestAttempt
			n.TestAttempts = append(n.TestAttempts, FinalizedAttempt{
				AttemptID: a.AttemptID, StartRevision: a.StartRevision, BoundRevision: rev,
				TestedCommit: a.TestedCommit, TestedTree: a.TestedTree, ResultDigest: sha256Hex(4),
				Execution: TestExecutionOK, Identity: TestIdentityUnchanged, TerminalReason: "exited 0", TerminalAuthor: TerminalByRunner,
			})
			return nil // ref deliberately left in place
		})
		if err == nil || !strings.Contains(err.Error(), "still the active one") {
			t.Fatalf("err = %v, want a double-binding refusal", err)
		}
	})
	t.Run("two finalizations in one transition", func(t *testing.T) {
		s := newStore(t)
		started := start(t, s, atTests(t, s), "attempt-0001")
		_, err := s.Mutate(started.Revision, func(rev uint64, n *RunState) error {
			a := *n.ActiveTestAttempt
			mk := func(id, digest string) FinalizedAttempt {
				return FinalizedAttempt{
					AttemptID: id, StartRevision: a.StartRevision, BoundRevision: rev,
					TestedCommit: a.TestedCommit, TestedTree: a.TestedTree, ResultDigest: digest,
					Execution: TestExecutionOK, Identity: TestIdentityUnchanged, TerminalReason: "exited 0", TerminalAuthor: TerminalByRunner,
				}
			}
			n.TestAttempts = append(n.TestAttempts, mk(a.AttemptID, sha256Hex(5)), mk("attempt-0002", sha256Hex(6)))
			n.ActiveTestAttempt = nil
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "only the one active attempt") {
			t.Fatalf("err = %v, want a multi-finalization refusal", err)
		}
	})
}

// TestFinalizingAndStartingAreSeparateOperations.
//
// One CAS must not finalize an attempt and start its replacement. Permitting it whenever a ledger entry
// was appended looked reasonable — the old attempt does reach the ledger — but the next attempt would
// then be created without the run ever passing through the state that authorizes starting one:
// ownerless TESTS with NO active attempt. A ledger-only indeterminate finalization satisfies placement
// on its own, so that intermediate authorization would simply never be observed.
func TestFinalizingAndStartingAreSeparateOperations(t *testing.T) {
	s := newStore(t)
	started := start(t, s, atTests(t, s), "attempt-0001")
	_, err := s.Mutate(started.Revision, func(rev uint64, n *RunState) error {
		a := *n.ActiveTestAttempt
		n.TestAttempts = append(n.TestAttempts, FinalizedAttempt{
			AttemptID: a.AttemptID, StartRevision: a.StartRevision, BoundRevision: rev,
			TestedCommit: a.TestedCommit, TestedTree: a.TestedTree, ResultDigest: sha256Hex(14),
			Execution: TestExecutionInterrupted, Identity: TestIdentityUnobserved, TerminalReason: "supervisor gone", TerminalAuthor: TerminalByCoordinator,
		})
		n.ActiveTestAttempt = attemptRef("attempt-0002", rev)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "separate operations") {
		t.Fatalf("err = %v, want a finalize-and-replace refusal", err)
	}
	// Done as two transitions it is fine, which is what makes the refusal a sequencing rule rather than
	// a prohibition on retrying.
	done := mustFinalize(t, s, started, sha256Hex(15))
	if _, err := s.Mutate(done.Revision, func(rev uint64, n *RunState) error {
		n.ActiveTestAttempt = attemptRef("attempt-0002", rev)
		return nil
	}); err != nil {
		t.Fatalf("a separately-started replacement was refused: %v", err)
	}
}

// TestFrozenCollectionsCannotBeMutatedInPlace.
//
// A struct copy duplicates only a slice HEADER, so a mutator writing next.X[i] writes through to the
// previous state — and the immutability checks compare next against prev, so they would see equality
// and accept the rewrite they exist to catch. It was found once on the attempt ledger and was still
// present, unnoticed, on every other frozen collection: the argv the gate runs, the environment it is
// allowed to see, and the environment actually frozen for it.
func TestFrozenCollectionsCannotBeMutatedInPlace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*RunState)
		mutet func(*RunState)
		want  string
	}{
		{
			"the gate's argv",
			func(n *RunState) { n.EffectivePolicy.TestGate = config.TestGate{Argv: []string{"go", "test"}} },
			func(n *RunState) { n.EffectivePolicy.TestGate.Argv[1] = "rm" },
			"effective_policy is immutable",
		},
		{
			// Reordered, for the same reason as the set case below: substituting a DIFFERENT name would
			// de-authorize an already-resolved variable and be refused by the authorization check
			// first, leaving the aliasing untested.
			"the inherited environment allowlist",
			func(n *RunState) {
				n.EffectivePolicy.TestGate = config.TestGate{Argv: []string{"go", "test"},
					Env: config.TestGateEnv{Inherit: []string{"ALPHA", "BETA"}}}
			},
			func(n *RunState) {
				n.EffectivePolicy.TestGate.Env.Inherit[0], n.EffectivePolicy.TestGate.Env.Inherit[1] =
					n.EffectivePolicy.TestGate.Env.Inherit[1], n.EffectivePolicy.TestGate.Env.Inherit[0]
			},
			"effective_policy is immutable",
		},
		{
			// Reordered rather than revalued: the derivation binding would refuse a changed VALUE
			// before immutability was reached, so the case would then prove the wrong guard. Order is
			// not semantic to derivation, which leaves the aliasing as the only thing under test.
			"the explicitly set environment",
			func(n *RunState) {
				n.EffectivePolicy.TestGate = config.TestGate{Argv: []string{"go", "test"},
					Env: config.TestGateEnv{Set: []config.EnvAssignment{{Name: "CI", Value: "1"}, {Name: "GOFLAGS", Value: "-count=1"}}}}
			},
			func(n *RunState) {
				n.EffectivePolicy.TestGate.Env.Set[0], n.EffectivePolicy.TestGate.Env.Set[1] =
					n.EffectivePolicy.TestGate.Env.Set[1], n.EffectivePolicy.TestGate.Env.Set[0]
			},
			"effective_policy is immutable",
		},
		{
			// An INHERITED name, whose value the policy does not pin — so the derivation check has
			// nothing to say and immutability is what must catch the rewrite. This is the one that
			// matters most: it is the environment the command actually runs with.
			"the frozen resolved environment",
			func(n *RunState) {
				n.EffectivePolicy.TestGate = config.TestGate{Argv: []string{"go", "test"},
					Env: config.TestGateEnv{Inherit: []string{"CI"}}}
			},
			func(n *RunState) { n.ResolvedExecution.Env[0].Value = []byte("/attacker/bin") },
			"resolved_execution is immutable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			init, err := s.Mutate(0, func(_ uint64, n *RunState) error {
				initState(n)
				tc.setup(n)
				// Resolved against a host that HAS the inherited names, so the frozen environment is
				// non-empty and there is something to try to rewrite.
				re, rerr := config.ResolveForRun(n.EffectivePolicy.TestGate, config.HostGOOS(),
					func(name string) (string, bool) { return "ambient-" + name, true }, RunDirRelFor(n.RunID))
				if rerr != nil {
					return rerr
				}
				n.ResolvedExecution = re
				return nil
			})
			if err != nil {
				t.Fatalf("init: %v", err)
			}
			_, err = s.Mutate(init.Revision, func(_ uint64, n *RunState) error {
				tc.mutet(n)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestLedgerIsAppendOnlyByValue is the invariant a length check would not give.
func TestLedgerIsAppendOnlyByValue(t *testing.T) {
	s := newStore(t)
	one := runAttempt(t, s, atTests(t, s), "attempt-0001", sha256Hex(1))

	t.Run("rewritten in place", func(t *testing.T) {
		_, err := s.Mutate(one.Revision, func(_ uint64, n *RunState) error {
			n.TestAttempts[0].Execution = TestExecutionInterrupted
			n.TestAttempts[0].TerminalAuthor = TerminalByCoordinator
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("err = %v, want an immutability refusal", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		_, err := s.Mutate(one.Revision, func(_ uint64, n *RunState) error {
			n.TestAttempts = nil
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "must not shrink") {
			t.Fatalf("err = %v, want a shrink refusal", err)
		}
	})
	t.Run("a second attempt appends legitimately", func(t *testing.T) {
		two := runAttempt(t, s, one, "attempt-0002", sha256Hex(2))
		if len(two.TestAttempts) != 2 {
			t.Fatalf("ledger = %+v, want two entries", two.TestAttempts)
		}
	})
}

// TestOneResultDigestCannotAuthorizeTwoOutcomes. The digest IS the evidence for an outcome, so the same
// evidence appearing twice would mean one run of the tests certifying two different answers. It is
// reachable only ACROSS transitions now, which is exactly how it would happen in practice.
func TestOneResultDigestCannotAuthorizeTwoOutcomes(t *testing.T) {
	s := newStore(t)
	dup := sha256Hex(8)
	one := runAttempt(t, s, atTests(t, s), "attempt-0001", dup)
	started := start(t, s, one, "attempt-0002")
	_, err := finalize(t, s, started, dup, func(e *FinalizedAttempt) { e.Execution = TestExecutionNonzero })
	if err == nil || !strings.Contains(err.Error(), "reuses a result digest") {
		t.Fatalf("err = %v, want a digest-reuse refusal", err)
	}
}

// TestAttemptIDCannotBeReused. The containment, the intent and the result all bind the attempt id, so a
// second attempt wearing a finalized one's name would make those bindings ambiguous. It is refused at
// START, which is the earliest point it can be — before any command runs under a name already spent.
func TestAttemptIDCannotBeReused(t *testing.T) {
	s := newStore(t)
	one := runAttempt(t, s, atTests(t, s), "attempt-0001", sha256Hex(9))
	_, err := s.Mutate(one.Revision, func(rev uint64, n *RunState) error {
		n.ActiveTestAttempt = attemptRef("attempt-0001", rev)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "still the active one") {
		t.Fatalf("err = %v, want an id-reuse refusal", err)
	}
}

// TestLedgerIsBoundedByThePolicyCeiling. Indeterminate retries deliberately do not spend the fix
// budget, so without this ceiling they would grow a full-snapshot state without limit.
func TestLedgerIsBoundedByThePolicyCeiling(t *testing.T) {
	s := newStore(t)
	// The ceiling is part of the FROZEN policy, so it is set at bootstrap: a test that edited it later
	// would be exercising a mutation the state layer refuses outright.
	init, err := s.Mutate(0, func(_ uint64, n *RunState) error {
		initState(n)
		n.EffectivePolicy.Limits.MaxTestAttempts = 1
		return nil
	})
	if err != nil {
		t.Fatalf("init with a capped ledger: %v", err)
	}
	one := runAttempt(t, s, toTests(t, s, driveToAgreedImplement(t, s, init)), "attempt-0001", sha256Hex(11))
	started := start(t, s, one, "attempt-0002")
	if _, err := finalize(t, s, started, sha256Hex(12), nil); err == nil || !strings.Contains(err.Error(), "max_test_attempts") {
		t.Fatalf("err = %v, want a ledger-ceiling refusal", err)
	}
}

// TestFinalizedOutcomeVocabularyIsClosed. Execution and identity are INDEPENDENT facts and the outcome
// is a total function of the pair; an unknown value on either axis would leave a case with no defined
// answer rather than a wrong one.
func TestFinalizedOutcomeVocabularyIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tweak func(*FinalizedAttempt)
		want  string
	}{
		{"unknown execution", func(e *FinalizedAttempt) { e.Execution = "probably-fine" }, "not a known execution observation"},
		{"absent execution", func(e *FinalizedAttempt) { e.Execution = "" }, "not a known execution observation"},
		{"unknown identity", func(e *FinalizedAttempt) { e.Identity = "maybe" }, "not a known identity observation"},
		{"absent identity", func(e *FinalizedAttempt) { e.Identity = "" }, "not a known identity observation"},
		{"no terminal reason", func(e *FinalizedAttempt) { e.TerminalReason = "  " }, "terminal_reason"},
		{"malformed result digest", func(e *FinalizedAttempt) { e.ResultDigest = "not-a-digest" }, "result_digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			started := start(t, s, atTests(t, s), "attempt-0001")
			_, err := finalize(t, s, started, sha256Hex(13), tc.tweak)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestActiveAttemptMalformations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutet func(uint64, *TestAttemptRef)
		want  string
	}{
		{"no id", func(_ uint64, a *TestAttemptRef) { a.AttemptID = "" }, "not a valid id"},
		{"start revision in the future", func(rev uint64, a *TestAttemptRef) { a.StartRevision = rev + 5 }, "out of range"},
		// Without a tested identity the outcome is a claim about "the code" with no way to say which.
		{"no tested commit", func(_ uint64, a *TestAttemptRef) { a.TestedCommit = "" }, "tested_commit"},
		{"no tested tree", func(_ uint64, a *TestAttemptRef) { a.TestedTree = "nope" }, "tested_tree"},
		{"no intent digest", func(_ uint64, a *TestAttemptRef) { a.IntentDigest = "short" }, "intent_digest"},
		{"already cancel-pending at the start", func(_ uint64, a *TestAttemptRef) { a.CancelPending = true }, "cannot already be cancel-pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			tests := atTests(t, s)
			_, err := s.Mutate(tests.Revision, func(rev uint64, n *RunState) error {
				a := attemptRef("attempt-0001", rev)
				tc.mutet(rev, a)
				n.ActiveTestAttempt = a
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestTheCancelledObservationIsBoundToARealCancel, in BOTH directions.
//
// The design's race has one winner either way: a cancel that binds first makes the runner's subsequent
// finalization a ledger-only `cancelled`; a result that binds first makes the cancel a no-op against a
// finalized attempt. Neither half was enforced, so both were expressible as lies — an ordinary attempt
// could be recorded as cancelled when no operator ever asked, and a cancel-pending attempt could take an
// ordinary verdict, spending the fix budget for a run the operator had already stopped.
func TestTheCancelledObservationIsBoundToARealCancel(t *testing.T) {
	t.Run("cancelled without a cancel having bound", func(t *testing.T) {
		s := newStore(t)
		started := start(t, s, atTests(t, s), "attempt-0001")
		_, err := finalize(t, s, started, sha256Hex(20), func(e *FinalizedAttempt) {
			e.Execution = TestExecutionCancelled
			e.TerminalReason = "cancelled"
		})
		if err == nil || !strings.Contains(err.Error(), "no cancel had bound") {
			t.Fatalf("err = %v, want a fabricated-cancel refusal", err)
		}
	})
	t.Run("a cancelled attempt taking an ordinary verdict", func(t *testing.T) {
		s := newStore(t)
		started := start(t, s, atTests(t, s), "attempt-0001")
		cancelled, err := s.Mutate(started.Revision, func(_ uint64, n *RunState) error {
			n.Lifecycle = LifecycleCancelled
			n.ActiveTestAttempt.CancelPending = true
			return nil
		})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := finalize(t, s, cancelled, sha256Hex(21), nil); err == nil ||
			!strings.Contains(err.Error(), "can only be finalized as cancelled") {
			t.Fatalf("err = %v, want a refusal to take an ordinary verdict after a cancel", err)
		}
		// And the legitimate finalization of that same attempt IS accepted.
		done, err := finalize(t, s, cancelled, sha256Hex(22), func(e *FinalizedAttempt) {
			e.Execution = TestExecutionCancelled
			e.TerminalReason = "cancelled by operator"
		})
		if err != nil {
			t.Fatalf("a cancelled attempt could not be finalized as cancelled: %v", err)
		}
		got, oerr := Outcome(done.TestAttempts[0].Execution, done.TestAttempts[0].Identity)
		if oerr != nil || got != OutcomeCancelled {
			t.Fatalf("verdict = %q (err %v), want cancelled", got, oerr)
		}
	})
}

// TestTerminalDetailMustArriveCanonical.
//
// The persistence boundary used to REDACT this field. That silently severed the entry's ResultDigest —
// which binds the canonical result published before this CAS — from the text it identifies: the digest
// stayed while the text it covered changed underneath it. So the boundary now refuses a non-canonical
// reason instead of fixing one, and CanonicalTerminalReason is the single constructor applied before
// both hashing and binding.
func TestTerminalDetailMustArriveCanonical(t *testing.T) {
	const secret = "sk-ant-abcdefghijklmnopqrstuvwx"
	raw := "exec failed: token=" + secret

	t.Run("a raw reason is refused, not rewritten", func(t *testing.T) {
		s := newStore(t)
		started := start(t, s, atTests(t, s), "attempt-0001")
		_, err := finalize(t, s, started, sha256Hex(23), func(e *FinalizedAttempt) {
			e.Execution = TestExecutionSpawnFailed
			e.TerminalReason = raw
		})
		if err == nil || !strings.Contains(err.Error(), "not canonical") {
			t.Fatalf("err = %v, want a refusal naming the canonical constructor", err)
		}
	})

	t.Run("the canonical form is accepted and stored unchanged", func(t *testing.T) {
		s := newStore(t)
		started := start(t, s, atTests(t, s), "attempt-0001")
		canonical := CanonicalTerminalReason(raw)
		if strings.Contains(canonical, secret) {
			t.Fatalf("the canonical form still carries the credential: %q", canonical)
		}
		if len(canonical) == len(raw) {
			t.Fatal("redaction did not change the text; the vector proves nothing")
		}
		done, err := finalize(t, s, started, sha256Hex(24), func(e *FinalizedAttempt) {
			e.Execution = TestExecutionSpawnFailed
			e.TerminalReason = canonical
		})
		if err != nil {
			t.Fatalf("a canonical reason was refused: %v", err)
		}
		// Stored EXACTLY as supplied — which is what keeps the result digest, computed over these same
		// bytes before the CAS, an identifier of what is actually in the ledger.
		if got := done.TestAttempts[0].TerminalReason; got != canonical {
			t.Fatalf("the persistence boundary altered a digest-bound field: %q -> %q", canonical, got)
		}
		if got := done.TestAttempts[0].ResultDigest; got != sha256Hex(24) {
			t.Fatalf("the result digest changed: %q", got)
		}
	})

	// Idempotence is what lets the caller apply this before hashing and the boundary check it
	// afterwards without the two disagreeing.
	t.Run("canonicalization is idempotent", func(t *testing.T) {
		once := CanonicalTerminalReason(raw)
		if twice := CanonicalTerminalReason(once); twice != once {
			t.Fatalf("not idempotent: %q -> %q", once, twice)
		}
	})
}

// TestTheCancelFactIsWholeOrAbsent.
//
// The cancel is ONE durable transition: the CAS that cancels marks the lifecycle cancelled and the
// attempt cancel-pending together. As two independent placement arms, either half could stand alone —
// and a fabricated half-state then satisfied the finalization rule, letting a cancel be recorded that
// never happened, or an ordinary verdict be taken on a run the operator had stopped.
func TestTheCancelFactIsWholeOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutet func(*RunState)
	}{
		{"cancel pending on a running run", func(n *RunState) {
			n.ActiveTestAttempt.CancelPending = true
		}},
		{"a cancelled run whose attempt is not cancel-pending", func(n *RunState) {
			n.Lifecycle = LifecycleCancelled
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			started := start(t, s, atTests(t, s), "attempt-0001")
			_, err := s.Mutate(started.Revision, func(_ uint64, n *RunState) error {
				tc.mutet(n)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "an active test attempt requires") {
				t.Fatalf("err = %v, want a half-state refusal", err)
			}
		})
	}
	// Both halves together are accepted, so the rule is a binding rather than a prohibition on cancelling.
	t.Run("both halves together", func(t *testing.T) {
		s := newStore(t)
		started := start(t, s, atTests(t, s), "attempt-0001")
		if _, err := s.Mutate(started.Revision, func(_ uint64, n *RunState) error {
			n.Lifecycle = LifecycleCancelled
			n.ActiveTestAttempt.CancelPending = true
			return nil
		}); err != nil {
			t.Fatalf("the whole cancel fact was refused: %v", err)
		}
	})
}

// TestALiveAttemptRequiresARunningRun sweeps the WHOLE lifecycle vocabulary.
//
// The rule was once "not cancelled", which is a negative test and therefore admitted every other value:
// paused, rate-limited, failed and completed runs could all hold a live attempt. An attempt EXECUTES,
// and only a running run executes — a live attempt under a terminal or paused run is a state the design
// does not describe, and the lifecycle machine would have been built on top of it.
//
// It iterates the production vocabulary rather than a copy, so adding a lifecycle without deciding
// whether an attempt may live under it fails here. Each case is pinned to the error it ACTUALLY
// produces: several lifecycles are refused by an older invariant first, and asserting the placement
// message for those would be asserting something that never runs.
func TestALiveAttemptRequiresARunningRun(t *testing.T) {
	// Sorted, so a failure names the same case on every run.
	var lifecycles []Lifecycle
	for l := range knownLifecycles {
		lifecycles = append(lifecycles, l)
	}
	sort.Slice(lifecycles, func(i, j int) bool { return lifecycles[i] < lifecycles[j] })
	if len(lifecycles) < 8 {
		t.Fatalf("the lifecycle vocabulary has %d values; this sweep expects the full set", len(lifecycles))
	}

	for _, l := range lifecycles {
		t.Run(string(l), func(t *testing.T) {
			s := newStore(t)
			started := start(t, s, atTests(t, s), "attempt-0001")
			_, err := s.Mutate(started.Revision, func(_ uint64, n *RunState) error {
				n.Lifecycle = l
				// The cancelled arm is the ONE exception, and it requires the whole cancel fact.
				if l == LifecycleCancelled {
					n.ActiveTestAttempt.CancelPending = true
				}
				return nil
			})
			switch l {
			case LifecycleRunning:
				if err != nil {
					t.Fatalf("a running run was refused a live attempt: %v", err)
				}
			case LifecycleCancelled:
				if err != nil {
					t.Fatalf("the cancelled exception was refused: %v", err)
				}
			default:
				if err == nil {
					t.Fatalf("a live attempt was accepted under lifecycle %q", l)
				}
				t.Logf("%s refused with: %v", l, err)
			}
		})
	}
}

// TestTheCancelledExceptionStillNeedsTheWholeFact. The sweep above accepts `cancelled`, so this pins
// that it accepts it only WITH the cancel bound — otherwise the exception would be a hole.
func TestTheCancelledExceptionStillNeedsTheWholeFact(t *testing.T) {
	s := newStore(t)
	started := start(t, s, atTests(t, s), "attempt-0001")
	_, err := s.Mutate(started.Revision, func(_ uint64, n *RunState) error {
		n.Lifecycle = LifecycleCancelled // without CancelPending
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "an active test attempt requires") {
		t.Fatalf("err = %v, want a half-state refusal", err)
	}
}

// TestATerminalAccountMustNameItsAuthority.
//
// The field has two possible authorities and reading it without knowing which applies is reading
// somebody's prose as somebody else's observation. Ordinarily the runner reports how the command ended;
// an attempt interrupted by a crash has no runner left to report anything, so recovery authors the
// account deterministically. Only that row may do so - anything else recording recovery prose would be
// presenting an inference as an observation.
func TestATerminalAccountMustNameItsAuthority(t *testing.T) {
	for _, tc := range []struct {
		name      string
		author    TerminalAuthor
		execution TestExecution
		wantErr   string
	}{
		{"the zero value is not an authority", "", TestExecutionOK, "not a known authority"},
		{"an invented authority", "the operator", TestExecutionOK, "not a known authority"},
		{"recovery may author an interrupted account", TerminalByRecovery, TestExecutionInterrupted, ""},
		// A LIVE coordinator observing an infrastructure fault is a third authority. Without it those
		// rows had no truthful value: a dead supervisor cannot author its own obituary and recovery is
		// not running.
		{"the coordinator may author an interrupted account", TerminalByCoordinator, TestExecutionInterrupted, ""},
		// `interrupted` is exactly what a live runner cannot report - had it been alive to report, it
		// would have reported the outcome instead.
		{"the runner may not author an interrupted account", TerminalByRunner, TestExecutionInterrupted, "cannot have observed it"},
		{"recovery may not author an ok account", TerminalByRecovery, TestExecutionOK, "cannot have observed it"},
		{"recovery may not author a timeout account", TerminalByRecovery, TestExecutionTimeout, "cannot have observed it"},
		{"the coordinator may not author a nonzero account", TerminalByCoordinator, TestExecutionNonzero, "cannot have observed it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			active := start(t, st, atTests(t, st), "attempt-0001")
			_, err := finalize(t, st, active, sha256Hex(1), func(e *FinalizedAttempt) {
				e.TerminalAuthor = tc.author
				e.Execution = tc.execution
				if tc.execution != TestExecutionOK {
					e.Identity = TestIdentityUnobserved
				}
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("a legitimate account was refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
