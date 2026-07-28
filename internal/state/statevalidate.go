package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/redact"
)

var knownLifecycles = map[Lifecycle]bool{
	LifecycleRunning: true, LifecyclePaused: true, LifecyclePausedBudget: true,
	LifecycleRateLimited: true, LifecycleCancelled: true, LifecycleFailedRetryable: true,
	LifecycleFailedTerminal: true, LifecycleCompleted: true,
}

var knownPhases = map[Phase]bool{
	PhaseInit: true, PhasePlanDraft: true, PhasePlanCritique: true, PhasePlanRevise: true,
	PhaseImplementStep: true, PhaseCheckpoint: true, PhaseFix: true, PhaseTests: true,
	PhaseVerify: true, PhaseAwaitGuidance: true, PhaseDone: true,
}

var knownFSClasses = map[string]bool{
	"supported-local": true, "known-unsupported": true, "unknown": true,
}

// strictDecodeRunState decodes exactly one JSON value with no unknown fields and
// no trailing content.
func strictDecodeRunState(data []byte) (RunState, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rs RunState
	if err := dec.Decode(&rs); err != nil {
		return RunState{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return RunState{}, fmt.Errorf("unexpected trailing content")
	}
	return rs, nil
}

// validate enforces the invariants that hold for every persisted run state.
func validate(rs *RunState) error {
	if rs.SchemaVersion != RunStateVersion {
		return fmt.Errorf("schema_version %d != %d", rs.SchemaVersion, RunStateVersion)
	}
	if !validRunID(rs.RunID) {
		return fmt.Errorf("invalid run_id %q", rs.RunID)
	}
	if rs.Revision == 0 {
		return fmt.Errorf("revision must be > 0")
	}
	if !knownLifecycles[rs.Lifecycle] {
		return fmt.Errorf("unknown lifecycle %q", rs.Lifecycle)
	}
	if !knownPhases[rs.Phase] {
		return fmt.Errorf("unknown phase %q", rs.Phase)
	}
	if rs.CreatedUnix <= 0 || rs.StartedUnix < 0 || rs.DeadlineUnix < 0 {
		return fmt.Errorf("invalid timestamps")
	}
	if err := validateSnapshot("task_snapshot", rs.TaskSnapshot); err != nil {
		return err
	}
	if err := validateSnapshot("policy_snapshot", rs.PolicySnapshot); err != nil {
		return err
	}
	if err := rs.EffectivePolicy.Validate(); err != nil {
		return fmt.Errorf("effective_policy: %w", err)
	}
	if err := validateFS(rs); err != nil {
		return err
	}
	if strings.TrimSpace(rs.Base) == "" {
		return fmt.Errorf("base is required")
	}
	if !isGitOID(rs.BaseCommit) {
		return fmt.Errorf("base_commit is not a git object id (40 or 64 lower-hex)")
	}
	// Workspace identity is DERIVED from the run id, never a caller claim.
	if rs.WorktreeRelPath != WorktreeRelPathFor(rs.RunID) {
		return fmt.Errorf("worktree_rel_path must be the derived %q", WorktreeRelPathFor(rs.RunID))
	}
	if rs.RunBranch != RunBranchFor(rs.RunID) {
		return fmt.Errorf("run_branch must be the derived %q", RunBranchFor(rs.RunID))
	}
	if rs.Base != rs.EffectivePolicy.BaseBranch {
		return fmt.Errorf("base %q must equal effective_policy.base_branch %q", rs.Base, rs.EffectivePolicy.BaseBranch)
	}
	hasArgv := len(rs.EffectivePolicy.TestGate.Argv) > 0
	if hasArgv == rs.EffectivePolicy.TestGate.Disabled {
		return fmt.Errorf("effective_policy.test_gate must set exactly one of argv or disabled")
	}
	if locatorKey(rs.TaskSnapshot.RelPath) == locatorKey(rs.PolicySnapshot.RelPath) {
		return fmt.Errorf("task and policy snapshot paths must be distinct")
	}
	if rs.PendingTxnID != "" && !validID(rs.PendingTxnID) {
		return fmt.Errorf("invalid pending_txn_id")
	}
	if err := validateTimes(rs); err != nil {
		return err
	}
	if err := validateCounters(rs.Counters); err != nil {
		return err
	}
	if err := validateAcceptedTurns(rs); err != nil {
		return err
	}
	if err := validateRef("assignment", rs.Assignment, rs.Revision); err != nil {
		return err
	}
	if err := validateEvidenceBinding(rs); err != nil {
		return err
	}
	if err := validateRef("first_turn", rs.FirstTurn, rs.Revision); err != nil {
		return err
	}
	if rs.Phase == PhaseInit && rs.FirstTurn != nil {
		return fmt.Errorf("INIT state must not have a first_turn")
	}
	if err := validateRef("gate", rs.Gate, rs.Revision); err != nil {
		return err
	}
	if err := validateProjection("recovery", rs.Recovery, rs.Revision); err != nil {
		return err
	}
	if err := validateProjection("failure", rs.Failure, rs.Revision); err != nil {
		return err
	}
	if err := validateV5Shape(rs); err != nil {
		return err
	}
	if err := validateV5Refs(rs); err != nil {
		return err
	}
	if err := validateBudgetHonesty(rs); err != nil {
		return err
	}
	if err := validateTestAttempts(rs); err != nil {
		return err
	}
	// The frozen environment must still be authorized by the frozen policy. Validating it here rather
	// than trusting the bootstrap means a state that was hand-edited, or written by a build whose
	// authority model differed, cannot hand a command variables the policy never allowed.
	// The run directory is DERIVED from the run id, never taken from the state being validated, so a
	// tampered state cannot supply the very path its scratch layout is then checked against.
	if err := rs.ResolvedExecution.ValidateFor(rs.EffectivePolicy.TestGate, config.HostGOOS(), RunDirRelFor(rs.RunID)); err != nil {
		return fmt.Errorf("resolved_execution: %w", err)
	}
	return nil
}

// validateAttemptTransition binds the attempt lifecycle to the transitions that may produce it.
//
// The structural rules in validateTestAttempts prove an entry is WELL FORMED. They do not prove it
// describes anything that happened, and that gap is the whole of this function: without it a mutation
// could invent a finalized attempt out of nothing, swap the active ref's tested tree, un-cancel a
// cancelled one, or append several outcomes at once. Every rule below closes one of those.
func validateAttemptTransition(old, next *RunState) error {
	// 1. Append-only, BY VALUE. A length check alone would let an entry be rewritten in place while the
	// count stayed the same — precisely how an indeterminate attempt could later be reclassified as a
	// clean pass, erasing the distinction between a broken environment and failing code.
	if len(next.TestAttempts) < len(old.TestAttempts) {
		return fmt.Errorf("test attempts must not shrink (%d -> %d)", len(old.TestAttempts), len(next.TestAttempts))
	}
	for i := range old.TestAttempts {
		if !reflect.DeepEqual(next.TestAttempts[i], old.TestAttempts[i]) {
			return fmt.Errorf("finalized test attempt %d is immutable", i)
		}
	}
	// 2. At most ONE finalization per transition. Finalization is the act of moving the single active
	// ref into the ledger, and there is only ever one ref to move.
	appended := len(next.TestAttempts) - len(old.TestAttempts)
	if appended > 1 {
		return fmt.Errorf("a transition finalized %d attempts; only the one active attempt can be finalized", appended)
	}
	if appended == 1 {
		e := next.TestAttempts[len(next.TestAttempts)-1]
		if e.BoundRevision != next.Revision {
			return fmt.Errorf("newly finalized test attempt must bind to revision %d, got %d", next.Revision, e.BoundRevision)
		}
		// 3. It must be THE active attempt, not a fabricated one. Comparing the whole identity — not
		// just the id — is what stops a finalization claiming a different tree than the one the attempt
		// was started against.
		a := old.ActiveTestAttempt
		if a == nil {
			return fmt.Errorf("test attempt %q was finalized with no active attempt to finalize", e.AttemptID)
		}
		if e.AttemptID != a.AttemptID || e.StartRevision != a.StartRevision ||
			e.TestedCommit != a.TestedCommit || e.TestedTree != a.TestedTree {
			return fmt.Errorf("finalized attempt %q does not match the active attempt %q it claims to finalize", e.AttemptID, a.AttemptID)
		}
		// Finalization CONSUMES the ref, and that is enforced STRUCTURALLY rather than here: an entry
		// whose attempt is also the active one is not a representable state at all (see
		// validateTestAttempts), so a duplicate check at this level could never fire. It is left out
		// rather than written as a guard that reads like one and cannot.
	}

	switch {
	case old.ActiveTestAttempt == nil && next.ActiveTestAttempt != nil:
		// 5. A new attempt starts AT the revision that starts it, so its identity cannot be
		// back-dated to borrow an earlier state's authorization.
		if next.ActiveTestAttempt.StartRevision != next.Revision {
			return fmt.Errorf("a new active attempt must start at revision %d, got %d", next.Revision, next.ActiveTestAttempt.StartRevision)
		}
		if next.ActiveTestAttempt.CancelPending {
			return fmt.Errorf("a new active attempt cannot already be cancel-pending")
		}
	case old.ActiveTestAttempt != nil && next.ActiveTestAttempt != nil:
		o, n := old.ActiveTestAttempt, next.ActiveTestAttempt
		if o.AttemptID != n.AttemptID {
			// Finalizing one attempt and starting its replacement are DISTINCT guarded operations, and
			// one CAS may not do both. Allowing it whenever a ledger entry was appended looked
			// reasonable — the old attempt does reach the ledger — but it lets the next attempt be
			// created without the run ever passing through the state that authorizes starting one:
			// ownerless TESTS with NO active attempt. A ledger-only indeterminate finalization satisfies
			// placement on its own, so the intermediate authorization would simply never be observed.
			return fmt.Errorf("active attempt %q was replaced by %q in one transition; finalizing and starting are separate operations", o.AttemptID, n.AttemptID)
		}
		// 6. A live attempt's identity is frozen. Its tree, its intent and its start are what the
		// eventual outcome is a statement ABOUT, so changing them mid-flight would let the answer be
		// re-pointed at a different question.
		if o.StartRevision != n.StartRevision || o.TestedCommit != n.TestedCommit ||
			o.TestedTree != n.TestedTree || o.IntentDigest != n.IntentDigest {
			return fmt.Errorf("the active attempt %q is immutable while it is active", o.AttemptID)
		}
		// 7. Cancellation is monotone. Un-cancelling would let a run that had already told status and
		// wait it was cancelled quietly continue.
		if o.CancelPending && !n.CancelPending {
			return fmt.Errorf("the active attempt %q cannot un-cancel", o.AttemptID)
		}
	case old.ActiveTestAttempt != nil && next.ActiveTestAttempt == nil:
		// 8. The ref may only be cleared BY finalizing it. Dropping it silently would leave a runner
		// holding facts that nothing can accept, and a live command with no record that it exists.
		if appended == 0 {
			return fmt.Errorf("active attempt %q was cleared without being finalized", old.ActiveTestAttempt.AttemptID)
		}
	}

	// 9. Where an attempt may be active at all. It belongs to an ownerless TESTS phase — no agent holds
	// the turn, the coordinator is running the gate — or to a run already marked cancelled whose
	// in-flight attempt still has to be bound. The second arm is what lets a cancel take effect
	// immediately, so status and wait wake, without discarding the only identity the runner can
	// finalize.
	if a := next.ActiveTestAttempt; a != nil {
		cancelled := next.Lifecycle == LifecycleCancelled && a.CancelPending
		ownerlessTests := next.Phase == PhaseTests && next.Assignment == nil
		if !cancelled && !ownerlessTests {
			return fmt.Errorf("an active test attempt requires ownerless TESTS or a cancelled run awaiting its terminal binding, got phase %s lifecycle %s assigned=%v",
				next.Phase, next.Lifecycle, next.Assignment != nil)
		}
	}
	return nil
}

// validateTestAttempts enforces the v8 attempt invariants.
//
// The ledger is the run's only record of what the mechanical gate actually did, and every rule here
// exists because its absence would let the record say something that is not true:
//
//   - an active ref must be well formed and bound to a tree, or an outcome would be a claim about "the
//     code" with no way to say which code;
//   - the ledger is bounded by the policy's own ceiling, because indeterminate retries do not spend the
//     fix budget and would otherwise grow a full-snapshot state without limit;
//   - one result digest may not appear twice, because the digest IS the evidence for an outcome and the
//     same evidence cannot authorize two different answers;
//   - an attempt id may not be reused, since the containment, the intent and the result all bind it.
func validateTestAttempts(rs *RunState) error {
	if a := rs.ActiveTestAttempt; a != nil {
		if !validID(a.AttemptID) {
			return fmt.Errorf("active_test_attempt.attempt_id is not a valid id")
		}
		if a.StartRevision == 0 || a.StartRevision > rs.Revision {
			return fmt.Errorf("active_test_attempt.start_revision %d out of range (1..%d)", a.StartRevision, rs.Revision)
		}
		if !isGitOID(a.TestedCommit) {
			return fmt.Errorf("active_test_attempt.tested_commit is not a git object id")
		}
		if !isGitOID(a.TestedTree) {
			return fmt.Errorf("active_test_attempt.tested_tree is not a git object id")
		}
		if !isSHA256Hex(a.IntentDigest) {
			return fmt.Errorf("active_test_attempt.intent_digest is not a sha256")
		}
	}
	max := rs.EffectivePolicy.Limits.MaxTestAttempts
	if max > 0 && len(rs.TestAttempts) > max {
		return fmt.Errorf("test_attempts has %d entries, exceeding limits.max_test_attempts %d", len(rs.TestAttempts), max)
	}
	seenID := make(map[string]bool, len(rs.TestAttempts))
	seenDigest := make(map[string]bool, len(rs.TestAttempts))
	for i, e := range rs.TestAttempts {
		if !validID(e.AttemptID) {
			return fmt.Errorf("test_attempts[%d].attempt_id is not a valid id", i)
		}
		if seenID[e.AttemptID] {
			return fmt.Errorf("test_attempts[%d] repeats attempt id %q", i, e.AttemptID)
		}
		seenID[e.AttemptID] = true
		if e.StartRevision == 0 || e.StartRevision > rs.Revision {
			return fmt.Errorf("test_attempts[%d].start_revision %d out of range (1..%d)", i, e.StartRevision, rs.Revision)
		}
		if e.BoundRevision == 0 || e.BoundRevision > rs.Revision {
			return fmt.Errorf("test_attempts[%d].bound_revision %d out of range (1..%d)", i, e.BoundRevision, rs.Revision)
		}
		if e.BoundRevision < e.StartRevision {
			return fmt.Errorf("test_attempts[%d] was bound at revision %d, before it started at %d", i, e.BoundRevision, e.StartRevision)
		}
		if !isGitOID(e.TestedCommit) || !isGitOID(e.TestedTree) {
			return fmt.Errorf("test_attempts[%d] tested identity is not a git object id", i)
		}
		if !isSHA256Hex(e.ResultDigest) {
			return fmt.Errorf("test_attempts[%d].result_digest is not a sha256", i)
		}
		if seenDigest[e.ResultDigest] {
			return fmt.Errorf("test_attempts[%d] reuses a result digest already bound to another outcome", i)
		}
		seenDigest[e.ResultDigest] = true
		// Validated through the same total function the gate acts on, so a stored pair that the
		// verdict cannot be derived from is not storable at all.
		if _, err := Outcome(e.Execution, e.Identity); err != nil {
			return fmt.Errorf("test_attempts[%d]: %w", i, err)
		}
		if strings.TrimSpace(e.TerminalReason) == "" || len(e.TerminalReason) > 256 {
			return fmt.Errorf("test_attempts[%d].terminal_reason is required and bounded", i)
		}
		// An attempt cannot be both active and finalized: the ref is MOVED into the ledger, not copied.
		if rs.ActiveTestAttempt != nil && rs.ActiveTestAttempt.AttemptID == e.AttemptID {
			return fmt.Errorf("test_attempts[%d] finalizes attempt %q while it is still the active one", i, e.AttemptID)
		}
	}
	return nil
}

// validateEvidenceBinding enforces the v7 global invariant in both directions: a live assignment for
// a phase the lead cannot edit in is actionable ONLY through a hash-bound review-evidence packet, so
// the binding exists if and only if such an assignment exists.
//
//   - a read-only actionable assignment REQUIRES a binding — otherwise the turn would be handed out
//     with no defined, verifiable thing to review;
//   - an IMPLEMENT_STEP/FIX assignment must have NONE — that turn carries a mutable worktree, and a
//     packet alongside it would be a second, disagreeing source of what the turn may see;
//   - no assignment means no binding — an ownerless, gated, or terminal state reviews nothing, and a
//     stale binding left behind would outlive the turn that authorized it.
//
// The bound turn/revision must equal the assignment's, and the path must be the DERIVED packet
// manifest for that turn, so a persisted binding can never point at another turn's packet.
func validateEvidenceBinding(rs *RunState) error {
	ev := rs.Evidence
	if rs.Assignment == nil {
		if ev != nil {
			return fmt.Errorf("evidence binding present with no assignment")
		}
		return nil
	}
	if RepoEditPhase(rs.Phase) {
		if ev != nil {
			return fmt.Errorf("evidence binding present for a %s assignment, which carries a worktree", rs.Phase)
		}
		return nil
	}
	if ev == nil {
		return fmt.Errorf("read-only %s assignment has no evidence binding", rs.Phase)
	}
	if ev.TurnID != rs.Assignment.ID {
		return fmt.Errorf("evidence binding names turn %q, not the assigned %q", ev.TurnID, rs.Assignment.ID)
	}
	if ev.IssuedRevision != rs.Assignment.IssuedRevision {
		return fmt.Errorf("evidence binding was issued at revision %d, not the assignment's %d", ev.IssuedRevision, rs.Assignment.IssuedRevision)
	}
	if ev.ManifestRelPath != evidence.PacketManifestRel(ev.TurnID) {
		return fmt.Errorf("evidence binding path is not the derived packet manifest for turn %q", ev.TurnID)
	}
	if !isSHA256Hex(ev.RootDigest) {
		return fmt.Errorf("evidence binding root digest is not a sha256")
	}
	return nil
}

// isSHA256Hex reports whether s is a 64-character lower-hex digest.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validateInit adds the requirements specific to the first generation.
func validateInit(rs *RunState) error {
	if rs.Revision != 1 {
		return fmt.Errorf("initial state must be revision 1, got %d", rs.Revision)
	}
	if rs.Lifecycle != LifecycleRunning {
		return fmt.Errorf("initial lifecycle must be running, got %q", rs.Lifecycle)
	}
	if rs.Phase != PhaseInit {
		return fmt.Errorf("initial phase must be %q, got %q", PhaseInit, rs.Phase)
	}
	if len(rs.AcceptedTurns) != 0 {
		return fmt.Errorf("initial state must have no accepted turns")
	}
	if rs.Assignment != nil || rs.Evidence != nil || rs.Gate != nil || rs.Recovery != nil || rs.Failure != nil {
		return fmt.Errorf("initial state must have no assignment, evidence, gate, recovery, or failure")
	}
	if rs.Counters.PlanRevisions != 0 || rs.Counters.TestFixes != 0 || rs.Counters.VerifyFixes != 0 || len(rs.Counters.StepFixes) != 0 {
		return fmt.Errorf("initial state must have zero counters and no step fixes")
	}
	if rs.PendingTxnID != "" {
		return fmt.Errorf("initial state must have no pending transaction")
	}
	if err := validateV5Init(rs); err != nil {
		return err
	}
	return nil
}

// validateTransition enforces immutability, monotonicity, and append-only rules.
func validateTransition(old, next *RunState) error {
	// Bootstrap/input fields are immutable after generation 1.
	if old.RunID != next.RunID {
		return fmt.Errorf("run_id is immutable")
	}
	if old.CreatedUnix != next.CreatedUnix {
		return fmt.Errorf("created_unix is immutable")
	}
	if old.TaskSnapshot != next.TaskSnapshot || old.PolicySnapshot != next.PolicySnapshot {
		return fmt.Errorf("input snapshots are immutable")
	}
	if !reflect.DeepEqual(old.EffectivePolicy, next.EffectivePolicy) {
		return fmt.Errorf("effective_policy is immutable")
	}
	// Frozen means frozen. The environment was resolved from the host ONCE, at first attach, precisely
	// so no later step re-reads ambient values; a mutation able to edit it would reintroduce exactly the
	// drift that freezing exists to prevent, and would do so invisibly, since the command would still
	// report running with "the frozen environment".
	if !reflect.DeepEqual(old.ResolvedExecution, next.ResolvedExecution) {
		return fmt.Errorf("resolved_execution is immutable")
	}
	if old.FS != next.FS {
		return fmt.Errorf("filesystem decision is immutable")
	}
	if old.Base != next.Base || old.BaseCommit != next.BaseCommit {
		return fmt.Errorf("base identity is immutable")
	}
	if old.WorktreeRelPath != next.WorktreeRelPath || old.RunBranch != next.RunBranch {
		return fmt.Errorf("workspace identity is immutable")
	}
	// An absorbing terminal lifecycle (completed/cancelled/failed_terminal) can
	// never be resurrected, so a terminal observation is stable. failed_retryable
	// is deliberately excluded — it may return to running for a retry.
	if IsAbsorbingLifecycle(old.Lifecycle) && next.Lifecycle != old.Lifecycle {
		return fmt.Errorf("absorbing terminal lifecycle %q cannot be changed to %q", old.Lifecycle, next.Lifecycle)
	}
	// Started/deadline are write-once.
	if err := writeOnce("started_unix", old.StartedUnix, next.StartedUnix); err != nil {
		return err
	}
	if err := writeOnce("deadline_unix", old.DeadlineUnix, next.DeadlineUnix); err != nil {
		return err
	}
	// Counters are non-decreasing.
	if next.Counters.PlanRevisions < old.Counters.PlanRevisions ||
		next.Counters.TestFixes < old.Counters.TestFixes ||
		next.Counters.VerifyFixes < old.Counters.VerifyFixes {
		return fmt.Errorf("counters must not decrease")
	}
	if len(next.Counters.StepFixes) < len(old.Counters.StepFixes) {
		return fmt.Errorf("step fixes must not shrink")
	}
	for i := range old.Counters.StepFixes {
		if next.Counters.StepFixes[i] < old.Counters.StepFixes[i] {
			return fmt.Errorf("step fix %d must not decrease", i)
		}
	}
	if err := validateAttemptTransition(old, next); err != nil {
		return err
	}
	// Accepted turns are append-only; existing entries are frozen and a NEW turn
	// must have been accepted at the resulting revision.
	for k, v := range old.AcceptedTurns {
		nv, ok := next.AcceptedTurns[k]
		if !ok || !reflect.DeepEqual(nv, v) { // DeepEqual: AcceptedTurn now holds a *GitCommitEvidence
			return fmt.Errorf("accepted turn %q is immutable", k)
		}
	}
	for k, v := range next.AcceptedTurns {
		if _, existed := old.AcceptedTurns[k]; !existed {
			if v.Receipt.Revision != next.Revision {
				return fmt.Errorf("newly accepted turn %q must bind to revision %d, got %d", k, next.Revision, v.Receipt.Revision)
			}
			// A newly accepted turn must correspond to a real outstanding turn on a
			// LIVE run: running lifecycle, not recovering or gated, in an actionable
			// agent phase, with exactly this turn assigned. So acceptance can never
			// record a turn the run never issued (INIT, no/other assignment) or
			// resurrect a terminal/paused/recovering run.
			if old.Lifecycle != LifecycleRunning {
				return fmt.Errorf("newly accepted turn %q requires a running run, lifecycle is %q", k, old.Lifecycle)
			}
			if old.Recovery != nil || old.Gate != nil {
				return fmt.Errorf("newly accepted turn %q cannot be accepted while the run is recovering or gated", k)
			}
			if old.Assignment == nil || old.Assignment.ID != k {
				return fmt.Errorf("newly accepted turn %q was not the pre-transition assigned turn", k)
			}
			// The assignment must be the one a valid pull could have issued: bound to
			// the current revision. An unrelated mutation that advanced the run while
			// leaving a stale assignment in place cannot be turned into an acceptance.
			if old.Assignment.IssuedRevision != old.Revision {
				return fmt.Errorf("newly accepted turn %q binds a stale assignment issued at revision %d, not the current %d", k, old.Assignment.IssuedRevision, old.Revision)
			}
			if !IsAgentPhase(old.Phase) {
				return fmt.Errorf("newly accepted turn %q accepted in non-agent phase %q", k, old.Phase)
			}
			// The accepted phase is authoritative: it is the phase the turn was in
			// (the phase before this transition advanced it), never a caller claim.
			if v.Phase != old.Phase {
				return fmt.Errorf("newly accepted turn %q phase %q must equal the pre-transition phase %q", k, v.Phase, old.Phase)
			}
		}
	}
	// FirstTurn is write-once append-only issuance history: it proves which first
	// turn was issued at INIT->PLAN_DRAFT and survives every later transition
	// (cancel/operator/terminal), so an external effect can be recognized after the
	// mutable Assignment moves on.
	if old.FirstTurn != nil {
		if next.FirstTurn == nil || *next.FirstTurn != *old.FirstTurn {
			return fmt.Errorf("first_turn is immutable once issued")
		}
	} else if next.FirstTurn != nil {
		// nil->present is the exact "first turn issued" transition: a live, pristine,
		// unassigned INIT advancing to PLAN_DRAFT, issuing the same Ref as the
		// assignment (both bound to the resulting revision) and setting the run clock
		// in this same generation. So the FirstTurn record is an exclusive fact.
		if old.Lifecycle != LifecycleRunning || next.Lifecycle != LifecycleRunning {
			return fmt.Errorf("first_turn may only be issued while the run is running")
		}
		if old.Phase != PhaseInit || next.Phase != PhasePlanDraft {
			return fmt.Errorf("first_turn may only be issued on an INIT->PLAN_DRAFT transition")
		}
		if old.Assignment != nil || old.StartedUnix != 0 || old.DeadlineUnix != 0 {
			return fmt.Errorf("first_turn may only be issued from a pristine unassigned INIT")
		}
		if next.Assignment == nil || *next.FirstTurn != *next.Assignment {
			return fmt.Errorf("first_turn must be the same Ref as the issued assignment")
		}
		if next.FirstTurn.IssuedRevision != next.Revision {
			return fmt.Errorf("first_turn must be issued at the resulting revision")
		}
		if next.StartedUnix == 0 || next.DeadlineUnix == 0 {
			return fmt.Errorf("first_turn issuance must set the run clock")
		}
	}
	// A newly-set or replaced ref must bind to the resulting revision.
	if err := refBindsToRevision("assignment", old.Assignment, next.Assignment, next.Revision); err != nil {
		return err
	}
	if err := validateEvidenceTransition(old, next); err != nil {
		return err
	}
	if err := refBindsToRevision("gate", old.Gate, next.Gate, next.Revision); err != nil {
		return err
	}
	// A newly-set or changed projection must bind to the resulting revision.
	if err := projBindsToRevision("recovery", old.Recovery, next.Recovery, next.Revision); err != nil {
		return err
	}
	if err := projBindsToRevision("failure", old.Failure, next.Failure, next.Revision); err != nil {
		return err
	}
	if err := validateV5Transition(old, next); err != nil {
		return err
	}
	return nil
}

// validateEvidenceTransition holds the binding to the assignment's own lifetime: a new or changed
// binding must be bound to the RESULTING revision, exactly as a newly-issued ref is.
//
// Together with the steady-state invariant (which ties a present binding's revision to the
// assignment's), this is what makes the packet unswappable underneath a live turn. Re-pointing a
// live turn at different review material would need a changed binding on an unchanged assignment —
// but an unchanged assignment still carries its original issued revision, so the rebound packet
// would have to carry that same older revision, and this rule refuses it. There is deliberately no
// separate "changed without reissuing" check: it could never fire.
func validateEvidenceTransition(old, next *RunState) error {
	if next.Evidence == nil {
		return nil
	}
	if old.Evidence != nil && *old.Evidence == *next.Evidence {
		return nil // unchanged
	}
	if next.Evidence.IssuedRevision != next.Revision {
		return fmt.Errorf("evidence binding was set/changed but bound to revision %d, not the resulting %d", next.Evidence.IssuedRevision, next.Revision)
	}
	return nil
}

func projBindsToRevision(field string, old, next *Projection, rev uint64) error {
	if next == nil {
		return nil
	}
	if old != nil && *old == *next {
		return nil // unchanged
	}
	if next.AtRevision != rev {
		return fmt.Errorf("%s was set/changed but bound to revision %d, not the resulting %d", field, next.AtRevision, rev)
	}
	return nil
}

func writeOnce(field string, old, next int64) error {
	if old != 0 && next != old {
		return fmt.Errorf("%s is write-once", field)
	}
	return nil
}

func refBindsToRevision(field string, old, next *Ref, rev uint64) error {
	if next == nil {
		return nil // cleared or never set
	}
	if old != nil && *old == *next {
		return nil // unchanged
	}
	if next.IssuedRevision != rev {
		return fmt.Errorf("%s was (re)issued but bound to revision %d, not the resulting %d", field, next.IssuedRevision, rev)
	}
	return nil
}

func validateSnapshot(field string, sr SnapshotRef) error {
	if !isLocalRelPath(sr.RelPath) {
		return fmt.Errorf("%s.rel_path %q is not a canonical local path", field, sr.RelPath)
	}
	if !isHex64(sr.Digest) {
		return fmt.Errorf("%s.digest is not a 64-char lower-hex sha256", field)
	}
	return nil
}

func validateCounters(c Counters) error {
	if c.PlanRevisions < 0 || c.TestFixes < 0 || c.VerifyFixes < 0 {
		return fmt.Errorf("counters must be non-negative")
	}
	for i, v := range c.StepFixes {
		if v < 0 {
			return fmt.Errorf("step fix %d is negative", i)
		}
	}
	return v5CounterCeiling(c)
}

func validateAcceptedTurns(rs *RunState) error {
	for k, v := range rs.AcceptedTurns {
		if !validID(k) {
			return fmt.Errorf("accepted turn key %q is not a canonical id", k)
		}
		if v.Receipt.TurnID != k {
			return fmt.Errorf("accepted turn key %q != receipt turn_id %q", k, v.Receipt.TurnID)
		}
		if v.ArtifactDigest != v.Receipt.ArtifactDigest {
			return fmt.Errorf("accepted turn %q artifact digests disagree", k)
		}
		if !isHex64(v.ArtifactDigest) {
			return fmt.Errorf("accepted turn %q digest is not a 64-char lower-hex sha256", k)
		}
		if v.Receipt.Revision == 0 || v.Receipt.Revision > rs.Revision {
			return fmt.Errorf("accepted turn %q receipt revision %d out of range (1..%d)", k, v.Receipt.Revision, rs.Revision)
		}
		if !knownPhases[v.Phase] {
			return fmt.Errorf("accepted turn %q has an unknown phase %q", k, v.Phase)
		}
		if err := validateGitCommitEvidence(k, v); err != nil {
			return err
		}
	}
	return validateGitCommitChain(rs)
}

// gitEvidencePhases are the phases whose acceptance carries a git-commit snapshot (schema v6).
var gitEvidencePhases = map[Phase]bool{PhaseImplementStep: true, PhaseFix: true}

// validateGitCommitEvidence enforces the v6 presence rule: an IMPLEMENT_STEP/FIX acceptance MUST
// carry a well-formed git-commit tuple; every other acceptance MUST NOT.
func validateGitCommitEvidence(k string, v AcceptedTurn) error {
	if !gitEvidencePhases[v.Phase] {
		if v.GitCommit != nil {
			return fmt.Errorf("accepted turn %q (phase %q) must not carry git-commit evidence", k, v.Phase)
		}
		return nil
	}
	gc := v.GitCommit
	if gc == nil {
		return fmt.Errorf("accepted turn %q (phase %q) requires git-commit evidence", k, v.Phase)
	}
	for _, oid := range []string{gc.Parent, gc.Tree, gc.Commit} {
		if !isGitOID(oid) {
			return fmt.Errorf("accepted turn %q git-commit evidence has a malformed OID", k)
		}
	}
	if len(gc.Parent) != len(gc.Tree) || len(gc.Tree) != len(gc.Commit) {
		return fmt.Errorf("accepted turn %q git-commit OIDs mix hash widths", k)
	}
	return nil
}

// validateGitCommitChain proves the run's git acceptances form a parent chain in receipt-revision
// order: the first has parent BaseCommit, each later has parent equal to the preceding acceptance's
// commit, and all OIDs share BaseCommit's hash width.
func validateGitCommitChain(rs *RunState) error {
	var chain []AcceptedTurn
	for _, e := range Ledger(*rs) { // receipt-revision order
		at := rs.AcceptedTurns[e.TurnID]
		if at.GitCommit != nil {
			chain = append(chain, at)
		}
	}
	prev := rs.BaseCommit
	for i, at := range chain {
		gc := at.GitCommit
		if i == 0 && len(gc.Parent) != len(rs.BaseCommit) {
			return fmt.Errorf("git acceptance %q OID width disagrees with base_commit", at.Receipt.TurnID)
		}
		if gc.Parent != prev {
			return fmt.Errorf("git acceptance %q parent %s breaks the commit chain (want %s)", at.Receipt.TurnID, gc.Parent, prev)
		}
		prev = gc.Commit
	}
	return nil
}

func validateRef(field string, r *Ref, rev uint64) error {
	if r == nil {
		return nil
	}
	if !validID(r.ID) {
		return fmt.Errorf("%s id %q is not a canonical id", field, r.ID)
	}
	if r.IssuedRevision == 0 || r.IssuedRevision > rev {
		return fmt.Errorf("%s issued_revision %d out of range (1..%d)", field, r.IssuedRevision, rev)
	}
	return nil
}

func validateProjection(field string, p *Projection, rev uint64) error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(p.Code) == "" || len(p.Code) > 128 {
		return fmt.Errorf("%s code is required and bounded", field)
	}
	if strings.TrimSpace(p.Reason) == "" || len(p.Reason) > 2048 {
		return fmt.Errorf("%s reason is required and bounded", field)
	}
	if strings.TrimSpace(p.NextAction) == "" || len(p.NextAction) > 512 {
		return fmt.Errorf("%s next_action is required and bounded", field)
	}
	if p.AtRevision == 0 || p.AtRevision > rev {
		return fmt.Errorf("%s at_revision %d out of range (1..%d)", field, p.AtRevision, rev)
	}
	return nil
}

