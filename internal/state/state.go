// Package state is the coordinator's typed, validated run state, persisted as
// immutable generations via internal/genstore. State never overwrites in
// place; each mutation is a compare-and-swap that appends the next generation.
//
// The mutator runs INSIDE the generation builder, after genstore has chosen the
// next (possibly gap-skipped) generation number, so identities and receipts bind
// to the exact resulting revision. Every mutation is validated for
// its own invariants and as a transition from the previous state (bootstrap
// fields immutable, counters non-decreasing, accepted turns append-only). Free
// text is redacted before persisting; a secret in an executable/control field is
// rejected (never silently rewritten). The returned value is decoded from the
// exact persisted bytes, so it never diverges from durable state.
//
// State holds only typed identities, digests, and relative references — never the
// protocol artifacts themselves (those arrive with the transport layer).
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/redact"
)

// RunStateVersion is the on-disk schema version; an unknown version fails closed.
// v2 added the authoritative accepted Phase to each accepted turn (v1 recorded
// only the artifact digest and receipt). v3 added the frozen workspace identity
// (worktree relative locator + run branch) so pull/git work never has to mine a
// completed bootstrap journal for it. v4 added the write-once FirstTurn issuance
// record — append-only assignment history that durably proves which first turn
// was issued at INIT->PLAN_DRAFT even after the mutable Assignment has moved on.
// v5 added the phase-engine working set: the plan-negotiation staging
// (CandidatePlan/CandidateChecks/PendingFindings), the frozen immutable AgreedPlan,
// the implementation cursor (StepIndex) and per-phase FIX/VERIFY context, and the
// durable human-gate resume record (Pause) unifying human-decision and
// quality-budget gates. v6 added the immutable git-commit evidence tuple on each
// IMPLEMENT_STEP/FIX accepted turn (GitCommit) — the parent/tree/commit the git
// transaction produced — with a receipt-order commit-chain invariant. It is a
// semantic format change: an older v5 generation is rejected outright (no implicit
// migration; a missing IMPLEMENT/FIX tuple is never treated as valid). An older
// generation is missing a required field, so it fails with version remediation,
// not a vague error (see checkSchemaVersion). v7 added the review-evidence binding
// (Evidence) that makes a read-only turn actionable: an assignment for a phase the
// lead cannot edit in EXISTS if and only if a hash-bound evidence packet was
// published for it. That is a global invariant, not an optional field, so v7 landed
// atomically with every issuance authority — no generation can exist in a
// half-upgraded shape where some read-only assignments carry a binding and others
// do not.
//
// v8 added the mechanical test gate's attempt identity — the at-most-one
// ActiveTestAttempt and the append-only TestAttempts ledger — together with
// ResolvedExecution, the environment the gate runs with, frozen at first attach.
// The three land in one version because each is a required authority the others
// assume: an attempt with no frozen environment could execute differently on
// recovery than it was authorized to. Before it, RunState
// could say a run was in TESTS but not WHICH run of the tests, so a runner
// finishing after a crash had nothing to prove it was finalizing its own attempt
// and an outcome could not be tied to the tree it was a statement about. It lands
// atomically with run-policy v2 for a mechanical reason as well as a conceptual
// one: state embeds the frozen policy and decode calls the CURRENT validator, so a
// policy-only bump would make every existing generation undecodable while the
// state schema still claimed to be the old one.
const RunStateVersion = 9

// stateRetention{Keep,Trigger} configure the state stores' pre-append hysteresis compaction
// (genstore.WithRetention): each generation is a full self-sufficient snapshot, so recovery
// needs only the head, and a bounded tail is kept for a torn-head fallback (up to keep-1
// generations back). Compaction is storage maintenance, not an operator protocol (and
// catalog/current-run are repository-scoped), so it is an internal constant, not a
// config.RunPolicy field. Applies to RunState, Registry, Catalog, and CurrentRun stores.
const (
	stateRetentionKeep    = 8
	stateRetentionTrigger = 16
)

// ErrRevisionConflict is returned when a mutation's expected revision does not
// match the current head.
var ErrRevisionConflict = errors.New("state: revision conflict")

// ErrUnsupportedSchema means a persisted generation carries a schema version this
// build does not support (it was written by a different claudex version).
var ErrUnsupportedSchema = errors.New("state: unsupported on-disk schema version")

// Lifecycle is the durable macro lifecycle of a run.
type Lifecycle string

const (
	LifecycleRunning         Lifecycle = "running"
	LifecyclePaused          Lifecycle = "paused"
	LifecyclePausedBudget    Lifecycle = "paused_budget"
	LifecycleRateLimited     Lifecycle = "rate_limited"
	LifecycleCancelled       Lifecycle = "cancelled"
	LifecycleFailedRetryable Lifecycle = "failed_retryable"
	LifecycleFailedTerminal  Lifecycle = "failed_terminal"
	LifecycleCompleted       Lifecycle = "completed"
)

// Phase is the current phase of the pairing loop. State owns the known values;
// the phase engine owns the allowed transitions between them.
type Phase string

const (
	PhaseInit          Phase = "INIT"
	PhasePlanDraft     Phase = "PLAN_DRAFT"
	PhasePlanCritique  Phase = "PLAN_CRITIQUE"
	PhasePlanRevise    Phase = "PLAN_REVISE"
	PhaseImplementStep Phase = "IMPLEMENT_STEP"
	PhaseCheckpoint    Phase = "CHECKPOINT"
	PhaseFix           Phase = "FIX"
	PhaseTests         Phase = "TESTS"
	PhaseVerify        Phase = "VERIFY"
	PhaseAwaitGuidance Phase = "AWAIT_GUIDANCE"
	PhaseDone          Phase = "DONE"
)

