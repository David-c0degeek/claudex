package evidence

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
)

// blobPerm/manifestPerm are read-only regular-file modes; the packet is immutable once committed.
const (
	blobPerm     = 0o400
	manifestPerm = 0o400
)

// ensureDir durably creates every segment of a forward-slash relative directory path within the
// evidence root (atomicfile.MkdirInRoot is single-level and re-runnable, so an existing prefix is
// re-confirmed, never recreated).
func ensureDir(evRoot *os.Root, rel string) error {
	var acc string
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		if acc == "" {
			acc = seg
		} else {
			acc = acc + "/" + seg
		}
		if err := atomicfile.MkdirInRoot(evRoot, acc, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// stageBytes writes data as staging/<turn>/<name>, entirely within the owning staging area (so its
// atomicfile temp never enters a committed packet root). It is idempotent AND self-correcting: a
// pre-existing staged file is trusted ONLY after its bytes are re-read and compared equal. A NAME is
// never accepted as proof of content — not even a content-addressed one, since a crash can leave a
// truncated file, and the manifest stages under a fixed name whose stale contents would otherwise be
// hard-linked in as the packet's commit point. Anything that disagrees is discarded and rewritten:
// staging is owned scratch outside every committed inventory, so replacing it is always safe.
func stageBytes(evRoot *os.Root, turnID, name string, data []byte) error {
	rel := path.Join(stagingDir, turnID, name)
	err := atomicfile.InstallInRoot(evRoot, rel, data, blobPerm)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	// Read one byte past the expected length so an oversize file is detected rather than truncated
	// into a false match; a non-regular or over-limit entry surfaces as an error and is discarded.
	got, rerr := atomicfile.ReadInRoot(evRoot, rel, int64(len(data))+1)
	if rerr == nil && bytes.Equal(got, data) {
		// A complete, byte-identical prior stage: re-confirm its directory entry durable without
		// reopening the read-only file RW (which Windows rejects).
		return atomicfile.ConfirmParentInRoot(evRoot, rel)
	}
	if derr := discardStaged(evRoot, rel); derr != nil {
		return derr
	}
	return atomicfile.InstallInRoot(evRoot, rel, data, blobPerm)
}

// discardStaged removes an unusable staging entry, failing closed if it cannot.
//
// It deliberately does NOT try to chmod its way past a refusal. A staged file that has already been
// committed is the SAME INODE as the packet entry that hard-links it, and a file's permission bits
// (on Windows, the read-only attribute) live on the inode, not on the directory entry: relaxing them
// through the staging name would make the COMMITTED packet payload writable. Removing a directory
// entry never needs write permission on the file it names — POSIX unlink is governed by the parent
// directory, and os.Root.Remove deletes a read-only file on Windows — so a refusal here is a real
// filesystem fault, not a permission detail to work around.
func discardStaged(evRoot *os.Root, rel string) error {
	if err := evRoot.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: unusable staging entry %q could not be discarded: %v", ErrCorrupt, rel, err)
	}
	return nil
}

// linkIntoPacket durably commits a staged file into the packet root by a no-clobber hard link:
// staging/<turn>/<name> -> <turn>/<name>. The link places ONLY the final entry in the packet root
// (the atomicfile temp stayed in staging), so a committed packet root never contains a temp. It
// enforces three authorities before trusting the commit:
//   - source-regularity: the staging source must be a real regular file (Lstat, never a symlink);
//   - no-clobber: os.Root.Link fails with fs.ErrExist if the target already exists (returned to the
//     caller, which re-verifies the committed entry rather than overwriting it);
//   - durability: the committed entry and its parent directory are confirmed power-safe via
//     atomicfile's rooted barrier.
func linkIntoPacket(evRoot *os.Root, turnID, name string) error {
	src := path.Join(stagingDir, turnID, name)
	dst := path.Join(turnID, name)

	info, err := evRoot.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: staging source %q is not a regular file", ErrCorrupt, src)
	}
	if err := evRoot.Link(src, dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fs.ErrExist // already committed; caller re-verifies the existing entry
		}
		return err
	}
	// The link created a NEW directory entry for an inode whose content is already durable (staging
	// fsynced it; the hard link shares the inode). Only the new parent-directory ENTRY needs to be
	// forced durable — confirm the parent, never reopen the read-only blob RW (which Windows rejects).
	if err := atomicfile.ConfirmParentInRoot(evRoot, dst); err != nil {
		return err
	}
	return nil
}

// committedRegular reads a committed packet file (regular-file-only, bounded), for verification.
func committedRegular(evRoot *os.Root, turnID, name string, maxBytes int64) ([]byte, error) {
	return atomicfile.ReadInRoot(evRoot, path.Join(turnID, name), maxBytes)
}

// requireInventory fails closed unless the packet root's entries are exactly want. It is the single
// inventory authority: the verifier holds a committed packet to {manifest} ∪ {listed blobs}, and the
// producer holds a manifest-ABSENT prefix to exactly the blobs this recipe expects. A prefix is
// recoverable only if it is a prefix of THIS packet — a foreign blob, a stray file, or a temp that
// should never be in a packet root must be refused BEFORE the manifest commit point, because after
// that point the packet is immutable and would fail verification forever.
func requireInventory(evRoot *os.Root, turnID string, want map[string]bool) error {
	names, err := readDirNames(evRoot, turnID)
	if err != nil {
		return fmt.Errorf("packet root unreadable: %v", err)
	}
	for _, n := range names {
		if !want[n] {
			return fmt.Errorf("unlisted packet entry %q", n)
		}
	}
	if len(names) != len(want) {
		return fmt.Errorf("packet holds %d entries, expected %d", len(names), len(want))
	}
	return nil
}

// sweepStaging reclaims a turn's staging area. Every entry is a hard link to an inode the packet
// already owns (or to bytes nothing kept), so removing the staging NAME frees the directory entry
// without touching the committed payload — and it must never relax permissions to do so, since the
// bits are shared with the committed link (see discardStaged).
//
// It is best-effort by design, not by oversight: staging lies outside every committed inventory, so
// a leftover entry is invisible to verification and is reclaimed by the next produce for this turn.
// Failing a fully published, fully verified packet over unreclaimed scratch would be strictly worse.
// The case that must NOT be silently tolerated — a staged entry that cannot be discarded when it has
// to be REPLACED — is fatal in discardStaged. It is safe to call under the run guard while the
// manifest is absent or after a successful publish.
func sweepStaging(evRoot *os.Root, turnID string) {
	dir := path.Join(stagingDir, turnID)
	entries, err := readDirNames(evRoot, dir)
	if err != nil {
		return
	}
	for _, name := range entries {
		_ = evRoot.Remove(path.Join(dir, name))
	}
	_ = evRoot.Remove(dir)
}

// readDirNames lists the immediate entry names of a directory within the evidence root.
func readDirNames(evRoot *os.Root, rel string) ([]string, error) {
	d, err := evRoot.Open(rel)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	infos, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(infos))
	for _, in := range infos {
		names = append(names, in.Name())
	}
	return names, nil
}