// redactAndGuard redacts documented free-text fields and rejects a secret in any
// executable/control field (never silently rewriting identity/control values).
func redactAndGuard(rs *RunState) error {
	rs.FS.Reason = redact.Text(rs.FS.Reason)
	if rs.Recovery != nil {
		rs.Recovery.Reason = redact.Text(rs.Recovery.Reason)
	}
	if rs.Failure != nil {
		rs.Failure.Reason = redact.Text(rs.Failure.Reason)
	}

	control := map[string]string{
		"run_id":                       rs.RunID,
		"base":                         rs.Base,
		"base_commit":                  rs.BaseCommit,
		"worktree_rel_path":            rs.WorktreeRelPath,
		"run_branch":                   rs.RunBranch,
		"phase":                        string(rs.Phase),
		"lifecycle":                    string(rs.Lifecycle),
		"task_snapshot.rel_path":       rs.TaskSnapshot.RelPath,
		"task_snapshot.digest":         rs.TaskSnapshot.Digest,
		"policy_snapshot.rel_path":     rs.PolicySnapshot.RelPath,
		"policy_snapshot.digest":       rs.PolicySnapshot.Digest,
		"fs.class":                     rs.FS.Class,
		"effective_policy.base_branch": rs.EffectivePolicy.BaseBranch,
		"pending_txn_id":               rs.PendingTxnID,
	}
	// EVERY executable element, not just the first. The gate used to be one command string and this map
	// held that one value; run-policy v2 made it a vector plus an environment, and a guard that still
	// checked a single field would have left the arguments and the environment unguarded — which is
	// where a credential is most likely to be written in the first place.
	//
	// Environment values are guarded here rather than redacted anywhere, and that is the point: a
	// redacted pair collapses two distinct secrets to one marker, so a digest over redacted values would
	// bind something the command never received. Refusing is what keeps the digest over ACTUAL values
	// sound.
	for i, a := range rs.EffectivePolicy.TestGate.Argv {
		control[fmt.Sprintf("effective_policy.test_gate.argv.%d", i)] = a
	}
	for i, n := range rs.EffectivePolicy.TestGate.Env.Inherit {
		control[fmt.Sprintf("effective_policy.test_gate.env.inherit.%d", i)] = n
	}
	// Fed as `name=value`, NOT as two separate entries. The detector's rules are schema-sensitive: a
	// bare value like `ordinary-value` matches nothing on its own, and it is the PAIR that carries the
	// signal. Splitting them — which an earlier version did — silently disabled the very rule that
	// makes an environment assignment recognizable as a credential.
	for i, e := range rs.EffectivePolicy.TestGate.Env.Set {
		control[fmt.Sprintf("effective_policy.test_gate.env.set.%d", i)] = e.Name + "=" + e.Value
	}
	// Defence in depth. Resolution refuses a credential-shaped pair BEFORE the intent is journaled,
	// which is the boundary that matters; this catches a state that was written by some other path.
	for i, e := range rs.ResolvedExecution.Env {
		control[fmt.Sprintf("resolved_execution.env.%d", i)] = string(e.Name) + "=" + string(e.Value)
	}
	if rs.Assignment != nil {
		control["assignment.id"] = rs.Assignment.ID
	}
	if rs.FirstTurn != nil {
		control["first_turn.id"] = rs.FirstTurn.ID
	}
	if rs.Gate != nil {
		control["gate.id"] = rs.Gate.ID
	}
	if rs.CandidatePlan != nil {
		addEventRefControls(control, "candidate_plan.source", rs.CandidatePlan.Source)
		control["candidate_plan.digest"] = rs.CandidatePlan.Digest
	}
	if rs.CandidateChecks != nil {
		control["candidate_checks.digest"] = rs.CandidateChecks.Digest
		for i, k := range rs.CandidateChecks.Keys {
			control[fmt.Sprintf("candidate_checks.keys.%d", i)] = k
		}
	}
	if rs.PendingFindings != nil {
		addEventRefControls(control, "pending_findings.source", rs.PendingFindings.Source)
		for i, k := range rs.PendingFindings.Keys {
			control[fmt.Sprintf("pending_findings.keys.%d", i)] = k
		}
	}
	if rs.AgreedPlan != nil {
		addEventRefControls(control, "agreed_plan.plan.source", rs.AgreedPlan.Plan.Source)
		control["agreed_plan.plan.digest"] = rs.AgreedPlan.Plan.Digest
		addEventRefControls(control, "agreed_plan.critique", rs.AgreedPlan.Critique)
		control["agreed_plan.checks.digest"] = rs.AgreedPlan.Checks.Digest
		for i, k := range rs.AgreedPlan.Checks.Keys {
			control[fmt.Sprintf("agreed_plan.checks.keys.%d", i)] = k
		}
	}
	if rs.FixReturn != "" {
		control["fix_return"] = string(rs.FixReturn)
	}
	if rs.Pause != nil {
		addEventRefControls(control, "pause.source", rs.Pause.Source)
		control["pause.kind"] = string(rs.Pause.Kind)
		control["pause.origin_phase"] = string(rs.Pause.OriginPhase)
		control["pause.resume_phase"] = string(rs.Pause.ResumePhase)
		if rs.Pause.FixReturn != "" {
			control["pause.fix_return"] = string(rs.Pause.FixReturn)
		}
		if rs.Pause.Budget != nil {
			control["pause.budget.kind"] = string(rs.Pause.Budget.Kind)
		}
	}
	if rs.Recovery != nil {
		control["recovery.code"] = rs.Recovery.Code
		control["recovery.next_action"] = rs.Recovery.NextAction
	}
	if rs.Failure != nil {
		control["failure.code"] = rs.Failure.Code
		control["failure.next_action"] = rs.Failure.NextAction
	}
	for k, v := range rs.AcceptedTurns {
		control["accepted_turns.key."+k] = k
		control["accepted_turns."+k+".turn_id"] = v.Receipt.TurnID
		control["accepted_turns."+k+".digest"] = v.ArtifactDigest
		if v.GitCommit != nil {
			control["accepted_turns."+k+".git_commit.parent"] = v.GitCommit.Parent
			control["accepted_turns."+k+".git_commit.tree"] = v.GitCommit.Tree
			control["accepted_turns."+k+".git_commit.commit"] = v.GitCommit.Commit
		}
	}
	for field, v := range control {
		if redact.Text(v) != v {
			return fmt.Errorf("state: a secret was detected in control field %s; use environment-based credentials, not run state", field)
		}
	}
	return nil
}

