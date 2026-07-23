package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
)

// VerifyRef is pull's self-contained verification: given the state-bound EvidenceRef, the committed
// packet at ref.ManifestRelPath must hash to ref.RootDigest AND be internally complete — every
// listed blob present/regular/exact, the bounds accounting recomputed, and the packet-root inventory
// equal to exactly {manifest} ∪ {the unique listed blob digests}. The state-bound digest is the
// trusted root, so a tampered manifest yields a different digest and fails closed. It never rewrites
// the packet.
func VerifyRef(evidenceDir string, ref EvidenceRef, bounds Bounds) error {
	if err := bounds.validate(); err != nil {
		return err
	}
	turnID, err := turnIDFromManifestRel(ref.ManifestRelPath)
	if err != nil {
		return err
	}
	evRoot, err := os.OpenRoot(evidenceDir)
	if err != nil {
		return err
	}
	defer evRoot.Close()
	_, canon, err := verifyPacket(evRoot, turnID, bounds)
	if err != nil {
		return err
	}
	if rootDigest(canon) != ref.RootDigest {
		return fmt.Errorf("%w: committed manifest digest does not equal the state-bound root", ErrVerify)
	}
	return nil
}

// verifyExpected is the producer's idempotent check: the committed packet must fully verify AND its
// canonical manifest bytes must equal the recipe-derived canon (so identity/entries/source all bind).
func verifyExpected(evRoot *os.Root, turnID string, bounds Bounds, expectedCanon []byte, expectedRoot string) error {
	_, canon, err := verifyPacket(evRoot, turnID, bounds)
	if err != nil {
		return err
	}
	if rootDigest(canon) != expectedRoot || string(canon) != string(expectedCanon) {
		return fmt.Errorf("%w: committed packet differs from the recipe that produced it", ErrVerify)
	}
	return nil
}

// verifyPacket is the complete verifier core over a committed packet root: read + canonical-check the
// manifest, recompute the exact bounds accounting, re-hash every listed blob (regular/size/sha256),
// and require the packet-root inventory to equal exactly {manifest} ∪ {unique listed blob digests}.
// Any missing, mismatched, or unlisted consumer-visible entry fails closed.
func verifyPacket(evRoot *os.Root, turnID string, bounds Bounds) (manifest, []byte, error) {
	stored, err := committedRegular(evRoot, turnID, ManifestName, bounds.MaxTotalBytes)
	if err != nil {
		return manifest{}, nil, fmt.Errorf("%w: manifest unreadable: %v", ErrVerify, err)
	}
	m, canon, err := parseCanonical(stored)
	if err != nil {
		return manifest{}, nil, err
	}
	if m.TurnID != turnID {
		return manifest{}, nil, fmt.Errorf("%w: manifest turn id %q != packet %q", ErrVerify, m.TurnID, turnID)
	}
	if len(m.Entries) > bounds.MaxRequests {
		return manifest{}, nil, fmt.Errorf("%w: %d entries exceed the %d-request limit", ErrVerify, len(m.Entries), bounds.MaxRequests)
	}

	blobs := make(map[string]int64) // unique digest -> size
	var payloadTotal int64
	for _, e := range m.Entries {
		switch e.Kind {
		case EntryDeletion:
			if e.SHA256 != "" || e.Size != 0 {
				return manifest{}, nil, fmt.Errorf("%w: deletion entry %q carries a blob", ErrVerify, e.GitPath)
			}
		case EntryFile:
			if !isOID64(e.SHA256) {
				return manifest{}, nil, fmt.Errorf("%w: file entry %q has a malformed digest", ErrVerify, e.GitPath)
			}
			if e.Size < 0 || e.Size > bounds.MaxFileBytes {
				return manifest{}, nil, fmt.Errorf("%w: file entry %q size %d over the %d file limit", ErrVerify, e.GitPath, e.Size, bounds.MaxFileBytes)
			}
			payloadTotal += e.Size
			blobs[e.SHA256] = e.Size
		default:
			return manifest{}, nil, fmt.Errorf("%w: entry %q has an unknown kind %q", ErrVerify, e.GitPath, e.Kind)
		}
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
	names, rerr := readDirNames(evRoot, turnID)
	if rerr != nil {
		return manifest{}, nil, fmt.Errorf("%w: packet root unreadable: %v", ErrVerify, rerr)
	}
	want := map[string]bool{ManifestName: true}
	for d := range blobs {
		want[d] = true
	}
	if len(names) != len(want) {
		return manifest{}, nil, fmt.Errorf("%w: packet inventory has %d entries, expected %d", ErrVerify, len(names), len(want))
	}
	for _, n := range names {
		if !want[n] {
			return manifest{}, nil, fmt.Errorf("%w: unlisted packet entry %q", ErrVerify, n)
		}
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
