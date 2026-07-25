package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"strings"
)

// Expectation is the identity a verifier INDEPENDENTLY knows the committed packet must bind, so
// verification is never merely self-consistent. pull resolves it from run state (the run, the issued
// turn, the phase that turn was issued in, and the proven source {commit, tree} the packet was cut
// from) and Produce resolves it from the recipe. A packet that verifies internally but binds a
// different run, turn, phase, or source object is a foreign packet and fails closed.
//
// The packet's CONTEXT — which files, which frozen snapshots — is bound separately and more
// strongly: it is covered by the state-bound RootDigest, which is the sha256 of the exact canonical
// manifest bytes and therefore of every entry in it.
type Expectation struct {
	RunID  string
	TurnID string
	Phase  string
	Source SourceObject
}

func (x Expectation) validate() error {
	if !isPacketName(x.RunID) || !isPacketName(x.TurnID) {
		return fmt.Errorf("%w: expected run/turn id is not a safe packet name", ErrVerify)
	}
	if strings.TrimSpace(x.Phase) == "" {
		return fmt.Errorf("%w: an expectation must name the phase", ErrVerify)
	}
	if !isOID(x.Source.Commit) || !isOID(x.Source.Tree) {
		return fmt.Errorf("%w: expected source {commit, tree} must be proven git object ids", ErrVerify)
	}
	return nil
}

// Expectation is the identity this recipe's packet must bind; the producer verifies its own output
// against it exactly as pull will.
func (r Recipe) Expectation() Expectation {
	return Expectation{RunID: r.RunID, TurnID: r.TurnID, Phase: r.Phase, Source: r.Source}
}

// VerifyRef is pull's complete verification: given the state-bound EvidenceRef and the identity the
// caller independently expects, the committed packet must
//   - live at the derived path for the expected turn and hash to ref.RootDigest;
//   - be structurally valid under the same authority that produced it (validateManifest);
//   - bind exactly the expected run/turn/phase/source;
//   - satisfy the recomputed bounds accounting;
//   - contain every listed blob, present, regular, of the listed size, hashing to its own name;
//   - and hold NOTHING else: the packet-root inventory must equal exactly {manifest} ∪ {the unique
//     listed blob digests}.
//
// It never rewrites the packet. Any disagreement fails closed.
func VerifyRef(evidenceDir string, ref EvidenceRef, bounds Bounds, expect Expectation) error {
	if err := bounds.validate(); err != nil {
		return err
	}
	if err := expect.validate(); err != nil {
		return err
	}
	turnID, err := turnIDFromManifestRel(ref.ManifestRelPath)
	if err != nil {
		return err
	}
	if turnID != expect.TurnID {
		return fmt.Errorf("%w: manifest path names turn %q, expected %q", ErrVerify, turnID, expect.TurnID)
	}
	if !isOID64(ref.RootDigest) {
		return fmt.Errorf("%w: bound root digest is not a sha256", ErrVerify)
	}
	evRoot, err := os.OpenRoot(evidenceDir)
	if err != nil {
		return err
	}
	defer evRoot.Close()
	_, canon, err := verifyPacket(evRoot, turnID, bounds, expect)
	if err != nil {
		return err
	}
	if rootDigest(canon) != ref.RootDigest {
		return fmt.Errorf("%w: committed manifest digest does not equal the state-bound root", ErrVerify)
	}
	return nil
}

// verifyExpected is the producer's own check: the committed packet must fully verify against the
// recipe's expectation AND its canonical manifest bytes must equal the recipe-derived canon, so the
// ref Produce returns is one its own verifier accepts.
func verifyExpected(evRoot *os.Root, turnID string, bounds Bounds, expectedCanon []byte, expectedRoot string, expect Expectation) error {
	_, canon, err := verifyPacket(evRoot, turnID, bounds, expect)
	if err != nil {
		return err
	}
	if rootDigest(canon) != expectedRoot || string(canon) != string(expectedCanon) {
		return fmt.Errorf("%w: committed packet differs from the recipe that produced it", ErrVerify)
	}
	return nil
}

