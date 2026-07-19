//go:build !windows

package attach

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// An ancestor symlink on the run path cannot redirect a snapshot write outside
// the repository: the rooted create refuses to traverse the symlink, so no file
// is written into the attacker-chosen outside directory.
func TestSnapshotAncestorSymlinkEscape(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	claudex := filepath.Join(repo, ".claudex")
	if err := os.MkdirAll(claudex, 0o700); err != nil {
		t.Fatalf("mkdir .claudex: %v", err)
	}
	// Point .claudex/runs at an outside directory.
	if err := os.Symlink(outside, filepath.Join(claudex, "runs")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
	if err == nil {
		t.Fatalf("bootstrap should fail when an ancestor of the run dir is a symlink")
	}
	// Nothing was written into the outside directory through the symlink.
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("bootstrap wrote %d entries into the outside directory via the symlink", len(entries))
	}
}
