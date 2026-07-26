package transport

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

var (
	// ErrNotTestsPhase means the run is not a running ownerless TESTS phase, so it
	// cannot accept a coordinator-authored test outcome.
	ErrNotTestsPhase = errors.New("transport: run is not a running ownerless TESTS phase")
	// ErrBadEvidence means the test-evidence digest is not a canonical 64-hex digest or
	// the expected revision is zero.
	ErrBadEvidence = errors.New("transport: test outcome evidence is malformed")
	// ErrOutcomeUnknown means the outcome append MAY have committed but could not be
	// reconciled; the caller must reload the run state to determine the result — a zero
	// result does NOT prove no mutation.
	ErrOutcomeUnknown = errors.New("transport: test outcome result is unknown; reload the run state")
)

// PreparedTestOutcome is the value-only, immutable input handed to a TestPrepare: the
// expected revision, the ownerless test-evidence source (an empty-turn digest, never an
// agent artifact/AcceptedTurn), and the locked current pair generation the FIX/VERIFY
// threshold is computed against. Its facts come only from the locked snapshot.
type PreparedTestOutcome struct {
	ExpectedRevision      uint64
	Source                state.EventRef // {TurnID:"", Digest: evidence digest}
	CurrentPairGeneration uint64
}

// TestPrepare is the coordinator-authored, authorization-independent semantic
// preparation of a test outcome: from a deep clone of the locked run state and the
// prepared outcome it returns the deterministic transition to apply, or a value-free
// error. It must not perform I/O, mint identities under the guard, or mutate the
// accepted-turns ledger.
type TestPrepare func(snapshot state.RunState, prepared PreparedTestOutcome) (PreparedTransition, error)

// TestOutcomeDeps are the injected, lock-sharing dependencies of a test outcome. There
// is deliberately no session, sink, or raw artifact: the outcome is coordinator-
// authored and writes no submit artifact.
type TestOutcomeDeps struct {
	Store    *state.Store
	Registry *state.RegistryStore
	Journal  JournalReader
	Prepare  TestPrepare
	// WorktreeClean observes the run worktree at the TESTS boundary. It is REQUIRED, not
	// optional: this outcome is coordinator-authored, so there is no caller for whom the
	// observation could legitimately be absent, and the boundary it guards is the one place an
	// editable turn can be issued without an agent submit having already proven the worktree.
	WorktreeClean WorktreeClean
}

func (d TestOutcomeDeps) validate() error {
	if d.Store == nil || d.Registry == nil || d.Journal == nil || d.Prepare == nil || d.WorktreeClean == nil {
		return ErrMissingSeam
	}
	lp := d.Store.LockPath()
	if d.Registry.LockPath() != lp || d.Journal.LockPath() != lp {
		return ErrLockMismatch
	}
	return nil
}

// TestOutcomeResult is the outcome of a committed test-outcome transition. It carries
// no receipt (there is no accepted turn) — only the committed run-state revision.
type TestOutcomeResult struct {
	Revision uint64
	// ReleaseWarning is non-nil when the transition committed but the lock release
	// failed; the transition is durable and the revision is authoritative.
	ReleaseWarning error
}

// SubmitTestOutcome applies a coordinator-authored test outcome to a run at ownerless
// TESTS under the one run guard. It writes NO artifact and NO accepted turn — the
// caller guarantees evidenceDigest identifies immutable external evidence. It is NOT
// idempotent: a replay after the state has advanced is stale or phase-inapplicable,
// because the durable outcome identity belongs to the external evidence journal.
func SubmitTestOutcome(ctx context.Context, deps TestOutcomeDeps, expectedRevision uint64, evidenceDigest string) (TestOutcomeResult, error) {
	if err := deps.validate(); err != nil {
		return TestOutcomeResult{}, err
	}
	if !state.IsHex64(evidenceDigest) || expectedRevision == 0 {
		return TestOutcomeResult{}, ErrBadEvidence
	}

	for attempt := 0; attempt < submitMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return TestOutcomeResult{}, err
		}
		g, ok, aerr := genstore.Acquire(deps.Store.LockPath())
		if aerr != nil {
			return TestOutcomeResult{}, aerr
		}
		if !ok {
			time.Sleep(submitBackoff)
			continue
		}
		return lockedTestOutcome(ctx, deps, g, expectedRevision, evidenceDigest)
	}
	return TestOutcomeResult{}, fmt.Errorf("transport: test outcome did not acquire the run lock after %d attempts: %w", submitMaxAttempts, genstore.ErrBusy)
}

