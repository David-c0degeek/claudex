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

	cloudQuery = func(string) (cloudState, uintptr) { return cloudNotUnder, uintptr(hrInvalidFunction) }
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

func TestClassifyHRESULT(t *testing.T) {
	cases := []struct {
		name string
		hr   uint32
		want cloudState
	}{
		{"s_ok", hrOK, cloudUnder},
		{"not_under_390", hresultFromWin32(390), cloudNotUnder},
		{"invalid_function_1", hresultFromWin32(1), cloudNotUnder},
		{"not_supported_50", hresultFromWin32(50), cloudNotUnder},
		{"incompatible_hardlinks_396", hresultFromWin32(396), cloudIndeterminate}, // must NOT be not-under
		{"access_denied_5", hresultFromWin32(5), cloudIndeterminate},
	}
	for _, c := range cases {
		if got := classifyHRESULT(c.hr); got != c.want {
			t.Fatalf("classifyHRESULT(%s=0x%08x) = %d, want %d", c.name, c.hr, got, c.want)
		}
	}
}

func TestHRESULTFromWin32(t *testing.T) {
	if got := hresultFromWin32(390); got != 0x80070186 {
		t.Fatalf("hresultFromWin32(390) = 0x%08x, want 0x80070186", got)
	}
	if got := hresultFromWin32(0); got != 0 {
		t.Fatalf("hresultFromWin32(0) = 0x%x, want 0", got)
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
