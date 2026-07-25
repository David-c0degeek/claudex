package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
)

// staged is one entry's materialized, bounded result before any durable packet effect.
type staged struct {
	entry  manifestEntry
	digest string // "" for a deletion
	bytes  []byte // nil for a deletion
}

// Produce materializes, publishes, and returns the EvidenceRef for a recipe's packet under
// evidenceDir. It is deterministic and idempotent: re-running with the same recipe re-verifies an
// already-committed packet (never rewriting it) and returns the same ref. Deterministic
// authoring/bounds failures (a missing/oversize blob, a request or total-bytes breach) surface
// BEFORE any packet-root commit, so a failed produce leaves the committed packet untouched (only
// staging, which is outside every packet inventory, may hold partial bytes).
//
// Publication is self-checking end to end: a pre-existing staged file is trusted only after its
// bytes are compared, a manifest-absent packet root is refused unless it is exactly a prefix of THIS
// packet, and the finished packet is re-verified against the recipe's expectation before the ref is
// returned. Produce therefore never returns a ref that VerifyRef would reject.
func Produce(ctx context.Context, evidenceDir string, r Recipe, reader ObjectReader) (EvidenceRef, error) {
	if err := r.validate(); err != nil {
		return EvidenceRef{}, err
	}

	// 1. Materialize every entry's bytes in memory, bounded, and build the manifest — all before any
	//    durable packet effect, so a bounds/authoring failure never touches the committed packet.
	items := make([]staged, 0, len(r.Entries))
	var payloadTotal int64
	m := manifest{RunID: r.RunID, TurnID: r.TurnID, Phase: r.Phase, Source: r.Source}
	for i, e := range r.Entries {
		if e.Kind == EntryDeletion {
			items = append(items, staged{entry: manifestEntry{GitPath: e.GitPath, Mode: e.Mode, Kind: EntryDeletion}})
			m.Entries = append(m.Entries, items[len(items)-1].entry)
			continue
		}
		b, err := materialize(ctx, e, r.Bounds, reader)
		if err != nil {
			// Reported by index only: the path is caller-supplied and never echoed.
			return EvidenceRef{}, fmt.Errorf("entry %d: %w", i, err)
		}
		sum := sha256.Sum256(b)
		digest := hex.EncodeToString(sum[:])
		me := manifestEntry{GitPath: e.GitPath, Mode: e.Mode, Kind: EntryFile, SHA256: digest, Size: int64(len(b))}
		items = append(items, staged{entry: me, digest: digest, bytes: b})
		m.Entries = append(m.Entries, me)
		payloadTotal += int64(len(b))
	}

	// 2. Canonical manifest + the full byte accounting (manifest bytes + logical payload bytes).
	canon, err := canonical(m)
	if err != nil {
		return EvidenceRef{}, err
	}
	if total := int64(len(canon)) + payloadTotal; total > r.Bounds.MaxTotalBytes {
		return EvidenceRef{}, fmt.Errorf("%w: packet is %d bytes over the %d total limit", ErrBounds, total-r.Bounds.MaxTotalBytes, r.Bounds.MaxTotalBytes)
	}
	ref := EvidenceRef{ManifestRelPath: packetManifestRel(r.TurnID), RootDigest: rootDigest(canon)}
	expect := r.Expectation()

	evRoot, err := os.OpenRoot(evidenceDir)
	if err != nil {
		return EvidenceRef{}, err
	}
	defer evRoot.Close()

	// 3. Idempotent: a committed manifest means the packet is already published — re-verify it fully
	//    equals what this recipe expects, then return (never rewrite an immutable packet).
	if _, err := evRoot.Lstat(ref.ManifestRelPath); err == nil {
		if verr := verifyExpected(evRoot, r.TurnID, r.Bounds, canon, ref.RootDigest, expect); verr != nil {
			return EvidenceRef{}, verr
		}
		sweepStaging(evRoot, r.TurnID)
		return ref, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return EvidenceRef{}, err
	}

	// 4. Publish: ensure staging + packet dirs, stage+link each unique blob (no-clobber; an existing
	//    committed blob from a crash prefix is re-verified equal), then link the manifest LAST.
	if err := ensureDir(evRoot, path.Join(stagingDir, r.TurnID)); err != nil {
		return EvidenceRef{}, err
	}
	if err := ensureDir(evRoot, r.TurnID); err != nil {
		return EvidenceRef{}, err
	}
	linked := make(map[string]bool)
	for _, it := range items {
		if it.digest == "" || linked[it.digest] {
			continue // deletion, or a duplicate content already committed
		}
		if err := commitBlob(evRoot, r.TurnID, it.digest, it.bytes, r.Bounds.MaxFileBytes); err != nil {
			return EvidenceRef{}, err
		}
		linked[it.digest] = true
	}

	// 5. The packet root now holds this recipe's complete blob set. It must hold NOTHING ELSE before
	//    the commit point: a manifest-absent root is a recoverable prefix of THIS packet only, and a
	//    foreign entry has to be refused now — once the manifest is linked the packet is immutable and
	//    the stray would make it unverifiable forever.
	if err := requireInventory(evRoot, r.TurnID, linked); err != nil {
		return EvidenceRef{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if err := commitManifest(evRoot, r.TurnID, canon); err != nil {
		return EvidenceRef{}, err
	}

	// 6. The commit point is published; prove the ref about to be returned is one this package's own
	//    verifier accepts. Produce never hands back a ref whose packet would fail VerifyRef.
	if err := verifyExpected(evRoot, r.TurnID, r.Bounds, canon, ref.RootDigest, expect); err != nil {
		return EvidenceRef{}, err
	}
	sweepStaging(evRoot, r.TurnID)
	return ref, nil
}

// materialize returns an entry's bounded bytes from the committed source or the inline context.
func materialize(ctx context.Context, e RecipeEntry, b Bounds, reader ObjectReader) ([]byte, error) {
	if e.Source.Inline != nil {
		if int64(len(e.Source.Inline)) > b.MaxFileBytes {
			return nil, fmt.Errorf("%w: inline payload is %d bytes, over the %d file limit", ErrBounds, len(e.Source.Inline), b.MaxFileBytes)
		}
		out := make([]byte, len(e.Source.Inline))
		copy(out, e.Source.Inline)
		return out, nil
	}
	if reader == nil {
		return nil, fmt.Errorf("%w: a commit-blob source needs an object reader", ErrRecipe)
	}
	size, err := reader.BlobSize(ctx, e.Source.CommitBlobOID)
	if err != nil {
		return nil, err
	}
	if size > b.MaxFileBytes {
		return nil, fmt.Errorf("%w: blob %s is %d bytes, over the %d file limit", ErrBounds, e.Source.CommitBlobOID, size, b.MaxFileBytes)
	}
	data, err := reader.Blob(ctx, e.Source.CommitBlobOID)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("%w: blob %s size %d disagrees with declared %d", ErrCorrupt, e.Source.CommitBlobOID, len(data), size)
	}
	return data, nil
}

// commitBlob stages a blob and links it durably into the packet root. If the target already exists
// (a crash prefix), it re-verifies the committed bytes hash to the expected digest rather than
// overwriting the immutable entry.
func commitBlob(evRoot *os.Root, turnID, digest string, data []byte, maxFileBytes int64) error {
	if err := stageBytes(evRoot, turnID, digest, data); err != nil {
		return err
	}
	if err := linkIntoPacket(evRoot, turnID, digest); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return reverifyBlob(evRoot, turnID, digest, maxFileBytes)
		}
		return err
	}
	return nil
}

// commitManifest stages the canonical manifest and links it LAST as the packet commit point. An
// existing committed manifest must equal these exact bytes.
func commitManifest(evRoot *os.Root, turnID string, canon []byte) error {
	if err := stageBytes(evRoot, turnID, ManifestName, canon); err != nil {
		return err
	}
	if err := linkIntoPacket(evRoot, turnID, ManifestName); err != nil {
		if errors.Is(err, fs.ErrExist) {
			got, rerr := committedRegular(evRoot, turnID, ManifestName, int64(len(canon))+1)
			if rerr != nil {
				return rerr
			}
			if string(got) != string(canon) {
				return fmt.Errorf("%w: committed manifest differs from the expected bytes", ErrVerify)
			}
			return nil
		}
		return err
	}
	return nil
}

// reverifyBlob re-reads an already-committed blob and requires its content to hash to digest.
func reverifyBlob(evRoot *os.Root, turnID, digest string, maxFileBytes int64) error {
	got, err := committedRegular(evRoot, turnID, digest, maxFileBytes)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("%w: committed blob %s does not hash to its name", ErrCorrupt, digest)
	}
	return nil
}
