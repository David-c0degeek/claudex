package testgate

import (
	"errors"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

// TestEveryDesignLaunchFailureRowReachesItsStatedRecord walks the non-crash table of section 9.
//
// These are the failures where the coordinator is ALIVE and something else went wrong, which is what
// separates them from the crash table. The rows are named as the design names them.
func TestEveryDesignLaunchFailureRowReachesItsStatedRecord(t *testing.T) {
	for _, tc := range []struct {
		row       string
		fault     LaunchFault
		bound     bool
		result    bool
		execution state.TestExecution
		needsFact bool
	}{
		{"supervisor spawn failure, before the CAS", FaultSupervisorSpawn, false, false, "", false},
		{"handshake failure, before the CAS", FaultHandshake, false, false, "", false},
		{"arming deadline expiry, before the CAS", FaultArmingDeadline, false, false, "", false},
		{"the GO write fails after the CAS", FaultGoWrite, true, false, state.TestExecutionInterrupted, true},
		{"supervisor lost before the command spawns", FaultSupervisorLostBeforeSpawn, true, false, state.TestExecutionInterrupted, true},
		{"supervisor lost after the command spawned", FaultSupervisorLostAfterSpawn, true, false, state.TestExecutionInterrupted, true},
		{"the real command fails to start after GO", FaultCommandDidNotStart, true, true, state.TestExecutionSpawnFailed, true},
	} {
		t.Run(tc.row, func(t *testing.T) {
			got, err := ClassifyLaunchFault(tc.fault)
			if err != nil {
				t.Fatalf("ClassifyLaunchFault: %v", err)
			}
			if got.AttemptBound != tc.bound {
				t.Fatalf("AttemptBound = %t, want %t (%s)", got.AttemptBound, tc.bound, got.Reason)
			}
			if got.ResultIssued != tc.result {
				t.Fatalf("ResultIssued = %t, want %t (%s)", got.ResultIssued, tc.result, got.Reason)
			}
			if got.Execution != tc.execution {
				t.Fatalf("Execution = %q, want %q (%s)", got.Execution, tc.execution, got.Reason)
			}
			if got.CompletionFactRequired != tc.needsFact {
				t.Fatalf("CompletionFactRequired = %t, want %t (%s)", got.CompletionFactRequired, tc.needsFact, got.Reason)
			}
		})
	}
}

// TestOnlyTheOperatorsOwnCommandIsEverRecordedAsSpawnFailed is the distinction the design says governs
// this whole table.
//
// `spawn_failed` is a statement about the CONFIGURED TEST COMMAND and it points an operator at the run
// policy. A supervisor or protocol fault is coordinator infrastructure. Both map to `indeterminate`, so
// nothing about the routing changes - but the record is what a human reads, and sending somebody to
// debug a test command because a helper process died is the same family of lie as routing an
// infrastructure failure to FIX.
func TestOnlyTheOperatorsOwnCommandIsEverRecordedAsSpawnFailed(t *testing.T) {
	for _, f := range AllLaunchFaults() {
		got, err := ClassifyLaunchFault(f)
		if err != nil {
			t.Fatalf("ClassifyLaunchFault(%q): %v", f, err)
		}
		isTheCommand := f == FaultCommandDidNotStart
		if (got.Execution == state.TestExecutionSpawnFailed) != isTheCommand {
			t.Fatalf("%q records %q; spawn_failed must be reachable from the configured command and nothing else",
				f, got.Execution)
		}
	}
}

// TestNoLaunchFaultIsAStatementAboutTheCode.
//
// None of these is evidence about the code under test, so none may advance the run and none may spend
// the fix budget. Asserting it through the real outcome function rather than by inspecting the recorded
// value is what makes it a claim about the ROUTING rather than about a string.
func TestNoLaunchFaultIsAStatementAboutTheCode(t *testing.T) {
	for _, f := range AllLaunchFaults() {
		got, err := ClassifyLaunchFault(f)
		if err != nil {
			t.Fatalf("ClassifyLaunchFault(%q): %v", f, err)
		}
		if got.BudgetSpent {
			t.Fatalf("%q spends the code-fix budget", f)
		}
		if got.Execution == "" {
			continue // nothing is recorded at all, because no attempt exists
		}
		for _, id := range state.AllTestIdentities() {
			outcome, err := state.Outcome(got.Execution, id)
			if err != nil {
				t.Fatalf("Outcome(%q, %q): %v", got.Execution, id, err)
			}
			if outcome == state.OutcomePass || outcome == state.OutcomeFail {
				t.Fatalf("%q with identity %q routes to %q; a launch fault is not evidence about the code",
					f, id, outcome)
			}
		}
	}
}

// TestAnAttemptIsBoundExactlyWhenTheFaultIsAfterTheCAS.
//
// The whole table is organised by one boundary, and it is the boundary that decides whether there is
// anything to record against at all. A pre-CAS fault that claimed a bound attempt would leave a ledger
// entry for an attempt that never became active.
func TestAnAttemptIsBoundExactlyWhenTheFaultIsAfterTheCAS(t *testing.T) {
	beforeCAS := map[LaunchFault]bool{
		FaultSupervisorSpawn: true, FaultHandshake: true, FaultArmingDeadline: true,
	}
	for _, f := range AllLaunchFaults() {
		got, err := ClassifyLaunchFault(f)
		if err != nil {
			t.Fatalf("ClassifyLaunchFault(%q): %v", f, err)
		}
		if got.AttemptBound == beforeCAS[f] {
			t.Fatalf("%q reports AttemptBound=%t on the %s side of the CAS",
				f, got.AttemptBound, map[bool]string{true: "near", false: "far"}[beforeCAS[f]])
		}
		if !got.AttemptBound {
			// Nothing exists to carry a record, a completion fact or a budget charge.
			if got.ResultIssued || got.Execution != "" || got.CompletionFactRequired {
				t.Fatalf("%q binds nothing but still records %+v", f, got)
			}
		} else if !got.CompletionFactRequired {
			// The closing rule of section 9: a live-coordinator fault does not weaken it, because the
			// residue to be excluded is identical to the crash case.
			t.Fatalf("%q binds an attempt without requiring the completion fact", f)
		}
	}
}

// TestOnlyAnAuthoritativeStatementIssuesAResult.
//
// A result is the one immutable record of what the command did. Only the row that actually knows
// something about the command - it could not be started - may publish one; the rest lost the ability to
// obtain an outcome, which is not the same as having obtained one.
func TestOnlyAnAuthoritativeStatementIssuesAResult(t *testing.T) {
	for _, f := range AllLaunchFaults() {
		got, err := ClassifyLaunchFault(f)
		if err != nil {
			t.Fatalf("ClassifyLaunchFault(%q): %v", f, err)
		}
		if got.ResultIssued != (f == FaultCommandDidNotStart) {
			t.Fatalf("%q reports ResultIssued=%t", f, got.ResultIssued)
		}
	}
}

// TestTheLaunchTableIsExactlyTheVocabulary asserts completeness in both directions, so a fault added
// without a row fails here rather than being classified by whatever a default would have produced.
func TestTheLaunchTableIsExactlyTheVocabulary(t *testing.T) {
	want := map[LaunchFault]bool{}
	for _, f := range AllLaunchFaults() {
		want[f] = true
		if _, ok := launchTable[f]; !ok {
			t.Errorf("no classification for %q", f)
		}
	}
	for f := range launchTable {
		if !want[f] {
			t.Errorf("the table classifies %q, which is not in the vocabulary", f)
		}
	}
}

// TestAnUnclassifiedLaunchFaultIsRefused.
//
// A default here would silently be `spawn_failed` or `interrupted` for something nobody classified, and
// both of those are claims about WHERE the problem is.
func TestAnUnclassifiedLaunchFaultIsRefused(t *testing.T) {
	for _, f := range []LaunchFault{"", "something_went_wrong", "SUPERVISOR_SPAWN"} {
		_, err := ClassifyLaunchFault(f)
		if !errors.Is(err, ErrLifecycle) {
			t.Fatalf("%q: err = %v, want ErrLifecycle", f, err)
		}
		if !strings.Contains(err.Error(), "no classification") {
			t.Fatalf("%q: err = %v", f, err)
		}
	}
}

// TestEveryLaunchRowStatesWhyItDecidedWhatItDid.
//
// The account is what an operator reads to find out whether the problem is theirs or the tool's, which
// is the entire point of separating these rows.
func TestEveryLaunchRowStatesWhyItDecidedWhatItDid(t *testing.T) {
	seen := map[string]LaunchFault{}
	for f, d := range launchTable {
		if strings.TrimSpace(d.Reason) == "" {
			t.Fatalf("%q is classified with no account of why", f)
		}
		if other, dup := seen[d.Reason]; dup {
			t.Fatalf("%q and %q share the account %q", f, other, d.Reason)
		}
		seen[d.Reason] = f
	}
}

// TestEveryRecordedLaunchFaultNamesItsAuthority.
//
// These are observations a LIVE coordinator makes about its own infrastructure, so the authority is
// settled here rather than inferred by whoever composes this later. The one shared rule refuses an
// interruption attributed to the runner - had the runner been alive to report, it would have reported
// the outcome instead - so a table that omitted the authority could not produce a truthful terminal at
// all.
func TestEveryRecordedLaunchFaultNamesItsAuthority(t *testing.T) {
	for _, f := range AllLaunchFaults() {
		got, err := ClassifyLaunchFault(f)
		if err != nil {
			t.Fatalf("ClassifyLaunchFault(%q): %v", f, err)
		}
		if got.Execution == "" {
			if got.TerminalAuthor != "" {
				t.Fatalf("%q records nothing but still names an authority %q", f, got.TerminalAuthor)
			}
			continue
		}
		if !state.KnownTerminalAuthor(got.TerminalAuthor) {
			t.Fatalf("%q records %q with no known authority", f, got.Execution)
		}
		// Checked through the PRODUCTION rule, so this table cannot drift from what state will accept.
		if !state.AuthorityAgreesWithExecution(got.TerminalAuthor, got.Execution) {
			t.Fatalf("%q attributes %q to %q, which cannot have observed it", f, got.Execution, got.TerminalAuthor)
		}
		if got.Execution == state.TestExecutionInterrupted && got.TerminalAuthor != state.TerminalByCoordinator {
			t.Fatalf("%q is a live-coordinator observation but is attributed to %q", f, got.TerminalAuthor)
		}
	}
}
