//go:build windows

package fsclass

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestClassifyCloudTriState(t *testing.T) {
	dir := t.TempDir()
	old := cloudQuery
	t.Cleanup(func() { cloudQuery = old })

	cloudQuery = func(string) (cloudState, uintptr) { return cloudUnder, hrOK }
	if res, _ := Classify(dir); res.Class != KnownUnsupported {
		t.Fatalf("cloudUnder -> %s, want known-unsupported", res.Class)
	}

	cloudQuery = func(string) (cloudState, uintptr) { return cloudIndeterminate, 0x80070005 }
	if res, _ := Classify(dir); res.Class != Unknown {
		t.Fatalf("cloudIndeterminate -> %s, want unknown", res.Class)
	}

	cloudQuery = func(string) (cloudState, uintptr) { return cloudNotUnder, hrInvalidFunction }
	if res, _ := Classify(dir); res.Class != SupportedLocal {
		t.Fatalf("cloudNotUnder on a fixed drive -> %s (%s), want supported-local", res.Class, res.Reason)
	}
}

func TestClassifyEnvSyncRootWins(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	sub := filepath.Join(resolved, "synced")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("OneDrive", resolved)

	old := cloudQuery
	t.Cleanup(func() { cloudQuery = old })
	// Even when the API says "not under", the env sync root is authoritative.
	cloudQuery = func(string) (cloudState, uintptr) { return cloudNotUnder, hrOK }

	res, err := Classify(sub)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Class != KnownUnsupported {
		t.Fatalf("path under OneDrive env root -> %s, want known-unsupported", res.Class)
	}
}

func TestPathWithin(t *testing.T) {
	if !pathWithin(`C:\Users\David\OneDrive\repo`, `C:\Users\David\OneDrive`) {
		t.Fatalf("descendant should be within")
	}
	if !pathWithin(`C:\Users\David\OneDrive`, `c:\users\david\onedrive`) {
		t.Fatalf("case-insensitive equal should be within")
	}
	if pathWithin(`C:\Users\David\OneDriveOther`, `C:\Users\David\OneDrive`) {
		t.Fatalf("sibling prefix must not be within")
	}
}

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
