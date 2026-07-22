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
	"github.com/David-c0degeek/claudex/internal/attach"
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

// A foreign index staged in the snapshot..PREPARE window must fail the submit closed
// BEFORE any journal record or effect: the transaction may never adopt a later index
// as its expected old identity (the section-D counterexample), and the foreign staged
// state is preserved untouched.
func TestGitTxnForeignStagedIndexPrePrepareFailsClosed(t *testing.T) {
	r := newGitTxnRig(t)
	base := r.branchOID(t)
	hooks := &submitHooks{beforeGitJournal: func() error {
		// Stage a foreign index entry WITHOUT touching worktree bytes: an index-only
		// cacheinfo add of a blob that has no worktree file.
		blob := strings.TrimSpace(string(mustGit(t, r.g, r.rn.runWorktree(), nil, "hash-object", "-w", "--stdin")))
		mustGit(t, r.g, r.rn.runWorktree(), nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+",foreign-staged.txt")
		return nil
	}}
	_, err := r.rn.Submit(withHooks(context.Background(), hooks), r.lead, r.implRaw)
	if !errors.Is(err, gitx.ErrIndexCAS) {
		t.Fatalf("pre-prepare foreign-staged submit err = %v, want gitx.ErrIndexCAS", err)
	}
	if _, _, ok := r.journalRecord(t); ok {
		t.Fatal("a pre-prepare identity failure still wrote a journal record")
	}
	if got := r.branchOID(t); got != base {
		t.Fatalf("a pre-prepare identity failure moved the ref to %s", got)
	}
	if rs := cur(t, r.rn); rs.Revision != r.preRevision {
		t.Fatalf("a pre-prepare identity failure advanced the state to %d", rs.Revision)
	}
	staged := string(mustGit(t, r.g, r.rn.runWorktree(), nil, "diff", "--cached", "--name-only"))
	if !strings.Contains(staged, "foreign-staged.txt") {
		t.Fatalf("the foreign staged entry was not preserved: %q", staged)
	}
}

// A detached HEAD or a same-OID branch switch in the snapshot..PREPARE window must
// fail the submit closed with no journal/ref/index/state effect: OID equality alone is
// not the frozen identity — the pre-PREPARE proof re-proves the SYMBOLIC run branch.
func TestGitTxnSymbolicHeadDriftPrePrepareFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drift func(t *testing.T, r *gitTxnRig, parent string)
	}{
		{"detached at the same parent", func(t *testing.T, r *gitTxnRig, parent string) {
			// Detach without touching the index: write the worktree HEAD directly.
			mustGit(t, r.g, r.rn.runWorktree(), nil, "update-ref", "--no-deref", "HEAD", parent)
		}},
		{"another branch at the same parent", func(t *testing.T, r *gitTxnRig, parent string) {
			mustGit(t, r.g, r.repo, nil, "branch", "imposter", parent)
			mustGit(t, r.g, r.rn.runWorktree(), nil, "symbolic-ref", "HEAD", "refs/heads/imposter")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newGitTxnRig(t)
			parent := r.branchOID(t)
			hooks := &submitHooks{beforeGitJournal: func() error {
				tc.drift(t, r, parent)
				return nil
			}}
			_, err := r.rn.Submit(withHooks(context.Background(), hooks), r.lead, r.implRaw)
			if err == nil {
				t.Fatal("a symbolic-HEAD drift in the prepare window must fail the submit")
			}
			if _, _, ok := r.journalRecord(t); ok {
				t.Fatal("a symbolic-HEAD drift still wrote a journal record")
			}
			if got := r.branchOID(t); got != parent {
				t.Fatalf("a symbolic-HEAD drift still moved the run branch to %s", got)
			}
			if rs := cur(t, r.rn); rs.Revision != r.preRevision {
				t.Fatalf("a symbolic-HEAD drift still advanced the state to %d", rs.Revision)
			}
		})
	}
}

// Tampering with the txn-private target index in the snapshot..PREPARE window must
// fail the submit closed before the journal records the intent — never PREPARE a
// transaction whose frozen target bytes are already gone.
func TestGitTxnPrivateTargetTamperPrePrepareFailsClosed(t *testing.T) {
	r := newGitTxnRig(t)
	base := r.branchOID(t)
	hooks := &submitHooks{beforeGitJournal: func() error {
		matches, gerr := filepath.Glob(filepath.Join(r.repo, ".git", "worktrees", "*", "index.claudex-target-*"))
		if gerr != nil || len(matches) != 1 {
			t.Fatalf("locate private target: %v (%d matches)", gerr, len(matches))
		}
		if werr := os.WriteFile(matches[0], []byte("tampered"), 0o600); werr != nil {
			t.Fatalf("tamper private target: %v", werr)
		}
		return nil
	}}
	_, err := r.rn.Submit(withHooks(context.Background(), hooks), r.lead, r.implRaw)
	if !errors.Is(err, gitx.ErrIndexCAS) {
		t.Fatalf("private-target tamper err = %v, want gitx.ErrIndexCAS", err)
	}
	if _, _, ok := r.journalRecord(t); ok {
		t.Fatal("a private-target tamper still wrote a journal record")
	}
	if got := r.branchOID(t); got != base {
		t.Fatalf("a private-target tamper still moved the ref to %s", got)
	}
	if rs := cur(t, r.rn); rs.Revision != r.preRevision {
		t.Fatalf("a private-target tamper still advanced the state to %d", rs.Revision)
	}
}