// repoEditPhases are the phases whose assignment carries a MUTABLE repository worktree. State owns
// this set because the evidence invariant is a state invariant: a turn either edits the repository
// or reviews a hash-bound evidence packet, never both and never neither. transport's role-aware
// EditableTurn is defined in terms of this same set, so the pull-time workspace decision and the
// persisted invariant cannot drift apart.
var repoEditPhases = map[Phase]bool{
	PhaseImplementStep: true,
	PhaseFix:           true,
}

// RepoEditPhase reports whether an assignment issued in this phase carries a mutable worktree
// (and therefore no evidence binding) rather than a read-only evidence packet.
func RepoEditPhase(p Phase) bool { return repoEditPhases[p] }

// SnapshotRef points at a hashed, copied input inside the run directory (relative
// local path + exact digest of the bytes), never the live source.
type SnapshotRef struct {
	RelPath string `json:"rel_path"`
	Digest  string `json:"digest"`
}

// FSResult is the frozen filesystem classification and any operator ack.
type FSResult struct {
	Class        string `json:"class"`
	Reason       string `json:"reason"` // free text, redactable
	Acknowledged bool   `json:"acknowledged"`
}

// Counters are lead-response budgets consumed so far. Non-negative, non-decreasing.
type Counters struct {
	PlanRevisions int   `json:"plan_revisions"`
	TestFixes     int   `json:"test_fixes"`
	VerifyFixes   int   `json:"verify_fixes"`
	StepFixes     []int `json:"step_fixes"`
}

// Ref binds an issued identity to the revision that issued it.
type Ref struct {
	ID             string `json:"id"`
	IssuedRevision uint64 `json:"issued_revision"`
}

// AssignmentEvidence binds the live assignment to the immutable review-evidence packet that makes
// its turn actionable. State owns this type: the packet manifest binds run/turn/phase/source, and
// this wrapper supplies the one fact the off-lock producer cannot know — the exact revision the
// assignment was issued at, which is only decided at the state CAS.
//
// It exists if and only if a read-only actionable assignment exists (see validateEvidenceBinding).
// An IMPLEMENT_STEP/FIX assignment has a mutable worktree instead and carries none.
type AssignmentEvidence struct {
	TurnID          string `json:"turn_id"`
	IssuedRevision  uint64 `json:"issued_revision"`
	ManifestRelPath string `json:"manifest_rel_path"`
	RootDigest      string `json:"root_digest"`
}

// Receipt is the durable acknowledgement of an accepted submit.
type Receipt struct {
	TurnID         string `json:"turn_id"`
	Revision       uint64 `json:"revision"`
	ArtifactDigest string `json:"artifact_digest"`
}

// AcceptedTurn records what was accepted for a turn (keyed by turn_id in the map).
// Phase is the single coordinator-authored acceptance fact: the phase the turn
// was in. Role and artifact message type are derived from it via the turn spec,
// so no redundant, disagreeing facts are persisted. GitCommit is the immutable
// git-commit evidence of the accepted implementation snapshot; it is REQUIRED
// exactly when Phase is IMPLEMENT_STEP or FIX and forbidden otherwise (schema v6).
// The field stays a pointer so a nil is the natural "no evidence" — the immutable
// append-only check compares turns by value (reflect.DeepEqual).
type AcceptedTurn struct {
	ArtifactDigest string             `json:"artifact_digest"`
	Receipt        Receipt            `json:"receipt"`
	Phase          Phase              `json:"phase"`
	GitCommit      *GitCommitEvidence `json:"git_commit,omitempty"`
}

// GitCommitEvidence is the immutable (parent, tree, commit) the git transaction froze and applied
// for an IMPLEMENT_STEP/FIX acceptance. All three are lower-hex OIDs of one consistent hash width;
// the run's git acceptances form a chain in receipt-revision order (first parent == BaseCommit,
// each later parent == the preceding git acceptance's commit).
type GitCommitEvidence struct {
	Parent string `json:"parent"`
	Tree   string `json:"tree"`
	Commit string `json:"commit"`
}

// Projection is a typed recovery or failure summary.
type Projection struct {
	Code       string `json:"code"`
	Reason     string `json:"reason"` // free text, redactable
	NextAction string `json:"next_action"`
	AtRevision uint64 `json:"at_revision"`
}

// EventRef is the provenance of one accepted artifact (an agent turn) or a
// coordinator test result. TurnID is a non-empty accepted agent turn id, or empty
// for the single coordinator TESTS source. EventRef carries no redundant kind: the
// authoritative artifact type is AcceptedTurns[TurnID].Phase, and the containing
// field supplies the expected source phase. Digest is the raw accepted canonical
// artifact digest (agent) or the coordinator test-result digest (TESTS).
type EventRef struct {
	Digest string `json:"digest"`
	TurnID string `json:"turn_id"`
}

// PlanRef points at one proposed plan. Digest is the MATERIALIZED canonical
// plan-document digest — distinct from Source.Digest, which is the accepted
// plan/plan_revision submit artifact (an envelope, and for a revision a patch).
// StepCount is the frozen step count (1..protocol.MaxPlanSteps) that bounds the
// implementation cursor and per-step fix vector without holding the steps.
type PlanRef struct {
	Source    EventRef `json:"source"`
	Digest    string   `json:"digest"`
	StepCount int      `json:"step_count"`
}

