package state

import (
	"strings"
	"testing"
)

// The attempt ledger is the run's only record of what the mechanical gate actually did. These tests
// are about one property: the record cannot be made to say something that is not true — not by a
// malformed entry, not by a rewrite, and not by reusing evidence.

// sha256Hex builds a distinct, well-formed digest per seed. It must be LOWER HEX: a helper that
// produced 64 arbitrary characters made every case fail on the digest check instead of the rule it
// named, which is the same "rejected for the wrong reason" trap the policy rejection table had.
func sha256Hex(seed byte) string {
	const hex = "0123456789abcdef"
	return strings.Repeat(string(hex[seed%16]), 63) + string(hex[(seed+1)%16])
}

func activeAttempt(rev uint64) *TestAttemptRef {
	return &TestAttemptRef{
		AttemptID:     "attempt-0001",
		StartRevision: rev,
		TestedCommit:  hex40(),
		TestedTree:    hex40(),
		IntentDigest:  sha256Hex(0),
	}
}

func finalized(id string, start, bound uint64, digest string) FinalizedAttempt {
	return FinalizedAttempt{
		AttemptID:      id,
		StartRevision:  start,
		BoundRevision:  bound,
		TestedCommit:   hex40(),
		TestedTree:     hex40(),
		ResultDigest:   digest,
		Execution:      TestExecutionPassed,
		Identity:       TestIdentityUnchanged,
		TerminalReason: "exited 0",
	}
}

