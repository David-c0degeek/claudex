// Package evidence is the sole producer and verifier of the immutable review-evidence packet
// (subject 04.3). A read-only review turn is actionable only through a hash-bound packet
// materialized from a COMMITTED Git object — never the live worktree. This package never mints run
// identity and never mutates run state; its callers (the issuance authorities) bind the resulting
// EvidenceRef into the state transition, and pull re-verifies + projects it.
//
// Slice 1 (this file set) is the producer + complete verifier + the serializable recipe/payload
// types + the rooted durable-link publication, provable standalone with no RunState change. The
// phase-specific SELECTION that resolves a Recipe from run state / the task contract, and the state
// v7 binding, are the integration slices.
//
// Publication topology (resolves the atomicfile linked-temp cut): all atomicfile writes happen in an
// owning per-turn STAGING directory OUTSIDE any committed packet root, so every .claudex-tmp-* and
// staged blob lives in staging. Each finalized staged blob is committed into the packet root by a
// durable no-clobber hard link, and the canonical manifest is linked LAST as the commit point. Temps
// never enter the committed packet inventory, so a manifest-present packet root stays immutable and
// its inventory is exhaustively verifiable.
package evidence

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// ManifestName is the canonical manifest leaf published last as a packet's commit point.
const ManifestName = "manifest.v1.json"

// stagingDir is the owning staging root (a sibling of every per-turn packet root under the evidence
// root), where all atomicfile temps and staged blobs live — never inside a committed packet.
const stagingDir = "staging"

// manifestSchemaVersion is the on-disk manifest schema version; an unknown version fails closed.
const manifestSchemaVersion = 1

var (
	// ErrBounds means a recipe exceeds a frozen evidence limit (per-file, total, or request count).
	// It is a deterministic authoring failure surfaced before any durable packet-root effect.
	ErrBounds = errors.New("evidence: recipe exceeds a frozen evidence limit")
	// ErrRecipe means a recipe is structurally invalid (bad identity, path, mode, or source).
	ErrRecipe = errors.New("evidence: recipe is invalid")
	// ErrVerify means a committed packet failed complete verification (schema, identity, a blob
	// digest/type/size, bounds, or an unlisted consumer-visible payload). It fails closed.
	ErrVerify = errors.New("evidence: packet failed verification")
	// ErrCorrupt means the packet store is internally inconsistent in a way that is neither a clean
	// recoverable prefix nor a verifiable packet (e.g. a foreign committed entry).
	ErrCorrupt = errors.New("evidence: packet store is corrupt")
)

// Bounds are the frozen per-run evidence limits (from run policy: 256 KiB total / 96 KiB file / 8
// requests by default). MaxTotalBytes counts the canonical manifest bytes PLUS every logical
// retained payload's bytes; MaxFileBytes applies to every retained payload; MaxRequests counts
// LOGICAL manifest entries (files + deletions), never unique deduplicated blob files.
type Bounds struct {
	MaxTotalBytes int64
	MaxFileBytes  int64
	MaxRequests   int
}

func (b Bounds) validate() error {
	if b.MaxTotalBytes <= 0 || b.MaxFileBytes <= 0 || b.MaxRequests <= 0 {
		return fmt.Errorf("%w: bounds must be positive", ErrRecipe)
	}
	if b.MaxFileBytes > b.MaxTotalBytes {
		return fmt.Errorf("%w: per-file bound exceeds the total bound", ErrRecipe)
	}
	return nil
}

// EntryKind classifies a logical manifest entry.
type EntryKind string

const (
	// EntryFile is a materialized payload: a content-addressed blob is present in the packet.
	EntryFile EntryKind = "file"
	// EntryDeletion records a path deleted versus the diff base; it carries no blob but still
	// consumes a request and manifest budget.
	EntryDeletion EntryKind = "deletion"
)

// BlobSource is where an EntryFile's bytes come from — exactly one is set. CommitBlobOID reads the
// blob from the proven committed source object; Inline carries frozen context bytes already in hand
// (task/policy snapshots, prior accepted artifacts).
type BlobSource struct {
	CommitBlobOID string
	Inline        []byte
}

// RecipeEntry is one fully-resolved, deterministic selection. GitPath holds the EXACT repository
// path bytes (a Go string is a byte string, so a non-UTF-8 Git path is carried verbatim); it is
// recorded losslessly in the manifest and never recreated on disk. Mode is the original Git mode;
// the content is content-addressed by sha256 in the packet.
type RecipeEntry struct {
	GitPath string
	Mode    string
	Kind    EntryKind
	Source  BlobSource
}

// SourceObject is the proven source identity every packet binds: the full {commit, tree}.
type SourceObject struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
}

// Recipe is a fully-resolved, deterministic packet recipe. The phase-specific logic that builds a
// Recipe from run state is the caller's (integration's) responsibility; this package materializes,
// publishes, and verifies exactly what the recipe names.
type Recipe struct {
	RunID   string
	TurnID  string
	Phase   string
	Source  SourceObject
	Entries []RecipeEntry
	Bounds  Bounds
}

// EvidenceRef is the hash-bound packet locator — the wire/state subset. ManifestRelPath is the
// packet manifest's path relative to the evidence root; RootDigest is the sha256 of the exact
// canonical manifest bytes.
type EvidenceRef struct {
	ManifestRelPath string
	RootDigest      string
}