// CheckSetRef is the materialized implementation-check obligation set: strictly
// sorted canonical keys plus the digest of the canonical full materialized check
// array. An empty key set carries the canonical digest of the empty array.
type CheckSetRef struct {
	Keys   []string `json:"keys"`
	Digest string   `json:"digest"`
}

// FindingObligations are the actionable critique findings a plan revision must
// answer: the raising plan_critique event plus a non-empty, strictly sorted key set.
type FindingObligations struct {
	Source EventRef `json:"source"`
	Keys   []string `json:"keys"`
}

// PlanAgreement is the frozen, immutable outcome of plan negotiation. Once set it
// never changes; it carries the agreed plan, the agreeing critique, the frozen
// implementation-check obligations, and the promotion revision.
type PlanAgreement struct {
	Plan           PlanRef     `json:"plan"`
	Critique       EventRef    `json:"critique"`
	Checks         CheckSetRef `json:"checks"`
	AgreedRevision uint64      `json:"agreed_revision"`
}

// VerifyRequirement owns the ownerless-VERIFY threshold: the pair generation that
// must be reached before a verification turn issues (VERIFY enters ownerless, with
// no assignment, until the reviewer is replaced). The attempt is DERIVED as
// Counters.VerifyFixes+1 (range 1..config.MaxBudget+1) and never stored.
type VerifyRequirement struct {
	RequiredGeneration uint64 `json:"required_generation"`
}

// PauseKind discriminates the two durable human-gated pauses. Both use
// AWAIT_GUIDANCE + LifecyclePaused + Gate; neither is the usage-window pause
// (LifecyclePausedBudget), which carries no gate.
type PauseKind string

const (
	PauseHumanDecision PauseKind = "human_decision"
	PauseQualityBudget PauseKind = "quality_budget"
)

// BudgetKind names the exhausted quality budget of a quality_budget pause.
type BudgetKind string

const (
	BudgetPlan       BudgetKind = "plan"
	BudgetCheckpoint BudgetKind = "checkpoint"
	BudgetTest       BudgetKind = "test"
	BudgetVerify     BudgetKind = "verify"
)

// BudgetPause is the quality-budget detail of a PauseContext.
type BudgetPause struct {
	Kind BudgetKind `json:"kind"`
}

// PauseContext is the durable resume record for a human-gated pause. It records
// where the run paused out of and resumes into, the accepted (or coordinator)
// event that raised the gate, and — moved in from the live state for the duration
// of the pause — the FIX return target and/or VERIFY requirement that must be
// restored on resume.
type PauseContext struct {
	Kind        PauseKind          `json:"kind"`
	OriginPhase Phase              `json:"origin_phase"`
	ResumePhase Phase              `json:"resume_phase"`
	FixReturn   Phase              `json:"fix_return,omitempty"`
	Source      EventRef           `json:"source"`
	Budget      *BudgetPause       `json:"budget,omitempty"`
	Verify      *VerifyRequirement `json:"verify,omitempty"`
}

// RunState is the authoritative typed state of a run. Absent refs/projections are
// nil (distinct from a present all-zero record).
type RunState struct {
	SchemaVersion   int              `json:"schema_version"`
	RunID           string           `json:"run_id"`
	Revision        uint64           `json:"revision"`
	Lifecycle       Lifecycle        `json:"lifecycle"`
	Phase           Phase            `json:"phase"`
	CreatedUnix     int64            `json:"created_unix"`
	StartedUnix     int64            `json:"started_unix"`
	DeadlineUnix    int64            `json:"deadline_unix"`
	TaskSnapshot    SnapshotRef      `json:"task_snapshot"`
	PolicySnapshot  SnapshotRef      `json:"policy_snapshot"`
	EffectivePolicy config.RunPolicy `json:"effective_policy"`
	// ResolvedExecution is the environment the mechanical test gate runs with, frozen at first attach
	// and bound here from the completed bootstrap intent. Attempts AND recovery read it from STATE and
	// never re-derive it, so a recovered attempt cannot silently execute in a different environment
	// than the one that was authorized.
	ResolvedExecution config.ResolvedExecution `json:"resolved_execution"`
	FS                FSResult                 `json:"fs"`
	Base              string                   `json:"base"`
	BaseCommit        string                   `json:"base_commit"`
	WorktreeRelPath   string                   `json:"worktree_rel_path"`
	RunBranch         string                   `json:"run_branch"`
	Counters          Counters                 `json:"counters"`
	Assignment        *Ref                     `json:"assignment,omitempty"`
	Evidence          *AssignmentEvidence      `json:"evidence,omitempty"`
	FirstTurn         *Ref                     `json:"first_turn,omitempty"`
	Gate              *Ref                     `json:"gate,omitempty"`
	CandidatePlan     *PlanRef                 `json:"candidate_plan,omitempty"`
	CandidateChecks   *CheckSetRef             `json:"candidate_checks,omitempty"`
	PendingFindings   *FindingObligations      `json:"pending_findings,omitempty"`
	AgreedPlan        *PlanAgreement           `json:"agreed_plan,omitempty"`
	StepIndex         *int                     `json:"step_index,omitempty"`
	FixReturn         Phase                    `json:"fix_return,omitempty"`
	Verify            *VerifyRequirement       `json:"verify,omitempty"`
	Pause             *PauseContext            `json:"pause,omitempty"`
	AcceptedTurns     map[string]AcceptedTurn  `json:"accepted_turns"`
	PendingTxnID      string                   `json:"pending_txn_id"`
	Recovery          *Projection              `json:"recovery,omitempty"`
	Failure           *Projection              `json:"failure,omitempty"`
	// ActiveTestAttempt is the at-most-one mechanical test attempt this run currently owns.
	//
	// Without it RunState has no attempt identity whatsoever: engine.Apply records no TESTS source, so
	// "which run of the tests produced this outcome" was a question the durable state could not answer,
	// and a runner finishing after a crash had nothing to prove it was finalizing its own attempt.
	ActiveTestAttempt *TestAttemptRef `json:"active_test_attempt,omitempty"`
	// TestAttempts is the APPEND-ONLY ledger of finalized attempts.
	//
	// Append-only because an attempt that was superseded is still a thing that ran: an indeterminate
	// attempt retried three times is a different situation from one clean pass, and overwriting would
	// erase exactly the evidence that distinguishes a broken environment from failing code.
	TestAttempts []FinalizedAttempt `json:"test_attempts,omitempty"`
}