// verifyPacket is the complete verifier core over a committed packet root: read the manifest,
// strictly parse it through the shared structural authority, hold it to the caller's independently
// expected identity, recompute the exact bounds accounting, re-hash every listed blob, and require
// the packet-root inventory to equal exactly {manifest} ∪ {unique listed blob digests}.
func verifyPacket(evRoot *os.Root, turnID string, bounds Bounds, expect Expectation) (manifest, []byte, error) {
	stored, err := committedRegular(evRoot, turnID, ManifestName, bounds.MaxTotalBytes)
	if err != nil {
		return manifest{}, nil, fmt.Errorf("%w: manifest unreadable: %v", ErrVerify, err)
	}
	m, canon, err := parseCanonical(stored)
	if err != nil {
		return manifest{}, nil, err
	}
	// Identity: the packet must bind the run, turn, phase, and source object the caller independently
	// expects, and its turn must be the directory it is committed under.
	if m.TurnID != turnID {
		return manifest{}, nil, fmt.Errorf("%w: manifest turn id %q != packet %q", ErrVerify, m.TurnID, turnID)
	}
	if m.RunID != expect.RunID || m.TurnID != expect.TurnID || m.Phase != expect.Phase {
		return manifest{}, nil, fmt.Errorf("%w: packet binds run/turn/phase %q/%q/%q, expected %q/%q/%q",
			ErrVerify, m.RunID, m.TurnID, m.Phase, expect.RunID, expect.TurnID, expect.Phase)
	}
	if m.Source != expect.Source {
		return manifest{}, nil, fmt.Errorf("%w: packet binds source {%s, %s}, expected {%s, %s}",
			ErrVerify, m.Source.Commit, m.Source.Tree, expect.Source.Commit, expect.Source.Tree)
	}
	if len(m.Entries) > bounds.MaxRequests {
		return manifest{}, nil, fmt.Errorf("%w: %d entries exceed the %d-request limit", ErrVerify, len(m.Entries), bounds.MaxRequests)
	}

	blobs := make(map[string]int64) // unique digest -> size
	var payloadTotal int64
	for i, e := range m.Entries {
		if e.Kind != EntryFile {
			continue // structure already proven by parseCanonical; a deletion carries no blob
		}
		if e.Size > bounds.MaxFileBytes {
			return manifest{}, nil, fmt.Errorf("%w: file entry %d size %d over the %d file limit", ErrVerify, i, e.Size, bounds.MaxFileBytes)
		}
		payloadTotal += e.Size
		// Entries sharing a blob must agree on its size (validateManifest proved this, and this fold
		// re-proves it locally): a silent overwrite here would verify one entry's claimed size and
		// leave the other's unchecked, so EVERY logical entry's size is covered by the single re-hash.
		if prev, ok := blobs[e.SHA256]; ok && prev != e.Size {
			return manifest{}, nil, fmt.Errorf("%w: file entry %d claims size %d for a blob an earlier entry sized %d", ErrVerify, i, e.Size, prev)
		}
		blobs[e.SHA256] = e.Size
	}
	if total := int64(len(canon)) + payloadTotal; total > bounds.MaxTotalBytes {
		return manifest{}, nil, fmt.Errorf("%w: packet total %d over the %d limit", ErrVerify, total, bounds.MaxTotalBytes)
	}

	// Re-hash every unique committed blob.
	for digest, size := range blobs {
		got, rerr := committedRegular(evRoot, turnID, digest, bounds.MaxFileBytes)
		if rerr != nil {
			return manifest{}, nil, fmt.Errorf("%w: blob %s unreadable: %v", ErrVerify, digest, rerr)
		}
		if int64(len(got)) != size {
			return manifest{}, nil, fmt.Errorf("%w: blob %s size %d != listed %d", ErrVerify, digest, len(got), size)
		}
		sum := sha256.Sum256(got)
		if hex.EncodeToString(sum[:]) != digest {
			return manifest{}, nil, fmt.Errorf("%w: blob %s content does not hash to its name", ErrVerify, digest)
		}
	}

	// Exhaustive inventory: the packet root must contain exactly the manifest + the unique blobs.
	want := map[string]bool{ManifestName: true}
	for d := range blobs {
		want[d] = true
	}
	if err := requireInventory(evRoot, turnID, want); err != nil {
		return manifest{}, nil, fmt.Errorf("%w: %v", ErrVerify, err)
	}
	return m, canon, nil
}

// turnIDFromManifestRel validates and extracts the turn id from a derived manifest rel path
// (<turn_id>/manifest.v1.json).
func turnIDFromManifestRel(rel string) (string, error) {
	if path.Base(rel) != ManifestName {
		return "", fmt.Errorf("%w: manifest rel path %q is not a packet manifest", ErrVerify, rel)
	}
	turnID := path.Dir(rel)
	if !isPacketName(turnID) || packetManifestRel(turnID) != rel {
		return "", fmt.Errorf("%w: manifest rel path %q is not a derived packet path", ErrVerify, rel)
	}
	return turnID, nil
}

// isOID64 reports whether s is a 64-char lower-hex sha256 (the blob content-address grammar).
func isOID64(s string) bool {
	if len(s) != 64 {
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
