package coordinator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// gitTxnRig is one opened run at IMPLEMENT_STEP with a real edit staged in its
// linked worktree, ready for one git-commit transaction.
type gitTxnRig struct {
	repo, runID, lead, pair string
	rn                      *Run
	g                       *gitx.Git
	implTurn                string
	implRaw                 []byte
	preRevision             uint64
	worktreeFile            string
	worktreeBytes           []byte
}

func newGitTxnRig(t *testing.T) *gitTxnRig {
	t.Helper()
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	t.Cleanup(func() { rn.Close() })
	g, err := gitx.New()
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))
	rs = cur(t, rn)
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil))
	rs = cur(t, rn)
	if rs.Phase != state.PhaseImplementStep {
		t.Fatalf("not at IMPLEMENT_STEP: %s", rs.Phase)
	}
	editWorktree(t, rn)
	file := filepath.Join(rn.runWorktree(), "work.txt")
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read edit: %v", err)
	}
	return &gitTxnRig{
		repo: repo, runID: runID, lead: lead, pair: pair,
		rn: rn, g: g,
		implTurn: rs.Assignment.ID, implRaw: implReport(t, rs.Assignment.ID, rs.Revision),
		preRevision: rs.Revision, worktreeFile: file, worktreeBytes: content,
	}
}