// A txn-private name swapped for a symlink to byte-identical target contents in the
// snapshot..PREPARE window must fail the submit closed with no journal/effect: the
// pre-PREPARE proof binds to the regular object, never to bytes reached through a link.
func TestGitTxnPrivateSymlinkPrePrepareFailsClosed(t *testing.T) {
	r := newGitTxnRig(t)
	base := r.branchOID(t)
	skipped := false
	hooks := &submitHooks{beforeGitJournal: func() error {
		matches, gerr := filepath.Glob(filepath.Join(r.repo, ".git", "worktrees", "*", "index.claudex-target-*"))
		if gerr != nil || len(matches) != 1 {
			t.Fatalf("locate private target: %v (%d matches)", gerr, len(matches))
		}
		private := matches[0]
		b, rerr := os.ReadFile(private)
		if rerr != nil {
			t.Fatalf("read private: %v", rerr)
		}
		copyPath := private + ".copy"
		if werr := os.WriteFile(copyPath, b, 0o600); werr != nil {
			t.Fatalf("write copy: %v", werr)
		}
		if rerr := os.Remove(private); rerr != nil {
			t.Fatalf("remove private: %v", rerr)
		}
		if serr := os.Symlink(copyPath, private); serr != nil {
			skipped = true
			// Restore so the submit proceeds; the host cannot express the attack.
			if werr := os.WriteFile(private, b, 0o600); werr != nil {
				t.Fatalf("restore private: %v", werr)
			}
		}
		return nil
	}}
	_, err := r.rn.Submit(withHooks(context.Background(), hooks), r.lead, r.implRaw)
	if skipped {
		t.Skip("symlinks unavailable on this host")
	}
	if !errors.Is(err, gitx.ErrIndexCAS) {
		t.Fatalf("symlinked-private submit err = %v, want gitx.ErrIndexCAS", err)
	}
	if _, _, ok := r.journalRecord(t); ok {
		t.Fatal("a symlinked private still wrote a journal record")
	}
	if got := r.branchOID(t); got != base {
		t.Fatalf("a symlinked private still moved the ref to %s", got)
	}
	if rs := cur(t, r.rn); rs.Revision != r.preRevision {
		t.Fatalf("a symlinked private still advanced the state to %d", rs.Revision)
	}
}

// A failed durable re-confirmation of the frozen evidence artifact halts recovery
// BEFORE any effect: pin B requires re-read AND re-confirm, so a visible-but-unproven
// artifact is never trusted. Healing the barrier lets the same transaction recover.
func TestGitTxnArtifactConfirmBarrierFailure(t *testing.T) {
	r := newGitTxnRig(t)
	base := r.branchOID(t)
	if _, err := r.rn.Submit(withHooks(context.Background(), oneShotApply("ref-cas")), r.lead, r.implRaw); !errors.Is(err, errCut) {
		t.Fatalf("cut submit err = %v, want the injected cut", err)
	}
	barrierErr := errors.New("artifact barrier fails")
	r.rn.store.WithSyncInRoot(func(*os.Root, string) error { return barrierErr })

	if _, err := r.rn.Submit(context.Background(), r.lead, r.implRaw); !errors.Is(err, barrierErr) {
		t.Fatalf("recovery with a failing artifact barrier err = %v, want the barrier failure", err)
	}
	if got := r.branchOID(t); got != base {
		t.Fatalf("a failed artifact barrier still moved the ref to %s", got)
	}
	if rec, _, ok := r.journalRecord(t); !ok || rec.Terminal() || rec.StepsDone != 0 {
		t.Fatalf("a failed artifact barrier still recorded progress: %+v", rec)
	}

	r.rn.store.WithSyncInRoot(atomicfile.SyncInRoot)
	res, err := r.rn.Submit(context.Background(), r.lead, r.implRaw)
	if err != nil {
		t.Fatalf("healed recovery: %v", err)
	}
	r.assertRecovered(t, res)
}

