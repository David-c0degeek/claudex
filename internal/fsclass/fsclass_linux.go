//go:build linux

package fsclass

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Filesystem magics that golang.org/x/sys/unix does not export, by their
// well-known values (see linux/magic.h).
const (
	magicJFS  = 0x3153464a
	magicCIFS = 0xff534d42
	magicSMB2 = 0xfe534d42
)

func classify(path string) (Result, error) {
	p, err := nearestExisting(path)
	if err != nil {
		return Result{}, err
	}
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return Result{}, err
	}
	return classifyLinuxMagic(int64(st.Type)), nil
}

// classifyLinuxMagic maps a statfs f_type magic to a class. Pure and testable.
// OverlayFS is deliberately Unknown: writes copy-up to the upper layer, whose
// backing store the overlay magic alone does not reveal (it could be network).
func classifyLinuxMagic(magic int64) Result {
	switch magic {
	case unix.EXT4_SUPER_MAGIC, // also EXT2/EXT3 (same magic)
		unix.XFS_SUPER_MAGIC,
		unix.BTRFS_SUPER_MAGIC,
		unix.TMPFS_MAGIC,
		unix.F2FS_SUPER_MAGIC,
		magicJFS,
		unix.REISERFS_SUPER_MAGIC:
		return Result{Class: SupportedLocal, Reason: fmt.Sprintf("local fstype 0x%x", uint64(magic))}
	case unix.NFS_SUPER_MAGIC,
		unix.SMB_SUPER_MAGIC,
		magicCIFS,
		magicSMB2,
		unix.NCP_SUPER_MAGIC,
		unix.AFS_SUPER_MAGIC:
		return Result{Class: KnownUnsupported, Reason: fmt.Sprintf("network fstype 0x%x", uint64(magic))}
	default:
		// OverlayFS, FUSE, and anything unrecognized: could be a user-space sync
		// client or a network-backed upper layer, so classify honestly as Unknown.
		return Result{Class: Unknown, Reason: fmt.Sprintf("unrecognized/indirect fstype 0x%x", uint64(magic))}
	}
}