func lockedTestOutcome(ctx context.Context, deps TestOutcomeDeps, g *genstore.Guard, expectedRevision uint64, evidenceDigest string) (TestOutcomeResult, error) {
	released := false
	release := func() error {
		if released {
			return nil
		}
		released = true
		return g.Release()
	}
	defer release()
	reject := func(opErr error) (TestOutcomeResult, error) {
		return TestOutcomeResult{}, errors.Join(opErr, releaseOutcome(false, 0, release()))
	}

	rs, ok, lerr := deps.Store.Load()
	if lerr != nil {
		return reject(lerr)
	}
	if !ok {
		return reject(ErrNoRun)
	}

	// Journal policy BEFORE the registry (a crash mid-attach is recovery, not a mismatch).
	head, jerr := deps.Journal.Head(g, rs.RunID)
	if jerr != nil {
		return reject(fmt.Errorf("%w: %v", ErrRecoveryRequired, jerr))
	}
	switch head {
	case JournalTerminal, JournalAbsent:
		// proceed
	default:
		return reject(ErrRecoveryRequired)
	}

	reg, ok, rerr := deps.Registry.Load()
	if rerr != nil {
		return reject(rerr)
	}
	if !ok || reg.RunID != rs.RunID {
		return reject(ErrRunMismatch)
	}

	// The run must be a running, ownerless TESTS phase.
	if rs.Phase != state.PhaseTests || rs.Lifecycle != state.LifecycleRunning ||
		rs.Assignment != nil || rs.Gate != nil || rs.Recovery != nil {
		return reject(ErrNotTestsPhase)
	}
	if expectedRevision != rs.Revision {
		return reject(staleErr(rs, expectedRevision))
	}

	// The TESTS boundary. Nothing has edited the repository legitimately since the last
	// implementation was accepted — TESTS is ownerless and every phase between carries a
	// read-only turn — so dirt observed here PREDATES this boundary and raced the snapshot.
	//
	// It is checked for EVERY genuine new authorized outcome, not only the branch that issues
	// FIX: a PASS advances into VERIFY, which is equally a phase no one may have edited in, and
	// gating only the FIX branch would let a dirty worktree through on exactly the path where
	// nobody looks at the repository again before the verifier reviews it. The check sits after
	// the stale/phase/journal/registry authorization and before Prepare, so a refusal binds no
	// identity and moves no state.
	if werr := boundaryWorktreeGate(deps.WorktreeClean); werr != nil {
		return reject(werr)
	}

	curPairGen, pgErr := currentPairGeneration(reg)
	if pgErr != nil {
		return reject(pgErr)
	}

	snapshot, cerr := cloneRunState(rs)
	if cerr != nil {
		return reject(fmt.Errorf("transport: could not snapshot the run state: %w", cerr))
	}
	prepared := PreparedTestOutcome{
		ExpectedRevision:      rs.Revision,
		Source:                state.EventRef{Digest: evidenceDigest}, // ownerless: empty turn id
		CurrentPairGeneration: curPairGen,
	}
	pt, perr := deps.Prepare(snapshot, prepared)
	if perr != nil {
		return reject(perr)
	}
	if pt.apply == nil {
		return reject(fmt.Errorf("%w: prepared transition has no apply", ErrTransitionInvalid))
	}
	// The outcome consumes no turn, so the "" consumed-turn is passed to the id recheck.
	if idErr := recheckIssuedIDs(pt, rs, ""); idErr != nil {
		return reject(idErr)
	}
	if err := ctx.Err(); err != nil {
		return reject(err)
	}

	// One CAS. No sink Put, no AcceptedTurn/Receipt: the ledger must be byte-identical
	// across the transition (a forged Prepare that nils/adds/deletes/alters it fails).
	committed, merr := deps.Store.MutateLocked(g, rs.Revision, func(gen uint64, next *state.RunState) error {
		before := cloneAcceptedTurns(next.AcceptedTurns)
		if aerr := pt.apply(gen, next); aerr != nil {
			return aerr
		}
		if !reflect.DeepEqual(before, next.AcceptedTurns) {
			return fmt.Errorf("%w: a test outcome must not change the accepted-turns ledger", ErrTransitionInvalid)
		}
		// The resulting shape must be one of the exact TESTS-graph outcomes ("not TESTS"
		// is insufficient — state encodes no phase edges and any terminal lifecycle is
		// locally valid, so a faulty Prepare could otherwise reach DONE).
		if serr := checkTestOutcomeShape(rs, next, prepared); serr != nil {
			return serr
		}
		if lerr := requireLiveOwner(next, "", gen); lerr != nil {
			return lerr
		}
		if berr := bindIssued(pt, next); berr != nil {
			return berr
		}
		if terr := checkEnterVerifyThreshold(rs, next, curPairGen); terr != nil {
			return terr
		}
		return nil
	})
	if merr != nil && genstore.IsDurabilityUnconfirmed(merr) {
		// The transition is VISIBLE and committed (committed.Revision is authoritative) but its
		// directory entry is not yet power-safe. Re-confirm inline before classifying: on
		// success it is durable; a persistent failure MUST preserve the committed/visible
		// classification (the caller re-confirms on recovery), never a proven-uncommitted zero
		// that would invite a retry of an already-applied ownerless transition.
		if cerr := deps.Store.ConfirmDurable(g); cerr != nil {
			return TestOutcomeResult{Revision: committed.Revision}, errors.Join(merr, cerr, release())
		}
		merr = nil
	}
	return classifyTestOutcome(committed.Revision, merr, release())
}

