package coordinator

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
	"github.com/David-c0degeek/claudex/internal/txn"
)

const (
	submitGitMaxAttempts = 64
	submitGitBackoff     = 200 * time.Microsecond
)

// submitGit drives one IMPLEMENT_STEP/FIX submit through the git commit transaction:
// recover any pending transaction, authorize and publish via the shared transport
// pipeline (PREPARE), snapshot the run worktree into a commit object, build the
// deterministic target index, freeze everything in a serializable acceptance plan,
// then journal and drive the three ordered durable effects — ref-cas, index-cas,
// state-cas — under one held run guard. A crash at any cut leaves a journalled
// transaction the next submit (or reopen) recovers forward from the frozen plan.
func (rn *Run) submitGit(ctx context.Context, sessionID string, raw []byte) (transport.SubmitResult, error) {
	// Precompute OFF the guard, exactly like the standalone path: fact I/O and RNG
	// minting against an optimistic snapshot; the guarded prepare work stays pure.
	prepare, err := rn.precompute(ctx, raw)
	if err != nil {
		return transport.SubmitResult{}, err
	}
	if h := hooksFrom(ctx); h != nil && h.afterPrecompute != nil {
		h.afterPrecompute()
	}
	deps := transport.SubmitDeps{
		Store:    rn.state,
		Registry: rn.registry,
		Journal:  runJournalReader{loc: rn.loc, state: rn.state},
		Sink:     rn.store,
		Prepare:  prepare,
	}

	for attempt := 0; attempt < submitGitMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return transport.SubmitResult{}, err
		}
		g, ok, aerr := genstore.Acquire(rn.loc.RunLock)
		if aerr != nil {
			return transport.SubmitResult{}, aerr
		}
		if !ok {
			time.Sleep(submitGitBackoff)
			continue
		}
		return rn.lockedSubmitGit(ctx, deps, g, sessionID, raw)
	}
	return transport.SubmitResult{}, fmt.Errorf("coordinator: git submit did not acquire the run lock after %d attempts: %w", submitGitMaxAttempts, genstore.ErrBusy)
}

