package state

import "testing"

// The verdict is a TOTAL function of two independent observations, and this file is where that is
// proved rather than asserted.
//
// The predecessor was a precedence list. A precedence list has to be read in order to be understood,
// and every combination nobody thought about falls through to whatever the last branch happens to be —
// so the dangerous cases are exactly the ones no one wrote down. The whole cross product is enumerated
// here, and a separate test proves the enumeration IS the whole cross product, so a value added to
// either axis without a decision cannot slip through.

// allExecutions and allIdentities are the closed vocabularies. If either grows, the completeness test
// below fails until the new combinations are decided.
var allExecutions = []TestExecution{
	TestExecutionOK, TestExecutionNonzero, TestExecutionTimeout,
	TestExecutionCancelled, TestExecutionSpawnFailed, TestExecutionInterrupted,
}

var allIdentities = []TestIdentity{
	TestIdentityUnchanged, TestIdentityChanged, TestIdentityUnobserved,
}

// TestOutcomeTableIsExactlyTheDesign enumerates all eighteen combinations.
//
// Each row carries the reason as well as the answer, because several of them are decisions rather than
// deductions — and a table of bare expectations would let a future change quietly reverse one.
func TestOutcomeTableIsExactlyTheDesign(t *testing.T) {
	type key struct {
		e TestExecution
		i TestIdentity
	}
	want := map[key]TestOutcome{}
	set := func(e TestExecution, i TestIdentity, o TestOutcome) { want[key{e, i}] = o }

	// Cancel outranks EVERYTHING, including a timeout: operator intent supersedes a bound, and calling
	// a cancelled attempt a failure would blame the code for a human's decision.
	for _, i := range allIdentities {
		set(TestExecutionCancelled, i, OutcomeCancelled)
	}
	// Neither of these is a statement about the code, so neither may spend the fix budget — whatever
	// identity says, including changed.
	for _, i := range allIdentities {
		set(TestExecutionSpawnFailed, i, OutcomeIndeterminate)
		set(TestExecutionInterrupted, i, OutcomeIndeterminate)
	}
	// The gate cannot certify a tree it did not observe. Passing on an unobserved identity would be the
	// strongest claim available made on the weakest evidence.
	for _, e := range []TestExecution{TestExecutionOK, TestExecutionNonzero, TestExecutionTimeout} {
		set(e, TestIdentityUnobserved, OutcomeIndeterminate)
	}
	// A changed tree FAILS even on a clean exit 0: the result describes a tree that no longer exists,
	// because the run was edited underneath the gate.
	for _, e := range []TestExecution{TestExecutionOK, TestExecutionNonzero, TestExecutionTimeout} {
		set(e, TestIdentityChanged, OutcomeFail)
	}
	// The ordinary cases.
	set(TestExecutionOK, TestIdentityUnchanged, OutcomePass)
	set(TestExecutionNonzero, TestIdentityUnchanged, OutcomeFail)
	set(TestExecutionTimeout, TestIdentityUnchanged, OutcomeFail)

	if len(want) != len(allExecutions)*len(allIdentities) {
		t.Fatalf("the expectation table has %d rows, want %d — it does not cover the cross product",
			len(want), len(allExecutions)*len(allIdentities))
	}
	for _, e := range allExecutions {
		for _, i := range allIdentities {
			got, err := Outcome(e, i)
			if err != nil {
				t.Fatalf("Outcome(%q, %q): %v", e, i, err)
			}
			if got != want[key{e, i}] {
				t.Fatalf("Outcome(%q, %q) = %q, want %q", e, i, got, want[key{e, i}])
			}
		}
	}
}

// TestOutcomeIsTotalOverTheEnumeratedVocabularies. Totality is the property the shape exists for, so it
// is checked directly rather than inferred from the table above passing.
func TestOutcomeIsTotalOverTheEnumeratedVocabularies(t *testing.T) {
	for _, e := range allExecutions {
		for _, i := range allIdentities {
			if _, err := Outcome(e, i); err != nil {
				t.Fatalf("no verdict for the enumerated pair (%q, %q): %v", e, i, err)
			}
		}
	}
}

// TestUnknownObservationsAreRefusedNotGuessed.
//
// A verdict the gate cannot justify is worse than no verdict: it would be acted on. An unrecognised
// value on either axis therefore produces an error rather than falling through to a default, which is
// exactly what a precedence list would have done with it.
func TestUnknownObservationsAreRefusedNotGuessed(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    TestExecution
		i    TestIdentity
		want string
	}{
		{"unknown execution", "probably-fine", TestIdentityUnchanged, "execution observation"},
		{"absent execution", "", TestIdentityUnchanged, "execution observation"},
		{"unknown identity", TestExecutionOK, "maybe", "identity observation"},
		{"absent identity", TestExecutionOK, "", "identity observation"},
		{"both unknown", "x", "y", "execution observation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Outcome(tc.e, tc.i)
			if err == nil {
				t.Fatalf("Outcome(%q, %q) = %q, want a refusal", tc.e, tc.i, got)
			}
			if got != "" {
				t.Fatalf("a refused pair still produced the verdict %q", got)
			}
		})
	}
}

// TestOnlyAPassAdvancesAndOnlyAFailSpendsBudget states the two consequences the verdict exists to
// drive, so a future edit that made a second value "advance" has to change this file too.
func TestOnlyAPassAdvancesAndOnlyAFailSpendsBudget(t *testing.T) {
	advancing := 0
	budgetSpending := 0
	for _, e := range allExecutions {
		for _, i := range allIdentities {
			o, err := Outcome(e, i)
			if err != nil {
				t.Fatalf("Outcome(%q, %q): %v", e, i, err)
			}
			switch o {
			case OutcomePass:
				advancing++
				if e != TestExecutionOK || i != TestIdentityUnchanged {
					t.Fatalf("(%q, %q) advances the run; only a clean exit on an unchanged tree may", e, i)
				}
			case OutcomeFail:
				budgetSpending++
				if i == TestIdentityUnobserved {
					t.Fatalf("(%q, %q) spends the fix budget on an unobserved tree", e, i)
				}
				if e == TestExecutionSpawnFailed || e == TestExecutionInterrupted || e == TestExecutionCancelled {
					t.Fatalf("(%q, %q) spends the fix budget for something that is not a code failure", e, i)
				}
			}
		}
	}
	if advancing != 1 {
		t.Fatalf("%d combinations advance the run, want exactly one", advancing)
	}
	if budgetSpending == 0 {
		t.Fatal("no combination spends the fix budget; an ordinary test failure must")
	}
}