func TestActiveAttemptAccepted(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	if _, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
		n.ActiveTestAttempt = activeAttempt(rev)
		return nil
	}); err != nil {
		t.Fatalf("a well-formed active attempt was rejected: %v", err)
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
		{"no start revision", func(_ uint64, a *TestAttemptRef) { a.StartRevision = 0 }, "out of range"},
		// Without a tested identity the outcome is a claim about "the code" with no way to say which.
		{"no tested commit", func(_ uint64, a *TestAttemptRef) { a.TestedCommit = "" }, "tested_commit"},
		{"no tested tree", func(_ uint64, a *TestAttemptRef) { a.TestedTree = "nope" }, "tested_tree"},
		{"no intent digest", func(_ uint64, a *TestAttemptRef) { a.IntentDigest = "short" }, "intent_digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			init := mustInit(t, s)
			_, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
				a := activeAttempt(rev)
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

// TestLedgerIsAppendOnlyByValue is the invariant a length check would not give.
//
// Rewriting an entry in place keeps the count identical, and it is exactly how an indeterminate
// attempt could be quietly reclassified as a clean pass after the fact — erasing the distinction
// between a broken environment and working code that the ledger exists to preserve.
func TestLedgerIsAppendOnlyByValue(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	one, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
		n.TestAttempts = []FinalizedAttempt{finalized("attempt-0001", rev, rev, sha256Hex(0))}
		return nil
	})
	if err != nil {
		t.Fatalf("append the first entry: %v", err)
	}

	t.Run("rewritten in place", func(t *testing.T) {
		_, err := s.Mutate(one.Revision, func(_ uint64, n *RunState) error {
			n.TestAttempts[0].Execution = TestExecutionIndeterminate
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
	t.Run("appended without binding to the resulting revision", func(t *testing.T) {
		_, err := s.Mutate(one.Revision, func(_ uint64, n *RunState) error {
			// A revision that is real and in range, but not the one this mutation produces — otherwise
			// the entry would be refused by the ordering rule and this case would prove nothing about
			// the binding rule it names.
			e := finalized("attempt-0002", 1, 1, sha256Hex(1))
			n.TestAttempts = append(n.TestAttempts, e)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "must bind to revision") {
			t.Fatalf("err = %v, want a binding refusal", err)
		}
	})
	t.Run("a further append is accepted", func(t *testing.T) {
		if _, err := s.Mutate(one.Revision, func(rev uint64, n *RunState) error {
			n.TestAttempts = append(n.TestAttempts, finalized("attempt-0002", rev, rev, sha256Hex(1)))
			return nil
		}); err != nil {
			t.Fatalf("a legitimate append was rejected: %v", err)
		}
	})
}

// TestOneResultDigestCannotAuthorizeTwoOutcomes. The digest IS the evidence for an outcome, so the same
// evidence appearing twice would mean one run of the tests certifying two different answers.
func TestOneResultDigestCannotAuthorizeTwoOutcomes(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	_, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
		dup := sha256Hex(2)
		a := finalized("attempt-0001", rev, rev, dup)
		b := finalized("attempt-0002", rev, rev, dup)
		b.Execution = TestExecutionFailed
		n.TestAttempts = []FinalizedAttempt{a, b}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "reuses a result digest") {
		t.Fatalf("err = %v, want a digest-reuse refusal", err)
	}
}

func TestAttemptIDCannotBeReused(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	_, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
		n.TestAttempts = []FinalizedAttempt{
			finalized("attempt-0001", rev, rev, sha256Hex(3)),
			finalized("attempt-0001", rev, rev, sha256Hex(4)),
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "repeats attempt id") {
		t.Fatalf("err = %v, want an id-reuse refusal", err)
	}
}

// TestAnAttemptCannotBeActiveAndFinalized. The ref is MOVED into the ledger, not copied; both at once
// would mean a finished attempt is still owed a finalization, and whoever finalized it next would be
// binding a second outcome to the same identity.
func TestAnAttemptCannotBeActiveAndFinalized(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	_, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
		n.ActiveTestAttempt = activeAttempt(rev)
		n.TestAttempts = []FinalizedAttempt{finalized(n.ActiveTestAttempt.AttemptID, rev, rev, sha256Hex(5))}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "still the active one") {
		t.Fatalf("err = %v, want a double-binding refusal", err)
	}
}

// TestLedgerIsBoundedByThePolicyCeiling. Indeterminate retries deliberately do not spend the fix
// budget, so without this ceiling they would grow a full-snapshot state without limit.
func TestLedgerIsBoundedByThePolicyCeiling(t *testing.T) {
	s := newStore(t)
	init := mustInit(t, s)
	_, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
		n.EffectivePolicy.Limits.MaxTestAttempts = 2
		for i := 0; i < 3; i++ {
			n.TestAttempts = append(n.TestAttempts, finalized(
				"attempt-000"+string(rune('1'+i)), rev, rev, sha256Hex(byte(20+i))))
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "max_test_attempts") {
		t.Fatalf("err = %v, want a ledger-ceiling refusal", err)
	}
}

// TestFinalizedOutcomeVocabularyIsClosed. Execution and identity are INDEPENDENT facts, and the outcome
// is a total function of the pair; an unknown value on either axis would leave a case with no defined
// answer rather than a wrong one.
func TestFinalizedOutcomeVocabularyIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutet func(*FinalizedAttempt)
		want  string
	}{
		{"unknown execution", func(e *FinalizedAttempt) { e.Execution = "probably-fine" }, "not a known outcome"},
		{"absent execution", func(e *FinalizedAttempt) { e.Execution = "" }, "not a known outcome"},
		{"unknown identity", func(e *FinalizedAttempt) { e.Identity = "maybe" }, "not a known observation"},
		{"absent identity", func(e *FinalizedAttempt) { e.Identity = "" }, "not a known observation"},
		{"no terminal reason", func(e *FinalizedAttempt) { e.TerminalReason = "  " }, "terminal_reason"},
		{"bound before it started", func(e *FinalizedAttempt) { e.StartRevision = e.BoundRevision; e.BoundRevision = 1 }, "before it started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			init := mustInit(t, s)
			_, err := s.Mutate(init.Revision, func(rev uint64, n *RunState) error {
				e := finalized("attempt-0001", rev, rev, sha256Hex(8))
				tc.mutet(&e)
				n.TestAttempts = []FinalizedAttempt{e}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
