package reviewpacket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/state"
)

// The packet materializes bytes from the COMMITTED object and is byte-stable regardless of later
// worktree edits — the core review-evidence invariant.
var testBounds = evidence.Bounds{MaxTotalBytes: 262144, MaxFileBytes: 98304, MaxRequests: 8}

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
	r := evidence.Recipe{
		RunID: "run-1", TurnID: "turn-1", Phase: "PLAN_DRAFT",
		Source:  evidence.SourceObject{Commit: commitOID, Tree: treeOID},
		Entries: []evidence.RecipeEntry{{GitPath: "file.txt", Mode: "100644", Kind: evidence.EntryFile, Source: evidence.BlobSource{CommitBlobOID: blobOID}}},
		Bounds:  testBounds,
	}
	ref, err := evidence.Produce(context.Background(), evDir, r, NewGitObjectReader(g, repo))
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if err := evidence.VerifyRef(evDir, ref, testBounds, r.Expectation()); err != nil {
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

// The packet binds the COMPLETE source identity, so {commit, tree} must be a PROVEN pair, not two
// independently well-formed ids: content is read from the commit while the tree is what the manifest
// records, so an unchecked pair could publish bytes from one commit while binding another's tree.
func TestResolveAtRejectsAnInconsistentSourcePair(t *testing.T) {
	repo := t.TempDir()
	g := gitEnv(t, repo)

	commitA, treeA := commitFile(t, g, repo, "a.txt", "first\n")
	_, treeB := commitFile(t, g, repo, "b.txt", "second\n")
	if treeA == treeB {
		t.Fatal("the two commits must have different trees for this test to mean anything")
	}

	d := Deps{RunDir: t.TempDir(), RepoDir: repo, Git: g}
	// The matched pair proves.
	if err := proveSourcePair(context.Background(), d, evidence.SourceObject{Commit: commitA, Tree: treeA}); err != nil {
		t.Fatalf("matched pair rejected: %v", err)
	}
	// Commit A with commit B's tree: both ids exist and are well-formed, but they are not a pair.
	err := proveSourcePair(context.Background(), d, evidence.SourceObject{Commit: commitA, Tree: treeB})
	if err == nil || !strings.Contains(err.Error(), "not the bound") {
		t.Fatalf("mismatched pair err = %v, want a pair refusal", err)
	}
	// A well-formed id that names no object fails closed too.
	if err := proveSourcePair(context.Background(), d, evidence.SourceObject{Commit: strings.Repeat("0", 40), Tree: treeA}); err == nil {
		t.Fatal("an unknown commit must fail closed")
	}
}

// gitEnv opens a gitx handle on a fresh repository.
func gitEnv(t *testing.T, repo string) *gitx.Git {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	g, err := gitx.New()
	if err != nil {
		t.Fatalf("gitx.New: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

// commitFile writes a file, commits it, and returns the resulting {commit, tree}.
func commitFile(t *testing.T, g *gitx.Git, repo, name, body string) (commit, tree string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", name}, {"commit", "-q", "-m", "c-" + name}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	ctx := context.Background()
	c, err := g.Run(ctx, repo, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	tr, err := g.Run(ctx, repo, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatalf("rev-parse tree: %v", err)
	}
	return string(c), string(tr)
}

// acceptedRun is a run state whose latest ACCEPTED turn carries the given git tuple.
func acceptedRun(commit, tree string) state.RunState {
	return state.RunState{
		RunID: "run-1",
		AcceptedTurns: map[string]state.AcceptedTurn{
			"turn-1": {
				ArtifactDigest: strings.Repeat("d", 64),
				Receipt:        state.Receipt{TurnID: "turn-1", Revision: 2, ArtifactDigest: strings.Repeat("d", 64)},
				Phase:          state.PhaseImplementStep,
				GitCommit:      &state.GitCommitEvidence{Parent: strings.Repeat("0", 40), Tree: tree, Commit: commit},
			},
		},
	}
}

// The accepted tuple is PERSISTED STATE, and persisted state is not evidence that the objects still
// exist and still form a pair. pull re-derives its expectation through ExpectedSource before
// verifying a bound packet, so ExpectedSource must PROVE the pair — otherwise a packet could be
// accepted while the source it asserts has gone missing or become inconsistent in the object store,
// which evidence.Expectation cannot catch (it checks only object-id grammar).
func TestExpectedSourceProvesTheAcceptedPair(t *testing.T) {
	repo := t.TempDir()
	g := gitEnv(t, repo)
	commitA, treeA := commitFile(t, g, repo, "a.txt", "first\n")
	_, treeB := commitFile(t, g, repo, "b.txt", "second\n")
	d := Deps{RunDir: t.TempDir(), RepoDir: repo, Git: g}
	ctx := context.Background()

	// The real accepted tuple proves.
	got, err := ExpectedSource(ctx, d, acceptedRun(commitA, treeA))
	if err != nil {
		t.Fatalf("a consistent accepted tuple was rejected: %v", err)
	}
	if got.Commit != commitA || got.Tree != treeA {
		t.Fatalf("source = %+v, want {%s, %s}", got, commitA, treeA)
	}

	// A tuple whose tree belongs to a DIFFERENT commit: both ids exist and are well-formed, so only
	// an object-store proof can reject it.
	if _, err := ExpectedSource(ctx, d, acceptedRun(commitA, treeB)); err == nil {
		t.Fatal("an inconsistent accepted {commit, tree} must fail closed")
	}
	// A tuple naming an object that is not in the store at all.
	if _, err := ExpectedSource(ctx, d, acceptedRun(strings.Repeat("1", 40), treeA)); err == nil {
		t.Fatal("an accepted commit missing from the object store must fail closed")
	}
	// The pre-implementation path is proven too: a base commit the store does not have fails closed.
	if _, err := ExpectedSource(ctx, d, state.RunState{RunID: "run-1", BaseCommit: strings.Repeat("2", 40)}); err == nil {
		t.Fatal("an unknown base commit must fail closed")
	}
}