// TestAttemptRef binds the one in-flight mechanical test attempt.
//
// It is written BEFORE the command is spawned and read by whoever finalizes the attempt, which may be a
// different process than the one that started it. Every field exists so a finalization can be checked
// against the attempt it claims to be finalizing rather than trusted.
type TestAttemptRef struct {
	// AttemptID is the identity the containment, the intent and the result all bind.
	AttemptID string `json:"attempt_id"`
	// StartRevision is the state revision at which this attempt became active.
	StartRevision uint64 `json:"start_revision"`
	// TestedCommit and TestedTree are the repository identity the attempt is a statement about. An
	// outcome without them would be a claim about "the code" with no way to say which code.
	TestedCommit string `json:"tested_commit"`
	TestedTree   string `json:"tested_tree"`
	// IntentDigest binds the exact published intent — argv, resolved executable, environment identity.
	IntentDigest string `json:"intent_digest"`
	// CancelPending records that a cancel bound BEFORE the runner finalized.
	//
	// It exists so a cancelled run can be marked cancelled immediately — so status and wait wake up —
	// without discarding the only identity with which the in-flight attempt can still be finalized.
	// Clearing the ref instead would have left a live runner holding facts nothing could accept.
	CancelPending bool `json:"cancel_pending,omitempty"`
}

// TestExecution is what the RUNNER observed: how the command ended, in the runner's own terms.
//
// It is deliberately independent of TestIdentity below, and deliberately NOT the verdict. The two were
// once a single precedence list, and a precedence list has to be read in order to be understood —
// whereas a pair of independent facts makes the verdict a total function of both, with no case left
// implicit. TestOutcome is that function.
type TestExecution string

const (
	// TestExecutionOK means the command ran to completion and reported success.
	TestExecutionOK TestExecution = "ok"
	// TestExecutionNonzero means it ran to completion and reported failure. A statement about the CODE.
	TestExecutionNonzero TestExecution = "nonzero"
	// TestExecutionTimeout means the policy's deadline expired. Distinct from nonzero because the
	// command never reported anything, though both map to the same verdict.
	TestExecutionTimeout TestExecution = "timeout"
	// TestExecutionCancelled means an operator cancelled the attempt. Nobody's code failed.
	TestExecutionCancelled TestExecution = "cancelled"
	// TestExecutionSpawnFailed means the CONFIGURED COMMAND could not start — a statement about the
	// operator's command, not about the code under test.
	TestExecutionSpawnFailed TestExecution = "spawn_failed"
	// TestExecutionInterrupted means the runner lost the ability to obtain an outcome: the supervisor
	// died, a frame was malformed, the owner disappeared. A statement about the ENVIRONMENT, and the
	// distinction matters because it must not spend the fix budget.
	TestExecutionInterrupted TestExecution = "interrupted"
)

// TestIdentity is whether the repository was the same at the end as at the start.
//
// Independent of TestExecution by construction: the verdict is a total function of the PAIR, so no case
// is left implicit the way an ordered precedence list would leave it.
type TestIdentity string

const (
	// TestIdentityUnchanged means the tested tree was identical at both observations.
	TestIdentityUnchanged TestIdentity = "unchanged"
	// TestIdentityChanged means it was not, so the outcome describes a tree that no longer exists.
	TestIdentityChanged TestIdentity = "changed"
	// TestIdentityUnobserved means identity could not be established at all.
	TestIdentityUnobserved TestIdentity = "unobserved"
)

// AllTestExecutions and AllTestIdentities are THE vocabularies, in production.
//
// They were briefly duplicated as test-only slices, which made the "closed vocabulary" claim depend on
// somebody remembering to edit a copy: adding a production constant did not enlarge the tested cross
// product, so the new combinations were never decided and never noticed. One authority, iterated by the
// tests, is what makes growing either vocabulary fail until its combinations exist.
func AllTestExecutions() []TestExecution {
	return []TestExecution{
		TestExecutionOK, TestExecutionNonzero, TestExecutionTimeout,
		TestExecutionCancelled, TestExecutionSpawnFailed, TestExecutionInterrupted,
	}
}