// checkTestOutcomeShape admits ONLY the three legal TESTS outcomes and rejects every
// other phase/lifecycle (including a forged TESTS->DONE): a pass into ownerless VERIFY
// (every counter preserved), an assigned fail into FIX returning to TESTS (test_fixes
// +1 and every other counter preserved, only while the frozen budget still permits a
// fix), or the exact TESTS quality-budget gate whose pause carries the ACCEPTED evidence
// source (so a Prepare cannot substitute a different valid digest). The counter effect
// is exact — a bare test_fixes check would let a faulty Prepare bump plan_revisions /
// verify_fixes / step_fixes, or push test_fixes past the policy limit instead of gating.
func checkTestOutcomeShape(rs state.RunState, next *state.RunState, prepared PreparedTestOutcome) error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrTransitionInvalid, msg) }
	switch next.Phase {
	case state.PhaseVerify:
		if next.Assignment != nil || next.Verify == nil {
			return bad("a TESTS pass enters ownerless VERIFY")
		}
		if !reflect.DeepEqual(next.Counters, rs.Counters) {
			return bad("a TESTS pass must preserve every counter")
		}
	case state.PhaseFix:
		if next.Assignment == nil || next.FixReturn != state.PhaseTests {
			return bad("a TESTS fail assigns a FIX returning to TESTS")
		}
		// The frozen budget decides fix-vs-gate: at the limit the only legal outcome is
		// the quality gate, so a FIX is admissible only below it.
		if rs.Counters.TestFixes >= rs.EffectivePolicy.Budgets.TestRounds {
			return bad("a TESTS fail at the frozen test budget must open the quality gate, not a FIX")
		}
		want := rs.Counters
		want.TestFixes = rs.Counters.TestFixes + 1
		if !reflect.DeepEqual(next.Counters, want) {
			return bad("a TESTS fail increments only test_fixes, by exactly one")
		}
	case state.PhaseAwaitGuidance:
		p := next.Pause
		if p == nil || p.Kind != state.PauseQualityBudget || p.Budget == nil || p.Budget.Kind != state.BudgetTest ||
			p.OriginPhase != state.PhaseTests || p.ResumePhase != state.PhaseFix || p.FixReturn != state.PhaseTests {
			return bad("a TESTS quality gate is the closed TESTS->FIX budget shape")
		}
		if p.Source != prepared.Source {
			return bad("the TESTS gate source must be the accepted evidence digest")
		}
		if !reflect.DeepEqual(next.Counters, rs.Counters) {
			return bad("a TESTS quality gate must preserve every counter")
		}
	default:
		return bad("a TESTS outcome must enter VERIFY, FIX, or the TESTS quality gate")
	}
	return nil
}

// classifyTestOutcome is the pure post-CAS result classifier: a committed append yields
// the revision plus a committed release-warning; a genstore.ErrAmbiguous is a typed
// outcome-unknown (never a proven-uncommitted zero result); every other failure is a
// proven-uncommitted zero result.
func classifyTestOutcome(committedRev uint64, merr, relErr error) (TestOutcomeResult, error) {
	switch {
	case merr == nil:
		return TestOutcomeResult{Revision: committedRev, ReleaseWarning: releaseOutcome(true, committedRev, relErr)}, nil
	case errors.Is(merr, genstore.ErrAmbiguous):
		return TestOutcomeResult{}, errors.Join(fmt.Errorf("%w: %w", ErrOutcomeUnknown, merr), relErr)
	case errors.Is(merr, state.ErrRevisionConflict) || errors.Is(merr, genstore.ErrBusy):
		return TestOutcomeResult{}, errors.Join(fmt.Errorf("transport: unexpected lock/revision state under the run guard: %w", merr), relErr)
	default:
		return TestOutcomeResult{}, errors.Join(merr, relErr)
	}
}

// cloneAcceptedTurns deep-copies the accepted-turns ledger for a before/after compare.
func cloneAcceptedTurns(m map[string]state.AcceptedTurn) map[string]state.AcceptedTurn {
	if m == nil {
		return nil
	}
	c := make(map[string]state.AcceptedTurn, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// boundaryWorktreeGate refuses a phase boundary whose worktree is already dirty.
//
// The successful observation is the LINEARIZATION POINT of the boundary, exactly as the read-only
// submit gate defines it: dirt present at this instant predates the boundary and refuses it, and
// dirt appearing after belongs to the editable interval this transition opens. The run guard does
// not lock external filesystem writers, so an edit landing between this observation and the state
// CAS is attributed to the new interval. That residue is inherent — recording the worktree at
// issuance and checking it at the next submit would move the race, not remove it — so the contract
// is stated rather than papered over.
//
// Observed dirt and an unobservable worktree stay separately typed: an observation that could not
// run is never reported as an observed edit, and its cause is preserved for errors.Is.
func boundaryWorktreeGate(observe WorktreeClean) error {
	clean, err := observe()
	if err != nil {
		return errors.Join(ErrWorktreeUnobserved, err)
	}
	if !clean {
		return ErrPostSnapshotEdit
	}
	return nil
}