func validateFS(rs *RunState) error {
	if !knownFSClasses[rs.FS.Class] {
		return fmt.Errorf("unknown fs class %q", rs.FS.Class)
	}
	if strings.TrimSpace(rs.FS.Reason) == "" {
		return fmt.Errorf("fs.reason is required")
	}
	switch rs.FS.Class {
	case "supported-local":
		if rs.FS.Acknowledged {
			return fmt.Errorf("supported-local filesystem must not be acknowledged (ack is only for the unknown override)")
		}
	case "unknown":
		if rs.EffectivePolicy.UnknownFSPolicy != "acknowledge" || !rs.FS.Acknowledged {
			return fmt.Errorf("unknown filesystem requires unknown_fs_policy=acknowledge and fs.acknowledged=true")
		}
	case "known-unsupported":
		return fmt.Errorf("a run must not operate on a known-unsupported filesystem")
	}
	return nil
}

func validateTimes(rs *RunState) error {
	// Before start: both unset.
	if rs.StartedUnix == 0 && rs.DeadlineUnix == 0 {
		return nil
	}
	if rs.StartedUnix <= 0 || rs.DeadlineUnix <= 0 {
		return fmt.Errorf("started_unix/deadline_unix must be both unset or both positive")
	}
	if rs.StartedUnix < rs.CreatedUnix {
		return fmt.Errorf("started_unix %d is before created_unix %d", rs.StartedUnix, rs.CreatedUnix)
	}
	// The deadline is the frozen run wall cap applied to the start (overflow-safe
	// via subtraction, since both are positive): an arbitrary deadline must not
	// silently replace the cap.
	if rs.DeadlineUnix <= rs.StartedUnix {
		return fmt.Errorf("deadline_unix must be after started_unix")
	}
	if rs.DeadlineUnix-rs.StartedUnix != rs.EffectivePolicy.Limits.MaxWallSeconds {
		return fmt.Errorf("deadline_unix - started_unix (%d) must equal the frozen max_wall_seconds (%d)",
			rs.DeadlineUnix-rs.StartedUnix, rs.EffectivePolicy.Limits.MaxWallSeconds)
	}
	return nil
}

func isGitOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validID is the canonical bounded identity grammar for turn/assignment/gate/txn
// ids. validRunID already enforces the same filename-safe rules.
func validID(s string) bool { return validRunID(s) }

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// isLocalRelPath enforces a single canonical representation for stored locators:
// a clean, forward-slash, relative path with no backslashes and no traversal.
// This avoids separator- and case-aliases that could point two allocations at one
// directory (see locatorKey for uniqueness comparison).
func isLocalRelPath(p string) bool {
	if p == "" || p == "." || p == ".." {
		return false
	}
	// Reject anything that is not a plain forward-slash relative path.
	if strings.ContainsRune(p, '\\') || strings.ContainsRune(p, ':') || strings.ContainsRune(p, 0) {
		return false // backslash separators, drive/ADS colons, NUL
	}
	if path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") {
		return false
	}
	// Filesystem boundary: on the running platform the concrete path must be
	// local (rejects Windows volume-qualified/UNC forms and reserved devices such
	// as NUL/CON that path.Clean cannot see). These locators are coordinator-
	// generated, so a conservative rule is safe.
	return filepath.IsLocal(filepath.FromSlash(p))
}

// locatorKey normalizes a locator for uniqueness comparison. It folds case so a
// case-insensitive filesystem (Windows) cannot alias two allocations to one
// directory; on a case-sensitive filesystem this is merely conservative.
func locatorKey(p string) string { return strings.ToLower(p) }

// validRunID is the filename-safe identifier grammar. Run ids become run
// directory names, refs, and branch segments, so it requires an alphanumeric
// start and rejects the reserved dot names.
func validRunID(s string) bool {
	if len(s) == 0 || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	if c := s[0]; !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