// lockedSubmitGit runs the whole transaction under the held guard g, releasing it
// exactly once on every path. A release failure after a durable acceptance is a
// warning on the result (the receipt is authoritative), before it joins the error.
func (rn *Run) lockedSubmitGit(ctx context.Context, deps transport.SubmitDeps, g *genstore.Guard, sessionID string, raw []byte) (res transport.SubmitResult, err error) {
	defer func() {
		if rerr := g.Release(); rerr != nil {
			if err == nil && res.Receipt != (state.Receipt{}) {
				res.ReleaseWarning = rerr
			} else {
				err = errors.Join(err, rerr)
			}
		}
	}()

	journal := txn.Open(rn.loc.CommitTxnDir, rn.loc.RunLock)
	// Recover any pending commit transaction FIRST: its state-cas may still be owed,
	// and PREPARE below must authorize against the post-recovery state.
	if _, _, rerr := journal.Recover(g, rn.commitTxnPlanFor(ctx, g)); rerr != nil {
		return transport.SubmitResult{}, rerr
	}

	// PREPARE: authorize the session, validate the artifact, run the pure engine
	// prepare, and publish the immutable artifact. A replay returns the durable
	// receipt with no new transaction — possibly WITH a durability error the caller
	// must surface alongside the authoritative receipt, so it is checked first.
	plan, replay, perr := transport.PrepareGitSubmit(ctx, deps, g, sessionID, raw)
	if replay != nil {
		return *replay, perr
	}
	if perr != nil {
		return transport.SubmitResult{}, perr
	}

	rs, ok, lerr := rn.state.Load()
	if lerr != nil {
		return transport.SubmitResult{}, lerr
	}
	if !ok {
		return transport.SubmitResult{}, transport.ErrNoRun
	}
	if rs.Revision != plan.ExpectedStateRevision {
		// Impossible under the held guard — an invariant, not a race to retry.
		return transport.SubmitResult{}, fmt.Errorf("coordinator: run advanced under the held guard (revision %d, plan %d)", rs.Revision, plan.ExpectedStateRevision)
	}

	// Freeze I0 BEFORE the snapshot: the snapshot's own barrier then validates the
	// staged index against the captured worktree, so the frozen pre-identity is
	// exactly the index that validation blessed — a foreign index staged after the
	// barrier can never be adopted as the transaction's expected old identity (the
	// pre-PREPARE recheck below and ApplyIndex's live-digest CAS both fail closed
	// on it instead).
	worktree := rn.runWorktree()
	preDigest, derr := rn.git.IndexDigest(ctx, worktree)
	if derr != nil {
		return transport.SubmitResult{}, derr
	}

	// SNAPSHOT: parent is the run's latest accepted git commit (or the base for the
	// first), so the accepted history forms the exact chain the state store validates.
	parent := rs.BaseCommit
	if ev, ok := state.LatestGitCommit(rs); ok {
		parent = ev.Commit
	}
	snapReq := gitx.SnapshotReq{
		RepoDir:          rn.repoDir,
		Parent:           parent,
		RunID:            rs.RunID,
		StartingRevision: rs.Revision,
		CreatedUnix:      rs.CreatedUnix,
	}
	commit, tree, serr := rn.git.SnapshotCommit(ctx, snapReq)
	if serr != nil {
		return transport.SubmitResult{}, serr
	}

	// Build the deterministic target at the txn-private path (proves hard-link
	// support pre-PREPARE). A crash before the journal prepare leaves only a private
	// orphan and an unreferenced commit object.
	txnID, terr := rn.mintTxnID()
	if terr != nil {
		return transport.SubmitResult{}, terr
	}
	private, pverr := rn.git.TargetIndexPath(ctx, worktree, txnID)
	if pverr != nil {
		return transport.SubmitResult{}, pverr
	}
	targetDigest, berr := rn.git.BuildTargetIndex(ctx, worktree, commit, private)
	if berr != nil {
		return transport.SubmitResult{}, berr
	}
	plan.GitCommit = state.GitCommitEvidence{Parent: parent, Tree: tree, Commit: commit}
	plan.IndexPreDigest = preDigest
	plan.IndexTargetDigest = targetDigest

	payload, merr := json.Marshal(plan)
	if merr != nil {
		return transport.SubmitResult{}, merr
	}
	if h := hooksFrom(ctx); h != nil && h.beforeGitJournal != nil {
		if herr := h.beforeGitJournal(); herr != nil {
			return transport.SubmitResult{}, herr
		}
	}
	// Pinned pre-PREPARE recheck: re-prove the FULL frozen identity — the registered
	// linked worktree with symbolic HEAD on the run branch at the frozen parent, the
	// frozen worktree tree, the frozen I0, the untampered txn-private target — and
	// cancellation, immediately before the journal records the intent, so any foreign
	// change in the snapshot..prepare window fails closed with NO journal record and
	// NO effect.
	if err := ctx.Err(); err != nil {
		return transport.SubmitResult{}, err
	}
	if err := rn.git.ConfirmPreState(ctx, snapReq, tree, preDigest, private, targetDigest); err != nil {
		return transport.SubmitResult{}, err
	}
	steps, sterr := rn.commitTxnSteps(ctx, g, plan, txnID)
	if sterr != nil {
		return transport.SubmitResult{}, sterr
	}
	if _, rerr := journal.Run(g, txn.Plan{
		Intent: txn.Intent{
			Version:               txn.IntentVersion,
			Kind:                  attach.CommitTxnIntentKind,
			TxnID:                 txnID,
			ExpectedStateRevision: rs.Revision,
			Payload:               payload,
		},
		Steps: steps,
	}); rerr != nil {
		return transport.SubmitResult{}, rerr
	}

	// The state-cas applied the acceptance; the durable receipt is authoritative.
	after, ok, aerr := rn.state.Load()
	if aerr != nil {
		return transport.SubmitResult{}, aerr
	}
	if ok {
		if a, has := after.AcceptedTurns[plan.TurnID]; has && a.ArtifactDigest == plan.Digest {
			return transport.SubmitResult{Receipt: a.Receipt}, nil
		}
	}
	return transport.SubmitResult{}, fmt.Errorf("coordinator: committed git submit could not be reconciled")
}

