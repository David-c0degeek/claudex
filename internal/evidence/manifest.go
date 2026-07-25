package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// manifest is the LOGICAL form of the packet manifest: GitPath holds the exact repository path
// bytes. It binds run/turn/phase/source and maps each logical entry (path + original mode + kind) to
// its content-addressed blob (sha256/size). It deliberately carries NO total-bytes field: the byte
// accounting is recomputed from the canonical manifest bytes + entry sizes at both produce and
// verify, so the manifest cannot be self-referential.
type manifest struct {
	SchemaVersion int
	RunID         string
	TurnID        string
	Phase         string
	Source        SourceObject
	Entries       []manifestEntry
}

// manifestEntry is one logical payload entry. SHA256 (== the in-packet blob name) and Size are set
// for a file; a deletion carries neither blob but still occupies a request and manifest budget.
type manifestEntry struct {
	GitPath string
	Mode    string
	Kind    EntryKind
	SHA256  string
	Size    int64
}

// wireManifest/wireEntry are the ON-DISK shape. A repository path is an arbitrary byte string, but
// encoding/json coerces string values to valid UTF-8 and replaces each invalid byte with U+FFFD, so
// storing a path as a JSON string is LOSSY: the distinct paths "x\x80" and "x\x81" would collapse to
// one representation and alias two different files onto one manifest entry. The wire form therefore
// carries the path as standard base64 of its exact bytes — lossless, byte-comparable, and canonical
// (a non-canonical encoding fails the re-encode equality check in parseCanonical).
type wireManifest struct {
	SchemaVersion int          `json:"schema_version"`
	RunID         string       `json:"run_id"`
	TurnID        string       `json:"turn_id"`
	Phase         string       `json:"phase"`
	Source        SourceObject `json:"source"`
	Entries       []wireEntry  `json:"entries"`
}

type wireEntry struct {
	PathB64 string    `json:"path_b64"`
	Mode    string    `json:"mode"`
	Kind    EntryKind `json:"kind"`
	SHA256  string    `json:"sha256,omitempty"`
	Size    int64     `json:"size"`
}

// validateManifest is the ONE structural authority over a manifest's logical content. Both sides use
// it — the producer through canonical(), which cannot encode a manifest that fails it, and the
// verifier through parseCanonical() — so a committed packet can never verify against weaker rules
// than the ones that produced it. Entries are reported by index, never echoed: a repository path is
// caller-supplied and may carry non-UTF-8 bytes or name something sensitive.
func validateManifest(m manifest) error {
	if m.SchemaVersion != manifestSchemaVersion {
		return fmt.Errorf("%w: manifest schema version %d, want %d", ErrVerify, m.SchemaVersion, manifestSchemaVersion)
	}
	if !isPacketName(m.RunID) {
		return fmt.Errorf("%w: manifest run id is not a safe packet name", ErrVerify)
	}
	if !isPacketName(m.TurnID) {
		return fmt.Errorf("%w: manifest turn id is not a safe packet name", ErrVerify)
	}
	if strings.TrimSpace(m.Phase) == "" {
		return fmt.Errorf("%w: manifest has no phase", ErrVerify)
	}
	if !isOID(m.Source.Commit) || !isOID(m.Source.Tree) {
		return fmt.Errorf("%w: manifest source {commit, tree} are not proven git object ids", ErrVerify)
	}
	if len(m.Entries) == 0 {
		return fmt.Errorf("%w: manifest lists no entries", ErrVerify)
	}
	seen := make(map[string]bool, len(m.Entries))
	for i, e := range m.Entries {
		if !isRawGitPath(e.GitPath) {
			return fmt.Errorf("%w: entry %d is not a valid repository path", ErrVerify, i)
		}
		if seen[e.GitPath] {
			return fmt.Errorf("%w: entry %d duplicates an earlier repository path", ErrVerify, i)
		}
		seen[e.GitPath] = true
		if !isGitMode(e.Mode) {
			return fmt.Errorf("%w: entry %d mode %q is not a canonical git mode", ErrVerify, i, e.Mode)
		}
		switch e.Kind {
		case EntryDeletion:
			if e.SHA256 != "" || e.Size != 0 {
				return fmt.Errorf("%w: deletion entry %d carries a blob", ErrVerify, i)
			}
		case EntryFile:
			if !isOID64(e.SHA256) {
				return fmt.Errorf("%w: file entry %d has a malformed digest", ErrVerify, i)
			}
			if e.Size < 0 {
				return fmt.Errorf("%w: file entry %d has a negative size", ErrVerify, i)
			}
		default:
			return fmt.Errorf("%w: entry %d has an unknown kind %q", ErrVerify, i, e.Kind)
		}
	}
	return nil
}