// After PREPARE, a missing or corrupted frozen evidence artifact fails recovery closed
// BEFORE any ref/index/state effect: a transaction whose artifact vanished can never
// become an accepted receipt.
func TestGitTxnMissingArtifactRecoveryFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, path string)
	}{
		{"missing", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove artifact: %v", err)
			}
		}},
		{"corrupt", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"tampered":true}`), 0o600); err != nil {
				t.Fatalf("corrupt artifact: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newGitTxnRig(t)
			base := r.branchOID(t)
			if _, err := r.rn.Submit(withHooks(context.Background(), oneShotApply("ref-cas")), r.lead, r.implRaw); !errors.Is(err, errCut) {
				t.Fatalf("cut submit err = %v, want the injected cut", err)
			}
			_, plan, ok := r.journalRecord(t)
			if !ok {
				t.Fatal("no pending transaction after the cut")
			}
			tc.corrupt(t, filepath.Join(r.rn.loc.ArtifactsDir, plan.TurnID, plan.Digest+".json"))

			_, err := r.rn.Submit(context.Background(), r.lead, r.implRaw)
			if err == nil || !strings.Contains(err.Error(), "missing or corrupt") {
				t.Fatalf("recovery with a %s artifact err = %v, want the missing-or-corrupt refusal", tc.name, err)
			}
			if got := r.branchOID(t); got != base {
				t.Fatalf("recovery with a %s artifact moved the ref to %s", tc.name, got)
			}
			if rs := cur(t, r.rn); rs.Revision != r.preRevision {
				t.Fatalf("recovery with a %s artifact advanced the state to %d", tc.name, rs.Revision)
			}
			if rec, _, ok := r.journalRecord(t); !ok || rec.Terminal() || rec.StepsDone != 0 {
				t.Fatalf("recovery with a %s artifact recorded progress: %+v", tc.name, rec)
			}
		})
	}
}

// Deleting the commit-txn journal after an accepted git tuple is corruption, not a
// fresh run: the open handle fails every submit recovery-required, and a reopen
// refuses the run.
func TestGitTxnJournalVanishedAfterEvidence(t *testing.T) {
	r := newGitTxnRig(t)
	if _, err := r.rn.Submit(context.Background(), r.lead, r.implRaw); err != nil {
		t.Fatalf("implement transaction: %v", err)
	}
	if err := os.RemoveAll(r.rn.loc.CommitTxnDir); err != nil {
		t.Fatalf("delete commit journal: %v", err)
	}
	// The open run: the aggregate journal seam fails the next submit closed.
	rs := cur(t, r.rn)
	if _, err := r.rn.Submit(context.Background(), r.pair, checkpointArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", true, nil)); !errors.Is(err, transport.ErrRecoveryRequired) {
		t.Fatalf("submit after journal deletion err = %v, want ErrRecoveryRequired", err)
	}
	// A reopen refuses the run outright.
	if _, err := OpenRun(r.repo, r.runID, rand.Reader); !errors.Is(err, transport.ErrRecoveryRequired) {
		t.Fatalf("reopen after journal deletion err = %v, want ErrRecoveryRequired", err)
	}
	// The replacement mutator refuses the same corruption, both stores untouched.
	regBefore, ok, err := r.rn.registry.Load()
	if err != nil || !ok {
		t.Fatalf("registry load: ok=%v err=%v", ok, err)
	}
	stateBefore := cur(t, r.rn)
	_, rerr := attach.ReplaceAttach(attach.ReplaceRequest{
		RepoDir: r.repo, RunID: r.runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: opID("d"), RNG: rand.Reader,
	})
	if !errors.Is(rerr, attach.ErrReplaceRecoveryRequired) {
		t.Fatalf("replace after journal deletion err = %v, want ErrReplaceRecoveryRequired", rerr)
	}
	if regAfter, _, _ := r.rn.registry.Load(); regAfter.Revision != regBefore.Revision {
		t.Fatalf("a refused replacement still mutated the registry (%d -> %d)", regBefore.Revision, regAfter.Revision)
	}
	if stateAfter := cur(t, r.rn); stateAfter.Revision != stateBefore.Revision {
		t.Fatalf("a refused replacement still mutated the run state (%d -> %d)", stateBefore.Revision, stateAfter.Revision)
	}
}

// A pending commit transaction blocks a session replacement outright, with the
// Registry and RunState untouched.
func TestGitTxnPendingBlocksReplaceAttach(t *testing.T) {
	r := newGitTxnRig(t)
	if _, err := r.rn.Submit(withHooks(context.Background(), oneShotApply("index-cas")), r.lead, r.implRaw); !errors.Is(err, errCut) {
		t.Fatalf("cut submit err = %v, want the injected cut", err)
	}
	regBefore, ok, err := r.rn.registry.Load()
	if err != nil || !ok {
		t.Fatalf("registry load: ok=%v err=%v", ok, err)
	}
	stateBefore := cur(t, r.rn)

	_, rerr := attach.ReplaceAttach(attach.ReplaceRequest{
		RepoDir: r.repo, RunID: r.runID, Role: state.SlotPair, Agent: state.AgentCodex,
		ExpectedGeneration: 1, OperationID: opID("c"), RNG: rand.Reader,
	})
	if !errors.Is(rerr, attach.ErrReplaceRecoveryRequired) {
		t.Fatalf("replace with a pending commit txn err = %v, want ErrReplaceRecoveryRequired", rerr)
	}
	if regAfter, _, _ := r.rn.registry.Load(); regAfter.Revision != regBefore.Revision {
		t.Fatalf("a blocked replacement still mutated the registry (%d -> %d)", regBefore.Revision, regAfter.Revision)
	}
	if stateAfter := cur(t, r.rn); stateAfter.Revision != stateBefore.Revision {
		t.Fatalf("a blocked replacement still mutated the run state (%d -> %d)", stateBefore.Revision, stateAfter.Revision)
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