// recoverCommitTxn acquires the run guard and drives any pending commit
// transaction to completion from its journalled plan. It is the recovery entry for
// a transaction whose state-cas already became visible (the phase has advanced, so
// no submit routes through the git driver again); a no-op when the journal is
// absent or terminal.
func (rn *Run) recoverCommitTxn(ctx context.Context) error {
	for attempt := 0; attempt < submitGitMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		g, ok, aerr := genstore.Acquire(rn.loc.RunLock)
		if aerr != nil {
			return aerr
		}
		if !ok {
			time.Sleep(submitGitBackoff)
			continue
		}
		journal := txn.Open(rn.loc.CommitTxnDir, rn.loc.RunLock)
		_, _, rerr := journal.Recover(g, rn.commitTxnPlanFor(ctx, g))
		return errors.Join(rerr, g.Release())
	}
	return fmt.Errorf("coordinator: commit-txn recovery did not acquire the run lock after %d attempts: %w", submitGitMaxAttempts, genstore.ErrBusy)
}

// runWorktree is the run's canonical linked-worktree path (the same derivation the
// attach layout and gitx enforce: <run dir>/worktree).
func (rn *Run) runWorktree() string { return filepath.Join(rn.loc.RunDir, "worktree") }

// mintTxnID mints a fresh commit-transaction id from the run's RNG, serialized with
// the other minting so concurrent submits cannot interleave RNG reads.
func (rn *Run) mintTxnID() (string, error) {
	rn.mintMu.Lock()
	defer rn.mintMu.Unlock()
	var b [16]byte
	if _, err := io.ReadFull(rn.rng, b[:]); err != nil {
		return "", err
	}
	return "ctxn-" + hex.EncodeToString(b[:]), nil
}

// commitTxnPlanFor rebuilds a pending commit transaction's plan from its journalled
// intent: strictly decode the frozen acceptance plan, bind it to the opened run, and
// rebuild the same three steps from the frozen identities. No re-snapshot and no
// re-authorization — the registry/session authorized PREPARE; recovery only completes
// the frozen durable effects.
func (rn *Run) commitTxnPlanFor(ctx context.Context, g *genstore.Guard) func(txn.Intent) (txn.Plan, error) {
	return func(in txn.Intent) (txn.Plan, error) {
		if in.Kind != attach.CommitTxnIntentKind {
			return txn.Plan{}, fmt.Errorf("coordinator: unexpected transaction kind %q", in.Kind)
		}
		dec := json.NewDecoder(bytes.NewReader(in.Payload))
		dec.DisallowUnknownFields()
		var plan transport.GitAcceptPlan
		if err := dec.Decode(&plan); err != nil {
			return txn.Plan{}, fmt.Errorf("coordinator: commit-txn payload undecodable: %v", err)
		}
		if plan.RunID != rn.loc.RunID {
			return txn.Plan{}, fmt.Errorf("coordinator: commit-txn payload run id %q is not the opened run %q", plan.RunID, rn.loc.RunID)
		}
		// Recovery must re-read AND durably re-confirm the exact frozen evidence
		// artifact by key BEFORE any step runs — never re-project it, and never let a
		// transaction whose artifact vanished, corrupted, or was never proven durable
		// move the ref/index or accept the turn.
		if err := rn.store.Confirm(plan.TurnID, plan.Digest); err != nil {
			return txn.Plan{}, fmt.Errorf("coordinator: the frozen evidence artifact is missing or corrupt: %w", err)
		}
		steps, err := rn.commitTxnSteps(ctx, g, plan, in.TxnID)
		if err != nil {
			return txn.Plan{}, err
		}
		return txn.Plan{Intent: in, Steps: steps}, nil
	}
}