func AllTestIdentities() []TestIdentity {
	return []TestIdentity{TestIdentityUnchanged, TestIdentityChanged, TestIdentityUnobserved}
}

// KnownTestExecution reports whether e is one of the enumerated observations.
func KnownTestExecution(e TestExecution) bool {
	for _, k := range AllTestExecutions() {
		if k == e {
			return true
		}
	}
	return false
}

// KnownTestIdentity reports whether i is one of the enumerated observations.
func KnownTestIdentity(i TestIdentity) bool {
	for _, k := range AllTestIdentities() {
		if k == i {
			return true
		}
	}
	return false
}

// TestOutcome is the VERDICT the gate acts on, derived from the pair above.
type TestOutcome string

const (
	// OutcomePass advances the run.
	OutcomePass TestOutcome = "pass"
	// OutcomeFail is an ordinary test-failure edge and spends the fix budget.
	OutcomeFail TestOutcome = "fail"
	// OutcomeIndeterminate is retryable and spends NO budget: the code did not fail, the runner could
	// not decide.
	OutcomeIndeterminate TestOutcome = "indeterminate"
	// OutcomeCancelled is ledger-only: no edge, no budget. Nobody's code failed.
	OutcomeCancelled TestOutcome = "cancelled"
)

type observation struct {
	Execution TestExecution
	Identity  TestIdentity
}

// outcomeTable is an EXPLICIT row per combination, with no fallthrough.
//
// It replaced an ordered set of cases ending in a default, and the default was the defect: a newly
// accepted execution omitted from the cases was silently classified as `fail`, and a newly accepted
// identity was guessed as pass or fail. Both are verdicts the gate would have acted on, decided by
// nobody. With a table, an undecided pair has no row and Outcome refuses — so adding a value to either
// vocabulary fails closed until somebody decides what it means.
//
// Several rows are DECISIONS rather than deductions:
//
//   - cancelled outranks everything, including a timeout: operator intent supersedes a bound, and
//     calling a cancelled attempt a failure blames the code for a human's decision;
//   - spawn_failed and interrupted are indeterminate whatever identity says, because neither is a
//     statement about the code and neither may spend the fix budget;
//   - an unobserved tree cannot pass even on a clean exit — that would be the strongest available claim
//     made on the weakest evidence;
//   - a changed tree fails even on a clean exit, because the result describes a tree that no longer
//     exists.
var outcomeTable = map[observation]TestOutcome{
	{TestExecutionOK, TestIdentityUnchanged}:  OutcomePass,
	{TestExecutionOK, TestIdentityChanged}:    OutcomeFail,
	{TestExecutionOK, TestIdentityUnobserved}: OutcomeIndeterminate,

	{TestExecutionNonzero, TestIdentityUnchanged}:  OutcomeFail,
	{TestExecutionNonzero, TestIdentityChanged}:    OutcomeFail,
	{TestExecutionNonzero, TestIdentityUnobserved}: OutcomeIndeterminate,

	{TestExecutionTimeout, TestIdentityUnchanged}:  OutcomeFail,
	{TestExecutionTimeout, TestIdentityChanged}:    OutcomeFail,
	{TestExecutionTimeout, TestIdentityUnobserved}: OutcomeIndeterminate,

	{TestExecutionCancelled, TestIdentityUnchanged}:  OutcomeCancelled,
	{TestExecutionCancelled, TestIdentityChanged}:    OutcomeCancelled,
	{TestExecutionCancelled, TestIdentityUnobserved}: OutcomeCancelled,

	{TestExecutionSpawnFailed, TestIdentityUnchanged}:  OutcomeIndeterminate,
	{TestExecutionSpawnFailed, TestIdentityChanged}:    OutcomeIndeterminate,
	{TestExecutionSpawnFailed, TestIdentityUnobserved}: OutcomeIndeterminate,

	{TestExecutionInterrupted, TestIdentityUnchanged}:  OutcomeIndeterminate,
	{TestExecutionInterrupted, TestIdentityChanged}:    OutcomeIndeterminate,
	{TestExecutionInterrupted, TestIdentityUnobserved}: OutcomeIndeterminate,
}

// Outcome is a TOTAL function of the two observations, by table lookup.
//
// An unknown value on either axis is refused, and so is a KNOWN pair with no decided row: a verdict the
// gate cannot justify is worse than no verdict, because it would be acted on.
func Outcome(e TestExecution, i TestIdentity) (TestOutcome, error) {
	if !KnownTestExecution(e) {
		return "", fmt.Errorf("state: %q is not a known execution observation", e)
	}
	if !KnownTestIdentity(i) {
		return "", fmt.Errorf("state: %q is not a known identity observation", i)
	}
	o, ok := outcomeTable[observation{e, i}]
	if !ok {
		return "", fmt.Errorf("state: no decided outcome for the known pair (%q, %q); the vocabulary grew without the combination being decided", e, i)
	}
	return o, nil
}

// MaxTerminalReasonBytes bounds the ledger's terminal detail, measured on the CANONICAL bytes.
//
// Exported because the runner must apply it where the fact is accepted: enforcing it only at the state
// CAS would let an oversized record become durable first and be refused afterwards, when it can no
// longer be un-written.
const MaxTerminalReasonBytes = 256

