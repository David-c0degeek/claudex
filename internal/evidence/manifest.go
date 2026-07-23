package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// manifest is the canonical, versioned on-disk packet manifest. It binds run/turn/phase/source and
// maps each logical entry (lossless git path + original mode + kind) to its content-addressed blob
// (sha256/size). It deliberately carries NO total-bytes field: the byte accounting is recomputed
// from the canonical manifest bytes + entry sizes at both produce and verify, so the manifest cannot
// be self-referential.
type manifest struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         string          `json:"run_id"`
	TurnID        string          `json:"turn_id"`
	Phase         string          `json:"phase"`
	Source        SourceObject    `json:"source"`
	Entries       []manifestEntry `json:"entries"`
}

// manifestEntry is one logical payload entry. SHA256 (== the in-packet blob name) and Size are set
// for a file; a deletion carries neither blob but still occupies a request and manifest budget.
type manifestEntry struct {
	GitPath string    `json:"git_path"`
	Mode    string    `json:"mode"`
	Kind    EntryKind `json:"kind"`
	SHA256  string    `json:"sha256,omitempty"`
	Size    int64     `json:"size"`
}

// canonical returns the deterministic wire bytes for a manifest: entries sorted by git path, then a
// compact JSON encoding. Two manifests with the same logical content produce identical bytes, so the
// RootDigest is stable and a verifier can re-encode and require byte-equality.
func canonical(m manifest) ([]byte, error) {
	m.SchemaVersion = manifestSchemaVersion
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].GitPath < m.Entries[j].GitPath })
	raw, err := json.Marshal(m)
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

// parseCanonical strictly decodes stored manifest bytes AND requires them to already be canonical:
// it re-encodes the parsed manifest and demands byte-equality, so a non-canonical or tampered
// manifest (reordered entries, extra whitespace, unknown fields) fails closed. It returns the parsed
// manifest and the (equal) canonical bytes.
func parseCanonical(stored []byte) (manifest, []byte, error) {
	dec := json.NewDecoder(bytes.NewReader(stored))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return manifest{}, nil, fmt.Errorf("%w: undecodable manifest: %v", ErrVerify, err)
	}
	if _, err := dec.Token(); err == nil {
		return manifest{}, nil, fmt.Errorf("%w: trailing content after the manifest", ErrVerify)
	}
	if m.SchemaVersion != manifestSchemaVersion {
		return manifest{}, nil, fmt.Errorf("%w: manifest schema version %d, want %d", ErrVerify, m.SchemaVersion, manifestSchemaVersion)
	}
	canon, err := canonical(m)
	if err != nil {
		return manifest{}, nil, fmt.Errorf("%w: %v", ErrVerify, err)
	}
	if !bytes.Equal(stored, canon) {
		return manifest{}, nil, fmt.Errorf("%w: manifest bytes are not canonical", ErrVerify)
	}
	return m, canon, nil
}
