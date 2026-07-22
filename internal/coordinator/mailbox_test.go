package coordinator

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
)

// MirrorMailbox serializes every writer of the shared repo-level mirror on the REPOSITORY lock
// (not a per-run lock) and loads the ledger inside that boundary. This proves both halves: the
// repo lock is HELD across the load->write window (a concurrent acquire fails while a mirror is
// in flight), and each rebuild reflects the LATEST ledger (a later submit's mirror strictly
// grows).
func TestMirrorMailboxSerializedAndLatest(t *testing.T) {
	t.Cleanup(func() { mirrorPostLoadHook = nil })
	repo := t.TempDir()
	runID, lead, pair := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	mailbox := filepath.Join(repo, ".claudex", "mailbox.md")

	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))
	if err := rn.MirrorMailbox(); err != nil {
		t.Fatalf("mirror 1: %v", err)
	}
	m1, err := os.ReadFile(mailbox)
	if err != nil || len(m1) == 0 {
		t.Fatalf("mailbox after first mirror: %d bytes err=%v", len(m1), err)
	}

	// The load->write window is guarded on the REPO lock: a concurrent acquire fails in flight.
	held := false
	mirrorPostLoadHook = func() {
		if g, ok, aerr := genstore.Acquire(attach.RepoLock(repo)); aerr == nil && ok {
			_ = g.Release()
			t.Error("MirrorMailbox did not hold the repo lock across load->write")
		} else {
			held = true
		}
	}
	if err := rn.MirrorMailbox(); err != nil {
		t.Fatalf("mirror 2 (guarded): %v", err)
	}
	mirrorPostLoadHook = nil
	if !held {
		t.Fatal("the post-load hook never observed the held repo lock")
	}

	// Each rebuild loads the LATEST ledger: a second accepted turn makes the mirror strictly grow.
	rs = cur(t, rn)
	submitOK(t, rn, pair, critiqueArtifact(t, rs.Assignment.ID, rs.Revision, "AGREE", false, nil, nil))
	if err := rn.MirrorMailbox(); err != nil {
		t.Fatalf("mirror 3: %v", err)
	}
	m2, err := os.ReadFile(mailbox)
	if err != nil {
		t.Fatalf("mailbox after third mirror: %v", err)
	}
	if len(m2) <= len(m1) {
		t.Fatalf("mirror did not reflect the later ledger: %d -> %d bytes", len(m1), len(m2))
	}
}

// A mirror binds to the CURRENT active-run pointer under the repo lock: once run A is no longer
// the active run (superseded at an active-run switch), a delayed A.MirrorMailbox is a no-op and
// can NEVER overwrite the projection a newer run wrote. This is the active-run-switch race the
// per-run lock did not serialize.
func TestMirrorMailboxActiveRunBind(t *testing.T) {
	repo := t.TempDir()
	runID, lead, _ := newPairedRun(t, repo)
	rn, err := OpenRun(repo, runID, rand.Reader)
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	defer rn.Close()
	mailbox := filepath.Join(repo, ".claudex", "mailbox.md")

	// A accepts a turn and mirrors its transcript.
	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))
	if err := rn.MirrorMailbox(); err != nil {
		t.Fatalf("A mirror: %v", err)
	}

	// Simulate the active-run SWITCH: a newer run took over (clear A from the active pointer) and
	// wrote its own projection to the shared mailbox.
	repoLock := attach.RepoLock(repo)
	activeDir := filepath.Join(repo, ".claudex", "active-run")
	pointer := state.OpenCurrentRun(activeDir, repoLock)
	g, ok, aerr := genstore.Acquire(repoLock)
	if aerr != nil || !ok {
		t.Fatalf("acquire repo lock: ok=%v err=%v", ok, aerr)
	}
	curPtr, ptrOK, perr := pointer.Load()
	if perr != nil || !ptrOK {
		t.Fatalf("load active pointer: ok=%v err=%v", ptrOK, perr)
	}
	if _, err := pointer.Clear(g, curPtr.Revision, curPtr.RunID); err != nil {
		t.Fatalf("clear active pointer: %v", err)
	}
	_ = g.Release()

	newerProjection := "NEWER RUN B PROJECTION\n"
	if err := os.WriteFile(mailbox, []byte(newerProjection), 0o600); err != nil {
		t.Fatalf("write newer projection: %v", err)
	}

	// A's delayed mirror must be a no-op — A is no longer the active run.
	if err := rn.MirrorMailbox(); err != nil {
		t.Fatalf("A delayed mirror (should be a no-op): %v", err)
	}
	md, err := os.ReadFile(mailbox)
	if err != nil {
		t.Fatalf("read mailbox: %v", err)
	}
	if string(md) != newerProjection {
		t.Fatalf("a superseded run's delayed mirror overwrote the newer projection: %q", md)
	}
}