// commitTxnSteps builds the transaction's three ordered, idempotent durable effects
// from a frozen plan: ref-cas (the run branch moves Parent -> Commit), index-cas (the
// real checked-out index syncs I0 -> target without touching worktree bytes), and
// state-cas (the acceptance applies from the frozen plan). Every Status maps the
// participant's observation onto the journal's Applied/NotApplied/Indeterminate; a
// foreign observation fails closed.
func (rn *Run) commitTxnSteps(ctx context.Context, g *genstore.Guard, plan transport.GitAcceptPlan, txnID string) ([]txn.Step, error) {
	worktree := rn.runWorktree()
	private, err := rn.git.TargetIndexPath(ctx, worktree, txnID)
	if err != nil {
		return nil, err
	}
	// Crash-cut seams (nil in production): fire at the start of a step's Apply and
	// ConfirmDurable, so a test halts the transaction with exactly the durable state
	// a power cut at that point would leave.
	hooks := hooksFrom(ctx)
	cutApply := func(step string) error {
		if hooks != nil && hooks.stepApply != nil {
			return hooks.stepApply(step)
		}
		return nil
	}
	cutConfirm := func(step string) error {
		if hooks != nil && hooks.stepConfirm != nil {
			return hooks.stepConfirm(step)
		}
		return nil
	}
	refT := gitx.RefTarget{
		Branch: "claudex/" + rn.loc.RunID,
		Parent: plan.GitCommit.Parent,
		Tree:   plan.GitCommit.Tree,
		Commit: plan.GitCommit.Commit,
	}
	idxT := gitx.IndexTarget{
		Worktree:     worktree,
		Commit:       plan.GitCommit.Commit,
		Tree:         plan.GitCommit.Tree,
		PreDigest:    plan.IndexPreDigest,
		TargetDigest: plan.IndexTargetDigest,
		Private:      private,
	}
	return []txn.Step{
		{
			Name: "ref-cas",
			Status: func() (txn.StepStatus, error) {
				st, err := rn.git.ObserveRef(ctx, rn.repoDir, refT)
				if err != nil {
					return txn.StatusIndeterminate, err
				}
				switch st {
				case gitx.RefAtCommit:
					return txn.StatusApplied, nil
				case gitx.RefAtParent:
					return txn.StatusNotApplied, nil
				default:
					return txn.StatusIndeterminate, nil
				}
			},
			Apply: func() error {
				if err := cutApply("ref-cas"); err != nil {
					return err
				}
				return rn.git.ApplyRef(ctx, rn.repoDir, refT)
			},
			ConfirmDurable: func() error {
				if err := cutConfirm("ref-cas"); err != nil {
					return err
				}
				return rn.git.ConfirmRef(ctx, rn.repoDir, refT)
			},
		},
		{
			Name: "index-cas",
			Status: func() (txn.StepStatus, error) {
				st, err := rn.git.ObserveIndex(ctx, idxT)
				if err != nil {
					return txn.StatusIndeterminate, err
				}
				switch st {
				case gitx.IndexAtTarget:
					return txn.StatusApplied, nil
				case gitx.IndexAtOld:
					return txn.StatusNotApplied, nil
				default:
					return txn.StatusIndeterminate, nil
				}
			},
			Apply: func() error {
				if err := cutApply("index-cas"); err != nil {
					return err
				}
				return rn.git.ApplyIndex(ctx, idxT)
			},
			ConfirmDurable: func() error {
				if err := cutConfirm("index-cas"); err != nil {
					return err
				}
				return rn.git.ConfirmIndex(ctx, idxT)
			},
		},
		{
			Name: "state-cas",
			Status: func() (txn.StepStatus, error) {
				rs, ok, err := rn.state.Load()
				if err != nil {
					return txn.StatusIndeterminate, err
				}
				if !ok {
					return txn.StatusIndeterminate, transport.ErrNoRun
				}
				switch transport.ObserveGitAccept(rs, plan) {
				case transport.GitAcceptApplied:
					return txn.StatusApplied, nil
				case transport.GitAcceptNotApplied:
					return txn.StatusNotApplied, nil
				default:
					return txn.StatusIndeterminate, nil
				}
			},
			Apply: func() error {
				if err := cutApply("state-cas"); err != nil {
					return err
				}
				// The acceptance references the artifact by (turn, digest): re-read and
				// durably re-confirm it immediately before the append, so a vanished,
				// corrupted, or durability-unproven artifact can never become an
				// accepted receipt.
				if err := rn.store.Confirm(plan.TurnID, plan.Digest); err != nil {
					return fmt.Errorf("coordinator: the frozen evidence artifact is missing or corrupt: %w", err)
				}
				rs, ok, err := rn.state.Load()
				if err != nil {
					return err
				}
				if !ok {
					return transport.ErrNoRun
				}
				_, merr := rn.state.MutateLocked(g, plan.ExpectedStateRevision, func(gen uint64, next *state.RunState) error {
					return transport.FinalizeGitAccept(rs, gen, next, plan)
				})
				return merr
			},
			ConfirmDurable: func() error {
				if err := cutConfirm("state-cas"); err != nil {
					return err
				}
				return rn.state.ConfirmDurable(g)
			},
		},
	}, nil
}
