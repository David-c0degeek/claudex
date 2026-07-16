package txn

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

type fakeParticipant struct {
	committed  bool
	observeErr error
}

func (f *fakeParticipant) Commit(intent []byte) error          { return nil }
func (f *fakeParticipant) Observe(intent []byte) (bool, error) { return f.committed, f.observeErr }

func newJournal(t *testing.T) (*Journal, string) {
	t.Helper()
	dir := t.TempDir()
	lock := filepath.Join(dir, "run.lock")
	return Open(filepath.Join(dir, "txn"), lock), lock
}

func withGuard(t *testing.T, lock string, fn func(g *genstore.Guard)) {
	t.Helper()
	g, ok, err := genstore.Acquire(lock)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	fn(g)
}

func TestPrepareThenCommit(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		p, err := j.PrepareLocked(g, "txn-1", []byte("ref oldOID->newOID"))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if p.Phase != Prepared || p.Revision != 1 {
			t.Fatalf("prepared = %+v", p)
		}
		c, err := j.MarkCommittedLocked(g, "txn-1")
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		if c.Phase != Committed || c.Revision != 2 || !c.Terminal() {
			t.Fatalf("committed = %+v", c)
		}
	})
	got, ok, err := j.Latest()
	if err != nil || !ok || got.Phase != Committed {
		t.Fatalf("latest = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestPrepareWhilePendingRejected(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.PrepareLocked(g, "txn-1", []byte("a")); err != nil {
			t.Fatalf("prepare 1: %v", err)
		}
		if _, err := j.PrepareLocked(g, "txn-2", []byte("b")); !errors.Is(err, ErrPending) {
			t.Fatalf("second prepare err = %v, want ErrPending", err)
		}
	})
}

func TestMarkWrongTxnRejected(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		j.PrepareLocked(g, "txn-1", []byte("a"))
		if _, err := j.MarkCommittedLocked(g, "txn-2"); !errors.Is(err, ErrNoPending) {
			t.Fatalf("mark wrong txn err = %v, want ErrNoPending", err)
		}
	})
}

func TestReconcileForwardOnObservedCommit(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		j.PrepareLocked(g, "txn-1", []byte("intent"))
		out, acted, err := j.ReconcileLocked(g, &fakeParticipant{committed: true})
		if err != nil || !acted {
			t.Fatalf("reconcile: acted=%v err=%v", acted, err)
		}
		if out.Phase != Committed {
			t.Fatalf("reconcile forward = %+v, want committed", out)
		}
	})
}

func TestReconcileRollbackOnUnobservedCommit(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		j.PrepareLocked(g, "txn-1", []byte("intent"))
		out, acted, err := j.ReconcileLocked(g, &fakeParticipant{committed: false})
		if err != nil || !acted {
			t.Fatalf("reconcile: acted=%v err=%v", acted, err)
		}
		if out.Phase != Aborted {
			t.Fatalf("reconcile rollback = %+v, want aborted", out)
		}
	})
}

func TestReconcileIdempotentWhenTerminal(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		j.PrepareLocked(g, "txn-1", []byte("intent"))
		j.MarkCommittedLocked(g, "txn-1")
		if _, acted, err := j.ReconcileLocked(g, &fakeParticipant{committed: true}); err != nil || acted {
			t.Fatalf("reconcile on terminal: acted=%v err=%v, want no action", acted, err)
		}
	})
}

func TestReconcileAcrossRestart(t *testing.T) {
	// A prepared record persists; a fresh Journal (simulated restart) reconciles it.
	dir := t.TempDir()
	lock := filepath.Join(dir, "run.lock")
	jdir := filepath.Join(dir, "txn")

	withGuard(t, lock, func(g *genstore.Guard) {
		j := Open(jdir, lock)
		if _, err := j.PrepareLocked(g, "txn-1", []byte("intent")); err != nil {
			t.Fatalf("prepare: %v", err)
		}
	})

	// New process/handle over the same directory.
	j2 := Open(jdir, lock)
	if cur, ok, _ := j2.Latest(); !ok || cur.Phase != Prepared {
		t.Fatalf("prepared record did not persist: %+v ok=%v", cur, ok)
	}
	withGuard(t, lock, func(g *genstore.Guard) {
		out, acted, err := j2.ReconcileLocked(g, &fakeParticipant{committed: true})
		if err != nil || !acted || out.Phase != Committed {
			t.Fatalf("restart reconcile = %+v acted=%v err=%v", out, acted, err)
		}
	})
}

func TestSecretInIntentRejected(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		if _, err := j.PrepareLocked(g, "txn-1", []byte("token=sk-ant-abcdefghijklmnopqrstuvwx")); err == nil {
			t.Fatalf("a secret in the intent should be rejected")
		}
	})
}

func TestNewTransactionAfterTerminal(t *testing.T) {
	j, lock := newJournal(t)
	withGuard(t, lock, func(g *genstore.Guard) {
		j.PrepareLocked(g, "txn-1", []byte("a"))
		j.MarkAbortedLocked(g, "txn-1")
		p2, err := j.PrepareLocked(g, "txn-2", []byte("b"))
		if err != nil {
			t.Fatalf("prepare after terminal: %v", err)
		}
		if p2.TxnID != "txn-2" || p2.Phase != Prepared {
			t.Fatalf("new txn = %+v", p2)
		}
	})
}
