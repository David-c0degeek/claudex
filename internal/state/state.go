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

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/genstore"
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
// quality-budget gates. An older generation is missing a required field, so it
// fails with version remediation, not a vague error (see checkSchemaVersion).
const RunStateVersion = 5

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

// Receipt is the durable acknowledgement of an accepted submit.
type Receipt struct {
	TurnID         string `json:"turn_id"`
	Revision       uint64 `json:"revision"`
	ArtifactDigest string `json:"artifact_digest"`
}

// AcceptedTurn records what was accepted for a turn (keyed by turn_id in the map).
// Phase is the single coordinator-authored acceptance fact: the phase the turn
// was in. Role and artifact message type are derived from it via the turn spec,
// so no redundant, disagreeing facts are persisted.
type AcceptedTurn struct {
	ArtifactDigest string  `json:"artifact_digest"`
	Receipt        Receipt `json:"receipt"`
	Phase          Phase   `json:"phase"`
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
// AWAIT_GUIDANCE + LifecyclePaused + Gate; neither is the D019 usage-window pause
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
	SchemaVersion   int                     `json:"schema_version"`
	RunID           string                  `json:"run_id"`
	Revision        uint64                  `json:"revision"`
	Lifecycle       Lifecycle               `json:"lifecycle"`
	Phase           Phase                   `json:"phase"`
	CreatedUnix     int64                   `json:"created_unix"`
	StartedUnix     int64                   `json:"started_unix"`
	DeadlineUnix    int64                   `json:"deadline_unix"`
	TaskSnapshot    SnapshotRef             `json:"task_snapshot"`
	PolicySnapshot  SnapshotRef             `json:"policy_snapshot"`
	EffectivePolicy config.RunPolicy        `json:"effective_policy"`
	FS              FSResult                `json:"fs"`
	Base            string                  `json:"base"`
	BaseCommit      string                  `json:"base_commit"`
	WorktreeRelPath string                  `json:"worktree_rel_path"`
	RunBranch       string                  `json:"run_branch"`
	Counters        Counters                `json:"counters"`
	Assignment      *Ref                    `json:"assignment,omitempty"`
	FirstTurn       *Ref                    `json:"first_turn,omitempty"`
	Gate            *Ref                    `json:"gate,omitempty"`
	CandidatePlan   *PlanRef                `json:"candidate_plan,omitempty"`
	CandidateChecks *CheckSetRef            `json:"candidate_checks,omitempty"`
	PendingFindings *FindingObligations     `json:"pending_findings,omitempty"`
	AgreedPlan      *PlanAgreement          `json:"agreed_plan,omitempty"`
	StepIndex       *int                    `json:"step_index,omitempty"`
	FixReturn       Phase                   `json:"fix_return,omitempty"`
	Verify          *VerifyRequirement      `json:"verify,omitempty"`
	Pause           *PauseContext           `json:"pause,omitempty"`
	AcceptedTurns   map[string]AcceptedTurn `json:"accepted_turns"`
	PendingTxnID    string                  `json:"pending_txn_id"`
	Recovery        *Projection             `json:"recovery,omitempty"`
	Failure         *Projection             `json:"failure,omitempty"`
}

// Store is the run-state store over a genstore.
type Store struct {
	gs *genstore.Store
}

// Open returns a state store handle (side-effect-free).
func Open(dir, lockPath string) *Store {
	return &Store{gs: genstore.Open(dir, lockPath)}
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
		return RunState{}, err
	}
	// Return the authoritative state decoded from the exact persisted bytes.
	return decodeRunState(built)
}

// cloneForNext deep-copies prev (or returns a normalized fresh state) so the
// mutator always sees usable collections and transition validation can compare
// old vs new without aliasing.
func cloneForNext(prev *RunState) *RunState {
	if prev == nil {
		return &RunState{AcceptedTurns: map[string]AcceptedTurn{}, Counters: Counters{StepFixes: []int{}}}
	}
	n := *prev
	n.Counters.StepFixes = append([]int(nil), prev.Counters.StepFixes...)
	n.AcceptedTurns = make(map[string]AcceptedTurn, len(prev.AcceptedTurns))
	for k, v := range prev.AcceptedTurns {
		n.AcceptedTurns[k] = v
	}
	if prev.Assignment != nil {
		a := *prev.Assignment
		n.Assignment = &a
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
		cc.Keys = append([]string(nil), prev.CandidateChecks.Keys...)
		n.CandidateChecks = &cc
	}
	if prev.PendingFindings != nil {
		pf := *prev.PendingFindings
		pf.Keys = append([]string(nil), prev.PendingFindings.Keys...)
		n.PendingFindings = &pf
	}
	if prev.AgreedPlan != nil {
		ap := *prev.AgreedPlan
		ap.Checks.Keys = append([]string(nil), prev.AgreedPlan.Checks.Keys...)
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
