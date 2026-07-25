package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/gitx"
)

// The packet materializes bytes from the COMMITTED object and is byte-stable regardless of later
// worktree edits — the core 04.3 invariant.
func TestReadsFromCommitNotWorktree(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	const committed = "COMMITTED CONTENT\n"
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte(committed), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "file.txt")
	git("commit", "-q", "-m", "c1")
	blobOID := git("rev-parse", "HEAD:file.txt")
	commitOID := git("rev-parse", "HEAD")
	treeOID := git("rev-parse", "HEAD^{tree}")

	// Edit the worktree AFTER the commit — the packet must never see this.
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("WORKTREE EDIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := gitx.New()
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	defer g.Close()

	evDir := t.TempDir()
	r := Recipe{
		RunID: "run-1", TurnID: "turn-1", Phase: "PLAN_DRAFT",
		Source:  SourceObject{Commit: commitOID, Tree: treeOID},
		Entries: []RecipeEntry{{GitPath: "file.txt", Mode: "100644", Kind: EntryFile, Source: BlobSource{CommitBlobOID: blobOID}}},
		Bounds:  testBounds,
	}
	ref, err := Produce(context.Background(), evDir, r, NewGitObjectReader(g, repo))
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if err := VerifyRef(evDir, ref, testBounds, r.Expectation()); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// The packet blob equals the COMMITTED bytes, addressed by their sha256 — not the worktree edit.
	sum := sha256.Sum256([]byte(committed))
	got, err := os.ReadFile(filepath.Join(evDir, "turn-1", hex.EncodeToString(sum[:])))
	if err != nil {
		t.Fatalf("read packet blob: %v", err)
	}
	if string(got) != committed {
		t.Fatalf("packet blob = %q, want the committed content %q (not the worktree edit)", got, committed)
	}
}