// CanonicalTerminalReason is the ONE representation of a runner's terminal detail.
//
// It exists so the same bytes are hashed into the published result and bound into the ledger. The
// persistence boundary used to redact this field itself, which silently severed the entry's
// ResultDigest from the text it identifies — the digest covered what was published, and what was stored
// was something else. Callers apply this before hashing; the state boundary then REFUSES anything that
// is not already canonical rather than quietly fixing it.
//
// Redaction rather than rejection is deliberate, and asymmetric with the environment rule on purpose:
// an environment value must be refused because a digest over redacted values would bind something the
// command never received, whereas this field is DESCRIPTIVE — nothing consumes it as an execution input
// — and refusing would let hostile command output strand the gate. It is idempotent, which is what
// makes applying it before hashing and checking it afterwards agree.
func CanonicalTerminalReason(s string) string { return redact.Text(s) }

// FinalizedAttempt is one immutable ledger entry.
type FinalizedAttempt struct {
	AttemptID     string `json:"attempt_id"`
	StartRevision uint64 `json:"start_revision"`
	// BoundRevision is the revision at which this entry was appended.
	BoundRevision uint64 `json:"bound_revision"`
	TestedCommit  string `json:"tested_commit"`
	TestedTree    string `json:"tested_tree"`
	// ResultDigest is the canonical digest of the published result record. It is the ledger's identity
	// for the attempt's evidence: the same digest may not authorize two differing outcomes.
	ResultDigest string `json:"result_digest"`
	// Execution and Identity are the two independent halves of the outcome.
	Execution TestExecution `json:"execution"`
	Identity  TestIdentity  `json:"identity"`
	// TerminalReason is the account of how the command ended (exited, signalled, timed out, cancelled,
	// spawn failure class). It is descriptive; Execution is what decides anything.
	TerminalReason string `json:"terminal_reason"`
	// TerminalAuthor says WHO wrote that account.
	//
	// It exists because the field has two possible authorities and reading it without knowing which
	// applies is reading somebody's prose as somebody else's observation. Ordinarily the runner reports
	// how the command ended. But an attempt interrupted by a crash has no runner terminal at all - the
	// process that would have written one is gone - and recovery must still finalize it, so the account
	// there is authored deterministically by recovery. Storing that in a field declared to be the
	// runner's own word would attribute an inference to an observer that never made it.
	TerminalAuthor TerminalAuthor `json:"terminal_author"`
}

// TerminalAuthor is the closed vocabulary of authorities for a terminal account.
type TerminalAuthor string

const (
	// TerminalByRunner means the process that ran the command reported how it ended. It is the only
	// authority that ever saw the command itself.
	TerminalByRunner TerminalAuthor = "runner"
	// TerminalByCoordinator means the coordinator was ALIVE and observed an infrastructure fault - the
	// supervisor died, its frames were corrupt, a GO could not be delivered. Nobody saw the command
	// end, so nothing can be claimed about it, but this is not a crash either: recovery is not running
	// and there is a live process making the observation.
	//
	// It exists because the first version of this vocabulary had only runner and recovery, which left
	// the live launch-fault rows with no truthful value at all: a dead supervisor cannot author its own
	// obituary, and recovery is not there to author it.
	TerminalByCoordinator TerminalAuthor = "coordinator"
	// TerminalByRecovery means no live process observed anything, because the owner crashed, and the
	// account was authored deterministically while settling the attempt afterwards.
	TerminalByRecovery TerminalAuthor = "recovery"
)

// AllTerminalAuthors is the closed vocabulary.
func AllTerminalAuthors() []TerminalAuthor {
	return []TerminalAuthor{TerminalByRunner, TerminalByCoordinator, TerminalByRecovery}
}

// AuthorityAgreesWithExecution is the ONE rule about who may say what, exported so the record boundary,
// the lifecycle and the ledger all apply it rather than each spelling out its own version.
//
// `interrupted` means nobody obtained an outcome, which is precisely the thing the RUNNER cannot report
// - if it were alive to report, it would have reported the outcome. Every other execution is something
// the runner watched happen, so neither the coordinator nor recovery may claim it: they were not there.
func AuthorityAgreesWithExecution(a TerminalAuthor, e TestExecution) bool {
	if e == TestExecutionInterrupted {
		return a == TerminalByCoordinator || a == TerminalByRecovery
	}
	return a == TerminalByRunner
}

// KnownTerminalAuthor reports whether a is in the vocabulary.
func KnownTerminalAuthor(a TerminalAuthor) bool {
	for _, k := range AllTerminalAuthors() {
		if a == k {
			return true
		}
	}
	return false
}

// Store is the run-state store over a genstore.
type Store struct {
	gs *genstore.Store
}

// Open returns a state store handle (side-effect-free).
func Open(dir, lockPath string) *Store {
	return &Store{gs: genstore.Open(dir, lockPath).WithRetention(stateRetentionKeep, stateRetentionTrigger)}
}

// LockPath is the mutation lock guarding this store.
func (s *Store) LockPath() string { return s.gs.LockPath() }

// Load returns the current run state, strictly decoded and fully validated.
// (_, false, nil) means no state exists yet.
func (s *Store) Load() (RunState, bool, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return RunState{}, false, err
	}
	if !ok {
		return RunState{}, false, nil
	}
	rs, err := decodeRunState(rec)
	if err != nil {
		return RunState{}, false, err
	}
	return rs, true, nil
}

