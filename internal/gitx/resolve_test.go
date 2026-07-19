package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oidOf resolves a revision to its exact OID via the same hardened handle, for cross-checking.
func oidOf(t *testing.T, g *Git, repo, rev string) string {
	t.Helper()
	return strings.TrimSpace(string(mustRun(t, g, repo, nil, "rev-parse", "--verify", rev)))
}

// commitFile adds a file and commits it, returning the new HEAD OID.
func commitFile(t *testing.T, g *Git, repo, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	mustRun(t, g, repo, commitEnv(), "add", name)
	mustRun(t, g, repo, commitEnv(), "commit", "-m", "c-"+name)
	return oidOf(t, g, repo, "HEAD")
}

func TestResolveBaseHappyPath(t *testing.T) {
	repo, g := initRepo(t)
	want := oidOf(t, g, repo, "refs/heads/main")
	got, err := NewBaseResolver(g).ResolveBase(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}

// A base branch that does not exist fails closed rather than resolving anything.
func TestResolveBaseMissingBranchFailsClosed(t *testing.T) {
	repo, g := initRepo(t)
	if _, err := NewBaseResolver(g).ResolveBase(context.Background(), repo, "nonesuch"); err == nil {
		t.Fatal("resolving a missing branch should fail")
	}
}

// A tag whose name matches the requested base, with NO branch of that name, is not resolved:
// the resolver is pinned to refs/heads/, so only a real local branch anchors a run.
func TestResolveBaseIgnoresSameNamedTag(t *testing.T) {
	repo, g := initRepo(t)
	mustRun(t, g, repo, commitEnv(), "tag", "onlytag") // a tag, but no branch "onlytag"
	if _, err := NewBaseResolver(g).ResolveBase(context.Background(), repo, "onlytag"); err == nil {
		t.Fatal("a tag-only name must not resolve as a base branch")
	}
}

// When a branch and a tag share a name but point at different commits, the resolver returns the
// BRANCH commit — refs/heads/<name> disambiguates to the local branch, never the tag.
func TestResolveBasePrefersBranchOverTag(t *testing.T) {
	repo, g := initRepo(t)
	c1 := oidOf(t, g, repo, "HEAD")
	c2 := commitFile(t, g, repo, "b.txt", "second")
	if c1 == c2 {
		t.Fatal("expected two distinct commits")
	}
	mustRun(t, g, repo, commitEnv(), "branch", "dev", c1) // branch dev -> c1
	mustRun(t, g, repo, commitEnv(), "tag", "dev", c2)    // tag dev   -> c2
	got, err := NewBaseResolver(g).ResolveBase(context.Background(), repo, "dev")
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	if got != c1 {
		t.Fatalf("resolved %q, want the branch commit %q (not the tag %q)", got, c1, c2)
	}
}

// A name that would traverse out of the refs/heads/ namespace is rejected BEFORE git runs, so a
// crafted base cannot reach, say, a tag via refs/heads/../tags/... normalization.
func TestResolveBaseRejectsNamespaceEscape(t *testing.T) {
	repo, g := initRepo(t)
	mustRun(t, g, repo, commitEnv(), "tag", "escaped")
	_, err := NewBaseResolver(g).ResolveBase(context.Background(), repo, "../../refs/tags/escaped")
	if !errors.Is(err, ErrBranchName) {
		t.Fatalf("escape attempt err = %v, want ErrBranchName", err)
	}
}

func TestResolveBaseContextCancelled(t *testing.T) {
	repo, g := initRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewBaseResolver(g).ResolveBase(ctx, repo, "main"); err == nil {
		t.Fatal("a cancelled context should fail the resolve")
	}
}

func TestValidateBranchName(t *testing.T) {
	valid := []string{"main", "feature/x", "release-1.2", "a_b", "user/topic/sub", "v1.2.3"}
	for _, n := range valid {
		if err := validateBranchName(n); err != nil {
			t.Errorf("validateBranchName(%q) = %v, want nil", n, err)
		}
	}
	invalid := []string{
		"", "-x", "--force", "a..b", "a/", "/a", "a//b", ".hidden", "feature/.x",
		"x.lock", "feature/y.lock", "a b", "a~", "a^", "a:", "a?", "a*", "a[", "a\\",
		"@", "a@{b", "end.", "..",
	}
	for _, n := range invalid {
		if err := validateBranchName(n); !errors.Is(err, ErrBranchName) {
			t.Errorf("validateBranchName(%q) = %v, want ErrBranchName", n, err)
		}
	}
}
