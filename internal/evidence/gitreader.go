package evidence

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/David-c0degeek/claudex/internal/gitx"
)

// gitReader is an ObjectReader backed by native `git cat-file` scoped to a repository. It reads blob
// objects from committed history via their object id — never the live worktree.
type gitReader struct {
	g       *gitx.Git
	repoDir string
}

// NewGitObjectReader returns an ObjectReader that reads blobs from repoDir's object store.
func NewGitObjectReader(g *gitx.Git, repoDir string) ObjectReader {
	return gitReader{g: g, repoDir: repoDir}
}

// BlobSize returns the object's size via `git cat-file -s`.
func (r gitReader) BlobSize(ctx context.Context, oid string) (int64, error) {
	if !isOID(oid) {
		return 0, fmt.Errorf("%w: %q is not a git object id", ErrRecipe, oid)
	}
	out, err := r.g.Run(ctx, r.repoDir, nil, "cat-file", "-s", oid)
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("evidence: cat-file -s %s: %v", oid, perr)
	}
	return n, nil
}

// Blob returns the blob content, first confirming the object type is exactly blob (never a tree or
// commit), via `git cat-file`.
func (r gitReader) Blob(ctx context.Context, oid string) ([]byte, error) {
	if !isOID(oid) {
		return nil, fmt.Errorf("%w: %q is not a git object id", ErrRecipe, oid)
	}
	t, err := r.g.Run(ctx, r.repoDir, nil, "cat-file", "-t", oid)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(t)) != "blob" {
		return nil, fmt.Errorf("%w: object %s is not a blob", ErrCorrupt, oid)
	}
	// RunRaw (not Run): blob content is verbatim bytes; Run would strip a trailing newline.
	return r.g.RunRaw(ctx, r.repoDir, nil, "cat-file", "blob", oid)
}