// Mutate applies fn as a compare-and-swap against expectedRevision, appending the
// next generation. It acquires and releases the guard itself; use MutateLocked to
// compose with other stores under one guard.
func (s *Store) Mutate(expectedRevision uint64, fn func(nextRevision uint64, next *RunState) error) (RunState, error) {
	g, ok, err := genstore.Acquire(s.gs.LockPath())
	if err != nil {
		return RunState{}, err
	}
	if !ok {
		return RunState{}, genstore.ErrBusy
	}
	rs, merr := s.MutateLocked(g, expectedRevision, fn)
	if rerr := g.Release(); merr == nil && rerr != nil {
		return rs, &genstore.PostCommitError{Generation: rs.Revision, Err: rerr}
	}
	return rs, merr
}

// MutateLocked appends the next generation under an already-held guard. fn runs
// inside the builder with the resulting revision, so identities/receipts bind to
// the real generation even across a quarantined-slot gap.
func (s *Store) MutateLocked(g *genstore.Guard, expectedRevision uint64, fn func(nextRevision uint64, next *RunState) error) (RunState, error) {
	rec, ok, err := s.gs.Latest()
	if err != nil {
		return RunState{}, err
	}

	var prev *RunState
	var head genstore.Head
	if ok {
		p, derr := decodeRunState(rec)
		if derr != nil {
			return RunState{}, derr
		}
		if p.Revision != expectedRevision {
			return RunState{}, fmt.Errorf("%w: expected %d, have %d", ErrRevisionConflict, expectedRevision, p.Revision)
		}
		prev = &p
		head = rec.Head()
	} else if expectedRevision != 0 {
		return RunState{}, fmt.Errorf("%w: expected %d on an empty store", ErrRevisionConflict, expectedRevision)
	}

	built, err := s.gs.AppendLocked(g, head, func(gen uint64, _ string) ([]byte, error) {
		next := cloneForNext(prev)
		next.Revision = gen
		next.SchemaVersion = RunStateVersion

		if err := fn(gen, next); err != nil {
			return nil, err
		}
		// The callback must not poison the generation identity: a changed revision
		// or schema version would be committed and only caught post-commit by
		// decodeRunState (bricking the head), so reject it before serialization.
		if next.Revision != gen {
			return nil, fmt.Errorf("mutation must not change the revision (want %d)", gen)
		}
		if next.SchemaVersion != RunStateVersion {
			return nil, fmt.Errorf("mutation must not change the schema version")
		}
		// Schema-aware redaction: redact free text, reject secrets in control
		// fields. Then validate the new state and the transition from prev.
		if err := redactAndGuard(next); err != nil {
			return nil, err
		}
		if err := validate(next); err != nil {
			return nil, err
		}
		if prev == nil {
			if err := validateInit(next); err != nil {
				return nil, err
			}
		} else if err := validateTransition(prev, next); err != nil {
			return nil, err
		}
		return json.Marshal(next)
	})
	if err != nil {
		// A visible-but-durability-unconfirmed append yields a valid record: preserve it
		// paired with the typed error so a direct caller has the value but must confirm
		// durability (never treat it as durable success).
		if genstore.IsDurabilityUnconfirmed(err) {
			rs, derr := decodeRunState(built)
			if derr != nil {
				return RunState{}, derr
			}
			return rs, err
		}
		return RunState{}, err
	}
	// Return the authoritative state decoded from the exact persisted bytes.
	return decodeRunState(built)
}

// ConfirmDurable re-confirms this store's directory is power-safe under the held
// guard (see genstore.Store.ConfirmDurable). A step whose effect is a run-state
// append confirms durability here before its transaction records progress.
func (s *Store) ConfirmDurable(g *genstore.Guard) error { return s.gs.ConfirmDurable(g) }

// WithSyncDir overrides the underlying store's directory-sync barrier. It is a test seam
// for injecting a durability-confirmation failure (a visible-but-unconfirmed append); it
// passes through to genstore.Store.WithSyncDir and returns the receiver for chaining.
func (s *Store) WithSyncDir(fn func(dir string) error) *Store { s.gs.WithSyncDir(fn); return s }

// WithWrite overrides the underlying store's record writer. It is a test seam for
// injecting a visible-but-durability-unconfirmed append (a write that publishes the record
// then reports a post-commit sync failure); it passes through to genstore.Store.WithWrite.
func (s *Store) WithWrite(fn func(path string, data []byte, perm os.FileMode) error) *Store {
	s.gs.WithWrite(fn)
	return s
}

// cloneForNext deep-copies prev (or returns a normalized fresh state) so the
// mutator always sees usable collections and transition validation can compare
// old vs new without aliasing.
// cloneSlice copies a slice, preserving the nil/empty distinction the immutability checks compare on.
func cloneSlice[T any](src []T) []T {
	if src == nil {
		return nil
	}
	dst := make([]T, len(src))
	copy(dst, src)
	return dst
}

