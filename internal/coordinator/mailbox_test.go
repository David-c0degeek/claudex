package coordinator

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

// MirrorMailbox serializes every writer of the repo-level mirror on the run lock and loads the
// ledger INSIDE that boundary, so a slow writer can never atomically replace the transcript last
// with a render older than a concurrent submit already published. This proves both halves: the
// run lock is HELD across the load->write window (a concurrent acquire fails while a mirror is in
// flight), and each rebuild reflects the LATEST ledger (a later submit's mirror strictly grows).
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

	// Accept a first turn (a plan), then mirror.
	rs := cur(t, rn)
	submitOK(t, rn, lead, planArtifact(t, rs.Assignment.ID, rs.Revision, false))
	if err := rn.MirrorMailbox(); err != nil {
		t.Fatalf("mirror 1: %v", err)
	}
	m1, err := os.ReadFile(mailbox)
	if err != nil || len(m1) == 0 {
		t.Fatalf("mailbox after first mirror: %d bytes err=%v", len(m1), err)
	}

	// The load->write window is guarded: a concurrent acquire fails while a mirror is in flight.
	held := false
	mirrorPostLoadHook = func() {
		if g, ok, aerr := genstore.Acquire(rn.RunLock()); aerr == nil && ok {
			_ = g.Release()
			t.Error("MirrorMailbox did not hold the run lock across load->write")
		} else {
			held = true
		}
	}
	if err := rn.MirrorMailbox(); err != nil {
		t.Fatalf("mirror 2 (guarded): %v", err)
	}
	mirrorPostLoadHook = nil
	if !held {
		t.Fatal("the post-load hook never observed the held run lock")
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