// canonical returns the deterministic wire bytes for a manifest: the full structural validation,
// then entries sorted by their exact path BYTES, then a compact JSON encoding of the lossless wire
// shape. Two manifests with the same logical content produce identical bytes, so the RootDigest is
// stable and a verifier can re-encode and require byte-equality. It never mutates the caller's
// entries.
func canonical(m manifest) ([]byte, error) {
	m.SchemaVersion = manifestSchemaVersion
	if err := validateManifest(m); err != nil {
		return nil, err
	}
	entries := append([]manifestEntry(nil), m.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].GitPath < entries[j].GitPath })
	w := wireManifest{
		SchemaVersion: m.SchemaVersion,
		RunID:         m.RunID,
		TurnID:        m.TurnID,
		Phase:         m.Phase,
		Source:        m.Source,
		Entries:       make([]wireEntry, 0, len(entries)),
	}
	for _, e := range entries {
		w.Entries = append(w.Entries, wireEntry{
			PathB64: base64.StdEncoding.EncodeToString([]byte(e.GitPath)),
			Mode:    e.Mode,
			Kind:    e.Kind,
			SHA256:  e.SHA256,
			Size:    e.Size,
		})
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("evidence: marshal manifest: %w", err)
	}
	return raw, nil
}

// rootDigest is the sha256 (lower hex) of the exact canonical manifest bytes.
func rootDigest(canonicalBytes []byte) string {
	sum := sha256.Sum256(canonicalBytes)
	return hex.EncodeToString(sum[:])
}

// parseCanonical strictly decodes stored manifest bytes, decodes every path back to its exact bytes,
// enforces the shared structural authority, AND requires the stored bytes to already be canonical:
// it re-encodes the parsed manifest and demands byte-equality, so a non-canonical or tampered
// manifest (reordered entries, extra whitespace, unknown fields, a non-canonical base64 path) fails
// closed. It returns the parsed manifest and the (equal) canonical bytes.
func parseCanonical(stored []byte) (manifest, []byte, error) {
	dec := json.NewDecoder(bytes.NewReader(stored))
	dec.DisallowUnknownFields()
	var w wireManifest
	if err := dec.Decode(&w); err != nil {
		return manifest{}, nil, fmt.Errorf("%w: undecodable manifest: %v", ErrVerify, err)
	}
	if _, err := dec.Token(); err == nil {
		return manifest{}, nil, fmt.Errorf("%w: trailing content after the manifest", ErrVerify)
	}
	m := manifest{
		SchemaVersion: w.SchemaVersion,
		RunID:         w.RunID,
		TurnID:        w.TurnID,
		Phase:         w.Phase,
		Source:        w.Source,
		Entries:       make([]manifestEntry, 0, len(w.Entries)),
	}
	for i, e := range w.Entries {
		raw, derr := base64.StdEncoding.DecodeString(e.PathB64)
		if derr != nil {
			return manifest{}, nil, fmt.Errorf("%w: entry %d path is not base64", ErrVerify, i)
		}
		m.Entries = append(m.Entries, manifestEntry{
			GitPath: string(raw),
			Mode:    e.Mode,
			Kind:    e.Kind,
			SHA256:  e.SHA256,
			Size:    e.Size,
		})
	}
	if err := validateManifest(m); err != nil {
		return manifest{}, nil, err
	}
	canon, err := canonical(m)
	if err != nil {
		return manifest{}, nil, err
	}
	if !bytes.Equal(stored, canon) {
		return manifest{}, nil, fmt.Errorf("%w: manifest bytes are not canonical", ErrVerify)
	}
	return m, canon, nil
}