func cloneForNext(prev *RunState) *RunState {
	if prev == nil {
		return &RunState{AcceptedTurns: map[string]AcceptedTurn{}, Counters: Counters{StepFixes: []int{}}}
	}
	n := *prev
	n.Counters.StepFixes = cloneInts(prev.Counters.StepFixes)
	// Copied, not shared — for EVERY nested mutable collection, not just the ones that were noticed.
	//
	// A struct copy duplicates only the slice HEADER, so a mutator writing next.X[i] writes through to
	// the previous state as well; the immutability checks compare next against prev with DeepEqual, so
	// they would see equality and accept the very rewrite they exist to catch. That was first found for
	// TestAttempts and then found again, by review, still present on the frozen policy's argv and
	// environment and on the resolved environment — which are exactly the authorities a run must not be
	// able to edit after bootstrap.
	// make+copy rather than append-to-nil, because append on an EMPTY source yields nil: an
	// empty-but-present collection would come back absent, and the immutability comparison would then
	// fail on a mutation that changed nothing.
	n.TestAttempts = cloneSlice(prev.TestAttempts)
	if prev.ActiveTestAttempt != nil {
		a := *prev.ActiveTestAttempt
		n.ActiveTestAttempt = &a
	}
	n.EffectivePolicy.TestGate.Argv = cloneSlice(prev.EffectivePolicy.TestGate.Argv)
	n.EffectivePolicy.TestGate.Env.Inherit = cloneSlice(prev.EffectivePolicy.TestGate.Env.Inherit)
	n.EffectivePolicy.TestGate.Env.Set = cloneSlice(prev.EffectivePolicy.TestGate.Env.Set)
	if prev.ResolvedExecution.Env != nil {
		env := make([]config.ResolvedVar, len(prev.ResolvedExecution.Env))
		for i, e := range prev.ResolvedExecution.Env {
			// The byte slices too: copying the ResolvedVar struct still shares Name and Value.
			env[i] = config.ResolvedVar{
				Name:  cloneSlice(e.Name),
				Value: cloneSlice(e.Value),
			}
		}
		n.ResolvedExecution.Env = env
	}
	n.AcceptedTurns = make(map[string]AcceptedTurn, len(prev.AcceptedTurns))
	for k, v := range prev.AcceptedTurns {
		n.AcceptedTurns[k] = v
	}
	if prev.Assignment != nil {
		a := *prev.Assignment
		n.Assignment = &a
	}
	if prev.Evidence != nil {
		e := *prev.Evidence
		n.Evidence = &e
	}
	if prev.FirstTurn != nil {
		f := *prev.FirstTurn
		n.FirstTurn = &f
	}
	if prev.Gate != nil {
		g := *prev.Gate
		n.Gate = &g
	}
	if prev.CandidatePlan != nil {
		cp := *prev.CandidatePlan
		n.CandidatePlan = &cp
	}
	if prev.CandidateChecks != nil {
		cc := *prev.CandidateChecks
		cc.Keys = cloneKeys(prev.CandidateChecks.Keys)
		n.CandidateChecks = &cc
	}
	if prev.PendingFindings != nil {
		pf := *prev.PendingFindings
		pf.Keys = cloneKeys(prev.PendingFindings.Keys)
		n.PendingFindings = &pf
	}
	if prev.AgreedPlan != nil {
		ap := *prev.AgreedPlan
		ap.Checks.Keys = cloneKeys(prev.AgreedPlan.Checks.Keys)
		n.AgreedPlan = &ap
	}
	if prev.StepIndex != nil {
		si := *prev.StepIndex
		n.StepIndex = &si
	}
	if prev.Verify != nil {
		v := *prev.Verify
		n.Verify = &v
	}
	if prev.Pause != nil {
		pc := *prev.Pause
		if prev.Pause.Budget != nil {
			b := *prev.Pause.Budget
			pc.Budget = &b
		}
		if prev.Pause.Verify != nil {
			v := *prev.Pause.Verify
			pc.Verify = &v
		}
		n.Pause = &pc
	}
	if prev.Recovery != nil {
		r := *prev.Recovery
		n.Recovery = &r
	}
	if prev.Failure != nil {
		f := *prev.Failure
		n.Failure = &f
	}
	return &n
}

// decodeRunState strictly decodes and fully validates a persisted record.
func decodeRunState(rec genstore.Record) (RunState, error) {
	// Check the on-disk schema version FIRST, on a loose probe, so an older/newer
	// generation fails with clear version remediation rather than a vague
	// unknown-field or missing-field corruption error from strict decoding.
	if err := checkSchemaVersion(rec.Payload); err != nil {
		return RunState{}, fmt.Errorf("state: generation %d: %w", rec.Generation, err)
	}
	rs, err := strictDecodeRunState(rec.Payload)
	if err != nil {
		return RunState{}, fmt.Errorf("state: decode generation %d: %w", rec.Generation, err)
	}
	normalize(&rs)
	if rs.Revision != rec.Generation {
		return RunState{}, fmt.Errorf("state: revision %d disagrees with generation %d", rs.Revision, rec.Generation)
	}
	if err := validate(&rs); err != nil {
		return RunState{}, fmt.Errorf("state: generation %d invalid: %w", rec.Generation, err)
	}
	return rs, nil
}

// checkSchemaVersion loosely reads only the schema_version field and rejects a
// generation this build does not support, before any strict shape decoding.
func checkSchemaVersion(payload []byte) error {
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return fmt.Errorf("%w: schema_version is unreadable", ErrUnsupportedSchema)
	}
	if probe.SchemaVersion != RunStateVersion {
		return fmt.Errorf("%w: on-disk schema_version %d, this build expects %d — the run was written by a different claudex version",
			ErrUnsupportedSchema, probe.SchemaVersion, RunStateVersion)
	}
	return nil
}

// normalize ensures nil-vs-empty collections do not create accidental variants.
func normalize(rs *RunState) {
	if rs.AcceptedTurns == nil {
		rs.AcceptedTurns = map[string]AcceptedTurn{}
	}
	if rs.Counters.StepFixes == nil {
		rs.Counters.StepFixes = []int{}
	}
}