func (r *gitTxnRig) branchOID(t *testing.T) string {
	t.Helper()
	out, err := r.g.Run(context.Background(), r.repo, nil, "rev-parse", "--verify", "refs/heads/claudex/"+r.runID)
	if err != nil {
		t.Fatalf("branch oid: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// journalRecord returns the newest commit-txn journal record and its decoded plan.
func (r *gitTxnRig) journalRecord(t *testing.T) (txn.Record, transport.GitAcceptPlan, bool) {
	t.Helper()
	rec, ok, err := txn.Open(r.rn.loc.CommitTxnDir, r.rn.loc.RunLock).Latest()
	if err != nil {
		t.Fatalf("journal latest: %v", err)
	}
	if !ok {
		return txn.Record{}, transport.GitAcceptPlan{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(rec.Intent.Payload))
	dec.DisallowUnknownFields()
	var plan transport.GitAcceptPlan
	if err := dec.Decode(&plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	return rec, plan, true
}

// assertRecovered proves the transaction completed exactly once from the frozen
// plan: the run branch is at the frozen commit, the acceptance carries the frozen
// evidence, the real index is clean, the worktree bytes were never rewritten, and
// an identical replay returns the same receipt without a second commit.
func (r *gitTxnRig) assertRecovered(t *testing.T, res transport.SubmitResult) {
	t.Helper()
	rec, plan, ok := r.journalRecord(t)
	if !ok || !rec.Complete || rec.Aborted {
		t.Fatalf("journal not cleanly terminal: %+v ok=%v", rec, ok)
	}
	rs := cur(t, r.rn)
	acc, ok := rs.AcceptedTurns[r.implTurn]
	if !ok || acc.GitCommit == nil || *acc.GitCommit != plan.GitCommit {
		t.Fatalf("acceptance evidence %+v does not match the frozen plan %+v", acc.GitCommit, plan.GitCommit)
	}
	if acc.Receipt != res.Receipt {
		t.Fatalf("returned receipt %+v != durable receipt %+v", res.Receipt, acc.Receipt)
	}
	if got := r.branchOID(t); got != plan.GitCommit.Commit {
		t.Fatalf("branch at %s, want the frozen commit %s", got, plan.GitCommit.Commit)
	}
	if now, err := os.ReadFile(r.worktreeFile); err != nil || string(now) != string(r.worktreeBytes) {
		t.Fatalf("recovery rewrote worktree bytes (err %v)", err)
	}
	out, err := r.g.Run(context.Background(), r.rn.runWorktree(), nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(strings.TrimRight(string(out), "\x00")) != 0 {
		t.Fatalf("worktree/index not clean after recovery: %q", out)
	}
	replay, err := r.rn.Submit(context.Background(), r.lead, r.implRaw)
	if err != nil {
		t.Fatalf("replay after recovery: %v", err)
	}
	if !replay.Idempotent || replay.Receipt != res.Receipt {
		t.Fatalf("replay = %+v (idem %v), want the recovered receipt", replay.Receipt, replay.Idempotent)
	}
	if got := r.branchOID(t); got != plan.GitCommit.Commit {
		t.Fatalf("replay moved the branch to %s", got)
	}
}

var errCut = errors.New("injected crash cut")

// oneShot returns hooks that fail once at the named seam/step, simulating a crash
// exactly there; the recovery retry runs hook-free.
func oneShotApply(step string) *submitHooks {
	fired := false
	return &submitHooks{stepApply: func(s string) error {
		if s == step && !fired {
			fired = true
			return errCut
		}
		return nil
	}}
}

func oneShotConfirm(step string) *submitHooks {
	fired := false
	return &submitHooks{stepConfirm: func(s string) error {
		if s == step && !fired {
			fired = true
			return errCut
		}
		return nil
	}}
}

// TestGitTxnCrashMatrixRecoversForward cuts the transaction at every journalled
// boundary — prepare recorded with no effects, each effect applied without its
// progress record, and each progress record without the next effect — and proves a
// hook-free retry recovers the SAME transaction forward from the frozen intent: one
// commit, one acceptance, one receipt.
func TestGitTxnCrashMatrixRecoversForward(t *testing.T) {
	cuts := []struct {
		name  string
		hooks *submitHooks
		// stepsDone is the journalled progress the cut must leave behind. A cut at a
		// step's ConfirmDurable surfaces wrapped as the journal's durability sentinel
		// (the effect is visible, its durability unproven); an Apply cut surfaces raw.
		stepsDone int
		wantErr   error
	}{
		{"prepare recorded, no effects", oneShotApply("ref-cas"), 0, errCut},
		{"ref effect applied, progress unrecorded", oneShotConfirm("ref-cas"), 0, txn.ErrDurabilityUnconfirmed},
		{"ref progress recorded, index effect absent", oneShotApply("index-cas"), 1, errCut},
		{"index effect applied, progress unrecorded", oneShotConfirm("index-cas"), 1, txn.ErrDurabilityUnconfirmed},
		{"index progress recorded, state effect absent", oneShotApply("state-cas"), 2, errCut},
		{"state applied, terminal unrecorded", oneShotConfirm("state-cas"), 2, txn.ErrDurabilityUnconfirmed},
	}
	for _, tc := range cuts {
		t.Run(tc.name, func(t *testing.T) {
			r := newGitTxnRig(t)
			if _, err := r.rn.Submit(withHooks(context.Background(), tc.hooks), r.lead, r.implRaw); !errors.Is(err, tc.wantErr) {
				t.Fatalf("cut submit err = %v, want %v", err, tc.wantErr)
			}
			rec, _, ok := r.journalRecord(t)
			if !ok || rec.Terminal() {
				t.Fatalf("cut did not leave a pending transaction: %+v ok=%v", rec, ok)
			}
			if rec.StepsDone != tc.stepsDone {
				t.Fatalf("cut left %d steps recorded, want %d", rec.StepsDone, tc.stepsDone)
			}
			// While the transaction is pending, every OTHER mutator fails closed.
			if _, err := r.rn.SubmitTestOutcome(context.Background(), true, strings.Repeat("a", 64), r.preRevision); !errors.Is(err, transport.ErrRecoveryRequired) {
				t.Fatalf("pending txn did not block a test outcome: %v", err)
			}
			// The hook-free retry recovers the frozen transaction forward.
			res, err := r.rn.Submit(context.Background(), r.lead, r.implRaw)
			if err != nil {
				t.Fatalf("recovery submit: %v", err)
			}
			r.assertRecovered(t, res)
		})
	}
}

// A crash AFTER the artifact publish and object builds but BEFORE the journal
// prepare leaves only orphans (an unaccepted artifact, an unreferenced commit, a
// private index file). A retry starts a fresh transaction over the identical
// bytes: the deterministic snapshot re-derives the same commit, the sink
// re-confirms the orphan, and exactly one acceptance results.
func TestGitTxnPrePrepareOrphanRetry(t *testing.T) {
	r := newGitTxnRig(t)
	fired := false
	hooks := &submitHooks{beforeGitJournal: func() error {
		if !fired {
			fired = true
			return errCut
		}
		return nil
	}}
	if _, err := r.rn.Submit(withHooks(context.Background(), hooks), r.lead, r.implRaw); !errors.Is(err, errCut) {
		t.Fatalf("cut submit err = %v, want the injected cut", err)
	}
	if _, _, ok := r.journalRecord(t); ok {
		t.Fatal("pre-prepare cut must leave NO journal record")
	}
	res, err := r.rn.Submit(context.Background(), r.lead, r.implRaw)
	if err != nil {
		t.Fatalf("retry submit: %v", err)
	}
	r.assertRecovered(t, res)
}

// A foreign ref move while the transaction is pending fails recovery closed: the
// pending transaction stays pending and the foreign ref is never overwritten.
func TestGitTxnForeignRefFailsClosed(t *testing.T) {
	r := newGitTxnRig(t)
	if _, err := r.rn.Submit(withHooks(context.Background(), oneShotApply("index-cas")), r.lead, r.implRaw); !errors.Is(err, errCut) {
		t.Fatalf("cut submit err = %v, want the injected cut", err)
	}
	// Move the run branch to a foreign commit (an empty-tree commit object).
	env := map[string]string{
		"GIT_AUTHOR_NAME": "f", "GIT_AUTHOR_EMAIL": "f@example.invalid",
		"GIT_COMMITTER_NAME": "f", "GIT_COMMITTER_EMAIL": "f@example.invalid",
	}
	tree := strings.TrimSpace(string(mustGit(t, r.g, r.repo, nil, "hash-object", "-t", "tree", "-w", "--stdin")))
	foreign := strings.TrimSpace(string(mustGit(t, r.g, r.repo, env, "commit-tree", tree, "-m", "foreign")))
	mustGit(t, r.g, r.repo, nil, "update-ref", "refs/heads/claudex/"+r.runID, foreign)

	if _, err := r.rn.Submit(context.Background(), r.lead, r.implRaw); !errors.Is(err, txn.ErrRecoveryRequired) {
		t.Fatalf("foreign-ref recovery err = %v, want txn.ErrRecoveryRequired", err)
	}
	if got := r.branchOID(t); got != foreign {
		t.Fatalf("recovery touched the foreign ref: %s", got)
	}
	if rec, _, ok := r.journalRecord(t); !ok || rec.Terminal() {
		t.Fatalf("foreign-ref recovery terminalized the transaction: %+v", rec)
	}
	if rs := cur(t, r.rn); rs.Revision != r.preRevision {
		t.Fatalf("foreign-ref recovery advanced the state to %d", rs.Revision)
	}
}

// A post-snapshot worktree edit while the transaction is pending fails the
// index-cas closed: the edit is preserved, the index is never overwritten, and the
// transaction stays pending.
func TestGitTxnForeignWorktreeFailsClosed(t *testing.T) {
	r := newGitTxnRig(t)
	if _, err := r.rn.Submit(withHooks(context.Background(), oneShotApply("index-cas")), r.lead, r.implRaw); !errors.Is(err, errCut) {
		t.Fatalf("cut submit err = %v, want the injected cut", err)
	}
	edited := []byte("post-snapshot foreign edit\n")
	if err := os.WriteFile(r.worktreeFile, edited, 0o600); err != nil {
		t.Fatalf("foreign edit: %v", err)
	}
	if _, err := r.rn.Submit(context.Background(), r.lead, r.implRaw); !errors.Is(err, txn.ErrRecoveryRequired) {
		t.Fatalf("foreign-worktree recovery err = %v, want txn.ErrRecoveryRequired", err)
	}
	if now, err := os.ReadFile(r.worktreeFile); err != nil || string(now) != string(edited) {
		t.Fatalf("the foreign worktree edit was not preserved (err %v)", err)
	}
	if rec, _, ok := r.journalRecord(t); !ok || rec.Terminal() {
		t.Fatalf("foreign-worktree recovery terminalized the transaction: %+v", rec)
	}
}

// A state-cas append that lands VISIBLE but durability-unconfirmed halts the
// transaction with ErrDurabilityUnconfirmed and NO recorded progress; recovery
// against a healthy store observes the visible acceptance, re-confirms it, and
// terminalizes — exactly one acceptance, never a double apply.
func TestGitTxnStateVisibleUnconfirmedRecovery(t *testing.T) {
	r := newGitTxnRig(t)
	failSync := errors.New("dir sync persistently fails")
	r.rn.state.WithWrite(func(path string, data []byte, perm os.FileMode) error {
		if werr := atomicfile.Write(path, data, perm); werr != nil {
			return werr
		}
		return &atomicfile.PostCommitSyncError{Path: path, Err: errors.New("dir sync failed")}
	}).WithSyncDir(func(string) error { return failSync })

	_, err := r.rn.Submit(context.Background(), r.lead, r.implRaw)
	if !errors.Is(err, txn.ErrDurabilityUnconfirmed) {
		t.Fatalf("visible-unconfirmed submit err = %v, want txn.ErrDurabilityUnconfirmed", err)
	}
	rec, _, ok := r.journalRecord(t)
	if !ok || rec.Terminal() || rec.StepsDone != 2 {
		t.Fatalf("halt did not stop at the state step: %+v ok=%v", rec, ok)
	}
	// The acceptance IS visible; only its durability is unconfirmed.
	if rs := cur(t, r.rn); rs.Revision == r.preRevision {
		t.Fatal("state append is not visible")
	}

	// Heal the store; the retry recovers: re-observes the visible acceptance,
	// re-confirms durability, and terminalizes without a second apply.
	r.rn.state.WithWrite(atomicfile.Write).WithSyncDir(atomicfile.SyncDir)
	res, err := r.rn.Submit(context.Background(), r.lead, r.implRaw)
	if err != nil {
		t.Fatalf("healed recovery: %v", err)
	}
	r.assertRecovered(t, res)
}
