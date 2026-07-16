//go:build linux

package fsclass

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestClassifyLinuxMagic(t *testing.T) {
	cases := []struct {
		name  string
		magic int64
		want  Class
	}{
		{"ext4", unix.EXT4_SUPER_MAGIC, SupportedLocal},
		{"xfs", unix.XFS_SUPER_MAGIC, SupportedLocal},
		{"btrfs", unix.BTRFS_SUPER_MAGIC, SupportedLocal},
		{"tmpfs", unix.TMPFS_MAGIC, SupportedLocal},
		{"jfs", magicJFS, SupportedLocal},
		{"nfs", unix.NFS_SUPER_MAGIC, KnownUnsupported},
		{"smb", unix.SMB_SUPER_MAGIC, KnownUnsupported},
		{"cifs", magicCIFS, KnownUnsupported},
		{"smb2", magicSMB2, KnownUnsupported},
		{"overlay", unix.OVERLAYFS_SUPER_MAGIC, Unknown},
		{"fuse", unix.FUSE_SUPER_MAGIC, Unknown},
		{"garbage", 0x12345678, Unknown},
	}
	for _, c := range cases {
		if got := classifyLinuxMagic(c.magic).Class; got != c.want {
			t.Fatalf("classifyLinuxMagic(%s) = %s, want %s", c.name, got, c.want)
		}
	}
}
