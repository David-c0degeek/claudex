package reviewpacket

import (
	"bytes"
	"context"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/evidence"
)

// changedEntries materializes what a range of history actually CHANGED: one entry per path that
// differs between parent and commit. This is the substance of a review turn — a reviewer is being
// asked about a change, not about the whole tree — and it is read from the committed objects, so it
// is byte-stable no matter what the lead edits afterwards.
//
// `diff-tree -r --raw -z --no-renames` is used deliberately:
//   - -r recurses, so every record names a leaf rather than a changed subtree;
//   - --raw exposes the destination blob id and both modes, so the payload is read from the object
//     store rather than re-derived from a textual patch;
//   - -z makes paths NUL-terminated, so a path carrying a newline or a byte git would otherwise
//     C-quote survives verbatim — the same losslessness the manifest demands;
//   - --no-renames keeps every record to exactly ONE path. A rename record carries two, and treating
//     a rename as an add plus a delete is both simpler to parse and more honest about what the
//     reviewer must look at: the new content, and the fact the old path is gone.
func changedEntries(ctx context.Context, d Deps, parent, commit string) ([]evidence.RecipeEntry, error) {
	out, err := d.Git.RunRaw(ctx, d.RepoDir, nil, "diff-tree", "-r", "--raw", "-z", "--no-renames", parent, commit)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the change range %s..%s: %v", ErrResolve, parent, commit, err)
	}
	return parseRawDiff(out)
}

// parseRawDiff decodes `diff-tree --raw -z` records. Each record is a metadata field terminated by
// NUL, then the path terminated by NUL:
//
//	:<srcmode> <dstmode> <srcsha> <dstsha> <status>\0<path>\0
//
// The stream is parsed EXHAUSTIVELY rather than best-effort: every byte must be accounted for. A
// change list that silently ignored a trailing fragment, or stopped at the first empty field, could
// drop a real change from the packet — and a packet missing a change is exactly the failure this
// whole subject exists to prevent. Anything that is not "empty output, or complete metadata/path
// pairs followed by exactly one terminator" fails closed.
func parseRawDiff(out []byte) ([]evidence.RecipeEntry, error) {
	if len(out) == 0 {
		return nil, nil // an empty range is legal: nothing changed
	}
	fields := bytes.Split(out, []byte{0})
	// A well-formed stream ends with a NUL, so Split leaves exactly one empty final field, and the
	// records before it come in pairs.
	if n := len(fields); n < 3 || len(fields[n-1]) != 0 || (n-1)%2 != 0 {
		return nil, fmt.Errorf("%w: the change stream is truncated or malformed", ErrResolve)
	}
	records := fields[:len(fields)-1]
	entries := make([]evidence.RecipeEntry, 0, len(records)/2)
	for i := 0; i < len(records); i += 2 {
		e, err := rawDiffEntry(records[i], records[i+1])
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func rawDiffEntry(meta, path []byte) (evidence.RecipeEntry, error) {
	if len(meta) == 0 || meta[0] != ':' || len(path) == 0 {
		return evidence.RecipeEntry{}, fmt.Errorf("%w: unparsable change record", ErrResolve)
	}
	parts := bytes.Fields(meta[1:])
	if len(parts) != 5 {
		return evidence.RecipeEntry{}, fmt.Errorf("%w: unparsable change record", ErrResolve)
	}
	srcMode, dstMode := string(parts[0]), string(parts[1])
	dstOID, status := string(parts[3]), string(parts[4])

	switch status {
	case "D":
		// A deletion materializes no bytes; it records the PRE-IMAGE mode, which is why a deleted
		// submodule is representable where a materialized one is not.
		return evidence.RecipeEntry{
			GitPath: string(path), Mode: srcMode, Kind: evidence.EntryDeletion,
		}, nil
	case "A", "M", "T":
		if !evidence.IsObjectID(dstOID) {
			return evidence.RecipeEntry{}, fmt.Errorf("%w: a change record has a malformed destination object id", ErrResolve)
		}
		// A gitlink has no blob to materialize, so it cannot be a file entry. Failing closed is the
		// honest outcome: the packet would otherwise silently omit a change the reviewer was told to
		// review. Submodule support is a separate decision, not something to paper over here.
		if dstMode == gitlinkMode {
			return evidence.RecipeEntry{}, fmt.Errorf("%w: the change range touches a submodule, which has no blob to materialize", ErrResolve)
		}
		return evidence.RecipeEntry{
			GitPath: string(path), Mode: dstMode, Kind: evidence.EntryFile,
			Source: evidence.BlobSource{CommitBlobOID: dstOID},
		}, nil
	default:
		// U (unmerged), X (unknown), and the rename/copy statuses --no-renames suppresses.
		return evidence.RecipeEntry{}, fmt.Errorf("%w: unsupported change status %q in the review range", ErrResolve, status)
	}
}

// gitlinkMode is the mode of a submodule entry: a commit id in another repository, not a blob.
const gitlinkMode = "160000"