// ObjectReader reads blob objects from a repository's committed history (never the worktree). The
// real implementation shells out to git cat-file scoped to the repo; tests inject a fake.
type ObjectReader interface {
	// BlobSize returns the size in bytes of the blob object, so a caller can bound the read before
	// materializing it.
	BlobSize(ctx context.Context, oid string) (int64, error)
	// Blob returns the full blob content. Callers bound it via BlobSize first.
	Blob(ctx context.Context, oid string) ([]byte, error)
}

// packetManifestRel is the derived, deterministic manifest path (relative to the evidence root) for
// a turn's packet. It is the single source of the ManifestRelPath binding.
func packetManifestRel(turnID string) string { return path.Join(turnID, ManifestName) }

// validate checks a recipe is structurally sound before any materialization. Identity fields must be
// nonempty and free of path separators (they become directory/blob names); every entry must carry a
// canonical Git path, a mode, and exactly one consistent source for its kind.
func (r Recipe) validate() error {
	if err := r.Bounds.validate(); err != nil {
		return err
	}
	if !isPacketName(r.RunID) || !isPacketName(r.TurnID) {
		return fmt.Errorf("%w: run/turn id is not a safe packet name", ErrRecipe)
	}
	if strings.TrimSpace(r.Phase) == "" {
		return fmt.Errorf("%w: phase is required", ErrRecipe)
	}
	if !isOID(r.Source.Commit) || !isOID(r.Source.Tree) {
		return fmt.Errorf("%w: source {commit, tree} must be proven git object ids", ErrRecipe)
	}
	if len(r.Entries) == 0 {
		return fmt.Errorf("%w: a packet must materialize at least one entry", ErrRecipe)
	}
	if len(r.Entries) > r.Bounds.MaxRequests {
		return fmt.Errorf("%w: %d logical entries exceed the %d-request limit", ErrBounds, len(r.Entries), r.Bounds.MaxRequests)
	}
	seen := make(map[string]bool, len(r.Entries))
	for i, e := range r.Entries {
		// Entry paths are reported by index only, never echoed: a repository path is
		// caller-supplied, may carry non-UTF-8 bytes, and could name something sensitive.
		if !isRawGitPath(e.GitPath) {
			return fmt.Errorf("%w: entry %d is not a valid repository path", ErrRecipe, i)
		}
		if seen[e.GitPath] {
			return fmt.Errorf("%w: entry %d duplicates an earlier repository path", ErrRecipe, i)
		}
		seen[e.GitPath] = true
		if !isGitMode(e.Mode) {
			return fmt.Errorf("%w: entry %d mode %q is not a canonical git mode", ErrRecipe, i, e.Mode)
		}
		switch e.Kind {
		case EntryDeletion:
			if e.Source.CommitBlobOID != "" || e.Source.Inline != nil {
				return fmt.Errorf("%w: deletion entry %d must carry no blob source", ErrRecipe, i)
			}
		case EntryFile:
			hasOID := e.Source.CommitBlobOID != ""
			hasInline := e.Source.Inline != nil
			if hasOID == hasInline {
				return fmt.Errorf("%w: file entry %d needs exactly one of commit-blob-oid or inline bytes", ErrRecipe, i)
			}
			if hasOID && !isOID(e.Source.CommitBlobOID) {
				return fmt.Errorf("%w: file entry %d blob oid is not a git object id", ErrRecipe, i)
			}
		default:
			return fmt.Errorf("%w: entry %d has an unknown kind %q", ErrRecipe, i, e.Kind)
		}
	}
	return nil
}

// isPacketName is the safe-name grammar for run/turn ids used as packet directory names: nonempty,
// bounded, no separators or dot names.
func isPacketName(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\:\x00") {
		return false
	}
	c := s[0]
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// isOID reports whether s is a 40- or 64-char lower-hex git object id.
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// isRawGitPath is the LOSSLESS repository-path grammar every materialized entry must satisfy. A Git
// path is an arbitrary byte string in which only '/' is structural: backslash, colon, and non-UTF-8
// bytes are all legal filename content, and a discovered diff path must round-trip them EXACTLY.
// Packet payloads are content-addressed by sha256 and the original path is never recreated on disk,
// so no platform-name restriction belongs here — only the structural rules that make a path a path:
// nonempty, bounded, relative, no NUL, and no empty or dot segments.
func isRawGitPath(p string) bool {
	if p == "" || len(p) > 4096 {
		return false
	}
	if strings.IndexByte(p, 0) >= 0 {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// IsSelectorPath is the STRICTER grammar for an AUTHOR-DECLARED selector (task-contract v2's
// relevant_repo_paths), which is a different thing from a discovered path: a human writes it, it is
// compared for exact-leaf equality against the source tree, and it must mean the same thing on every
// host. It is therefore a platform-INDEPENDENT canonical UTF-8 subset of isRawGitPath (never
// filepath.IsLocal, whose answer varies by host): valid UTF-8, no backslash or drive colon (both of
// which read as separators on some platforms and would make a selector ambiguous), and no control
// characters. U+FFFD is refused as well, so no selector can be spelled as the replacement character
// that a lossy encoder would have produced from invalid bytes.
func IsSelectorPath(p string) bool {
	if !isRawGitPath(p) || !utf8.ValidString(p) {
		return false
	}
	if strings.ContainsAny(p, "\\:") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return false
		}
	}
	return true
}

// isGitMode reports whether s is a canonical six-digit octal Git mode (100644, 100755, 120000,
// 160000, 040000). Every entry carries one, including a deletion, which records its pre-image mode.
func isGitMode(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}
