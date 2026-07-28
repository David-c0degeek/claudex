package testgate

import (
	"fmt"

	"github.com/David-c0degeek/claudex/internal/state"
)

// LaunchFault is a failure with the coordinator STILL ALIVE, which is what separates this vocabulary
// from the crash table above.
//
// One distinction governs all of it. `spawn_failed` is a statement about the CONFIGURED TEST COMMAND,
// and it points an operator at the run policy. A supervisor or protocol fault is coordinator
// infrastructure, and recording it as `spawn_failed` sends somebody to debug a test command that was
// never even reached. Both map to `indeterminate`, so nothing about the routing changes - but the record
// is what a human reads, and that makes it the same family of lie as routing an infrastructure failure
// to FIX.
type LaunchFault string

const (
	// FaultSupervisorSpawn, FaultHandshake and FaultArmingDeadline all happen BEFORE the active CAS.
	FaultSupervisorSpawn LaunchFault = "supervisor_spawn"
	FaultHandshake       LaunchFault = "handshake"
	FaultArmingDeadline  LaunchFault = "arming_deadline"
	// FaultGoWrite is a GO that failed to reach a supervisor that had already died. Framing guarantees a
	// truncated GO is not a GO, so no command started.
	FaultGoWrite LaunchFault = "go_write"
	// FaultSupervisorLostBeforeSpawn and FaultSupervisorLostAfterSpawn cover a supervisor that died or
	// sent corrupt frames on either side of the command starting.
	FaultSupervisorLostBeforeSpawn LaunchFault = "supervisor_lost_before_spawn"
	FaultSupervisorLostAfterSpawn  LaunchFault = "supervisor_lost_after_spawn"
	// FaultCommandDidNotStart is the ONLY member of this vocabulary that is about the operator's command.
	FaultCommandDidNotStart LaunchFault = "command_did_not_start"
)

// AllLaunchFaults is the closed vocabulary.
func AllLaunchFaults() []LaunchFault {
	return []LaunchFault{
		FaultSupervisorSpawn, FaultHandshake, FaultArmingDeadline, FaultGoWrite,
		FaultSupervisorLostBeforeSpawn, FaultSupervisorLostAfterSpawn, FaultCommandDidNotStart,
	}
}

// LaunchDisposition is what a live coordinator does about one of those faults.
type LaunchDisposition struct {
	// AttemptBound says whether an attempt reference exists, which is decided by whether the fault
	// happened before or after the active CAS.
	AttemptBound bool
	// ResultIssued says whether an immutable result record is published. Only an authoritative statement
	// about the command produces one.
	ResultIssued bool
	// Execution is the recorded observation, empty when no attempt exists to record anything against.
	Execution state.TestExecution
	// TerminalAuthor is WHO the recorded account belongs to. It is settled here rather than inferred
	// later: these are observations a LIVE coordinator makes about its own infrastructure, and the one
	// authority rule refuses an interruption attributed to the runner - which is the value composition
	// would otherwise have had to guess.
	TerminalAuthor state.TerminalAuthor
	// CompletionFactRequired says whether the attempt can only become retryable once the section 1
	// cleanup fact is proven. A live-coordinator fault does not weaken that rule: the residue to be
	// excluded is identical to the crash case.
	CompletionFactRequired bool
	// BudgetSpent says whether this consumes the code-fix budget. Nothing here does - none of these
	// faults is a statement about the code.
	BudgetSpent bool
	Reason      string
}

var launchTable = map[LaunchFault]LaunchDisposition{
	FaultSupervisorSpawn: {
		Reason: "the supervisor could not be started, before anything was bound"},
	FaultHandshake: {
		Reason: "the supervisor handshake failed, before anything was bound"},
	FaultArmingDeadline: {
		Reason: "arming did not complete within its deadline, before anything was bound"},
	FaultGoWrite: {
		AttemptBound: true, Execution: state.TestExecutionInterrupted, TerminalAuthor: state.TerminalByCoordinator, CompletionFactRequired: true,
		Reason: "the GO could not be delivered; framing guarantees a truncated GO is not a GO, so no command started"},
	FaultSupervisorLostBeforeSpawn: {
		AttemptBound: true, Execution: state.TestExecutionInterrupted, TerminalAuthor: state.TerminalByCoordinator, CompletionFactRequired: true,
		Reason: "the supervisor was lost before the command spawned, so its facts are untrustworthy"},
	FaultSupervisorLostAfterSpawn: {
		AttemptBound: true, Execution: state.TestExecutionInterrupted, TerminalAuthor: state.TerminalByCoordinator, CompletionFactRequired: true,
		Reason: "the supervisor was lost after the command spawned; partial streams are retained"},
	FaultCommandDidNotStart: {
		AttemptBound: true, ResultIssued: true, Execution: state.TestExecutionSpawnFailed,
		TerminalAuthor:         state.TerminalByRunner,
		CompletionFactRequired: true,
		Reason:                 "the configured test command could not be started, which is an authoritative statement about the policy"},
}

// ClassifyLaunchFault decides what one live-coordinator fault is recorded as.
//
// It refuses an unknown fault rather than choosing a default. A default here would silently be
// `spawn_failed` or `interrupted` for something nobody classified, and both of those are claims about
// where the problem is.
func ClassifyLaunchFault(f LaunchFault) (LaunchDisposition, error) {
	d, ok := launchTable[f]
	if !ok {
		return LaunchDisposition{}, fmt.Errorf("%w: no classification for the launch fault %q", ErrLifecycle, f)
	}
	return d, nil
}
