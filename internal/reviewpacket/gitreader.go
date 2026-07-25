// Package reviewpacket resolves a run's review-evidence packet: it turns run state, the frozen task
// contract, and the repository's committed history into the fully-resolved evidence.Recipe for a
// turn, and supplies the Git-backed object reader that materializes it.
//
// It is deliberately separate from internal/evidence. That package is the packet AUTHORITY — it
// produces and verifies packets and knows nothing about runs, phases, or Git — and keeping it a leaf
// is what lets run state validate a persisted evidence binding without dragging a Git dependency
// into the state layer. Everything that needs to look at a run to decide WHAT belongs in a packet
// lives here.
package reviewpacket

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/gitx"
)

// gitReader is an evidence.ObjectReader backed by native `git cat-file` scoped to a repository. It
// reads blob objects from committed history via their object id — never the live worktree.
type gitReader struct {
	g       *gitx.Git
	repoDir string
}

// NewGitObjectReader returns an ObjectReader that reads blobs from repoDir's object store.
func NewGitObjectReader(g *gitx.Git, repoDir string) evidence.ObjectReader {
	return gitReader{g: g, repoDir: repoDir}
}

// BlobSize returns the object's size via `git cat-file -s`.
func (r gitReader) BlobSize(ctx context.Context, oid string) (int64, error) {
	if !evidence.IsObjectID(oid) {
		return 0, fmt.Errorf("%w: %q is not a git object id", evidence.ErrRecipe, oid)
	}
	out, err := r.g.Run(ctx, r.repoDir, nil, "cat-file", "-s", oid)
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("reviewpacket: cat-file -s %s: %v", oid, perr)
	}
	return n, nil
}

// Blob returns the blob content, first confirming the object type is exactly blob (never a tree or
// commit), via `git cat-file`.
func (r gitReader) Blob(ctx context.Context, oid string) ([]byte, error) {
	if !evidence.IsObjectID(oid) {
		return nil, fmt.Errorf("%w: %q is not a git object id", evidence.ErrRecipe, oid)
	}
	t, err := r.g.Run(ctx, r.repoDir, nil, "cat-file", "-t", oid)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(t)) != "blob" {
		return nil, fmt.Errorf("%w: object %s is not a blob", evidence.ErrCorrupt, oid)
	}
	// RunRaw (not Run): blob content is verbatim bytes; Run would strip a trailing newline.
	return r.g.RunRaw(ctx, r.repoDir, nil, "cat-file", "blob", oid)
}
