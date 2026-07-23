package evidence

import (
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

// stageBytes writes data as a content-addressed staging blob staging/<turn>/<digest>, entirely
// within the owning staging area (so its atomicfile temp never enters a committed packet root). It
// is idempotent: an already-present staged blob (content-addressed, same bytes) is re-confirmed
// durable rather than rewritten.
func stageBytes(evRoot *os.Root, turnID, digest string, data []byte) error {
	rel := path.Join(stagingDir, turnID, digest)
	if err := atomicfile.InstallInRoot(evRoot, rel, data, blobPerm); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// A complete prior stage exists (content-addressed name == content hash); re-confirm its
			// directory entry durable without reopening the read-only file RW.
			return atomicfile.ConfirmParentInRoot(evRoot, rel)
		}
		return err
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

// sweepStaging removes a turn's staging area best-effort (it is outside every committed packet
// inventory, so leftover staging never affects verification; this only reclaims space). It is safe
// to call under the run guard while the manifest is absent OR after a successful publish.
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
