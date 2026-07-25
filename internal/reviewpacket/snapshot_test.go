package reviewpacket

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
)

// SnapshotRef is "path plus the EXACT digest of the bytes", so the digest is the authority. Without
// checking it, swapping a task snapshot for different but still-valid v2 JSON would change the
// declared selectors — and therefore the packet — while immutable run state still named the old
// digest, producing a packet bound to a task the run never agreed to.
func TestReadSnapshotRequiresTheFrozenDigest(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"schema_version":2,"goal":"g"}`)
	if err := os.WriteFile(filepath.Join(dir, "task.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	ref := state.SnapshotRef{RelPath: "task.json", Digest: config.Hash(body)}

	got, err := readSnapshot(dir, ref, "task")
	if err != nil {
		t.Fatalf("matching digest rejected: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("bytes = %q, want %q", got, body)
	}

	// Tampered content at the same path: still valid JSON, different bytes.
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(`{"schema_version":2,"goal":"h"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSnapshot(dir, ref, "task"); err == nil || !strings.Contains(err.Error(), "does not match the digest") {
		t.Fatalf("tampered snapshot err = %v, want a digest refusal", err)
	}

	// A malformed frozen digest is refused before anything is read.
	if _, err := readSnapshot(dir, state.SnapshotRef{RelPath: "task.json", Digest: "nope"}, "task"); err == nil {
		t.Fatal("a non-sha256 frozen digest must be refused")
	}
}

// The read is regular-only: a rooted Open would follow an in-root symlink to other content, and a
// FIFO could block the resolution indefinitely.
func TestReadSnapshotRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"schema_version":2}`)
	if err := os.WriteFile(filepath.Join(dir, "real.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real.json"), filepath.Join(dir, "link.json")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("this host cannot create symlinks")
		}
		t.Fatal(err)
	}
	// Even though the target's bytes hash correctly, a symlink is not a regular file.
	ref := state.SnapshotRef{RelPath: "link.json", Digest: config.Hash(body)}
	if _, err := readSnapshot(dir, ref, "task"); err == nil {
		t.Fatal("a symlinked snapshot must be refused")
	}

	// A directory in the snapshot's place is equally not a snapshot.
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readSnapshot(dir, state.SnapshotRef{RelPath: "adir", Digest: config.Hash(body)}, "task"); err == nil {
		t.Fatal("a directory snapshot must be refused")
	}
}
