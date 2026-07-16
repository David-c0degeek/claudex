//go:build windows

package fsclass

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestClassifyDriveType(t *testing.T) {
	cases := []struct {
		dt   uint32
		want Class
	}{
		{windows.DRIVE_FIXED, SupportedLocal},
		{windows.DRIVE_REMOTE, KnownUnsupported},
		{windows.DRIVE_RAMDISK, Unknown},
		{windows.DRIVE_REMOVABLE, Unknown},
		{windows.DRIVE_UNKNOWN, Unknown},
		{windows.DRIVE_NO_ROOT_DIR, Unknown},
		{windows.DRIVE_CDROM, Unknown},
	}
	for _, c := range cases {
		if got := classifyDriveType(c.dt).Class; got != c.want {
			t.Fatalf("classifyDriveType(%d) = %s, want %s", c.dt, got, c.want)
		}
	}
}
