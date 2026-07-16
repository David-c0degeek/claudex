//go:build windows

package fsclass

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modCldAPI                   = windows.NewLazySystemDLL("cldapi.dll")
	procCfGetSyncRootInfoByPath = modCldAPI.NewProc("CfGetSyncRootInfoByPath")
)

func classify(path string) (Result, error) {
	p, err := nearestExisting(path)
	if err != nil {
		return Result{}, err
	}

	vol := filepath.VolumeName(p)
	// A UNC path (\\server\share) is a network location by construction.
	if strings.HasPrefix(vol, `\\`) {
		return Result{Class: KnownUnsupported, Reason: "UNC network path " + vol}, nil
	}

	// A Cloud Files sync root (OneDrive/Dropbox-style) can sit on a DRIVE_FIXED
	// volume, so check the path itself before trusting the drive type.
	if isRoot, available := isCloudSyncRoot(p); available && isRoot {
		return Result{Class: KnownUnsupported, Reason: "cloud sync root " + p}, nil
	} else if !available {
		// Detection API absent (older Windows): fall through, but say so.
		res := classifyDriveType(driveType(vol))
		res.Reason += " (cloud sync-root detection unavailable)"
		return res, nil
	}

	return classifyDriveType(driveType(vol)), nil
}

// classifyDriveType maps a GetDriveType result to a class. Pure and testable.
func classifyDriveType(dt uint32) Result {
	switch dt {
	case windows.DRIVE_FIXED:
		return Result{Class: SupportedLocal, Reason: "local fixed drive"}
	case windows.DRIVE_REMOTE:
		return Result{Class: KnownUnsupported, Reason: "network drive"}
	case windows.DRIVE_RAMDISK:
		return Result{Class: Unknown, Reason: "ram disk (volatile storage)"}
	case windows.DRIVE_REMOVABLE:
		return Result{Class: Unknown, Reason: "removable drive (unproven media semantics)"}
	default: // DRIVE_UNKNOWN, DRIVE_NO_ROOT_DIR, DRIVE_CDROM
		return Result{Class: Unknown, Reason: "undetermined drive type"}
	}
}

func driveType(vol string) uint32 {
	if vol == "" {
		return windows.DRIVE_UNKNOWN
	}
	rootp, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return windows.DRIVE_UNKNOWN
	}
	return windows.GetDriveType(rootp)
}

// isCloudSyncRoot reports whether path lives under a Cloud Files sync root, and
// whether the detection API was available at all. CfGetSyncRootInfoByPath
// returns S_OK when the path is inside a registered sync root.
func isCloudSyncRoot(path string) (isRoot bool, available bool) {
	if err := procCfGetSyncRootInfoByPath.Find(); err != nil {
		return false, false
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, true
	}
	var buf [512]byte
	var returned uint32
	const cfSyncRootInfoBasic = 0
	hr, _, _ := procCfGetSyncRootInfoByPath.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(cfSyncRootInfoBasic),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&returned)),
	)
	// S_OK (0) => path is under a sync root. Any failure HRESULT => not a sync
	// root (or not applicable); either way, not positively a sync root.
	return hr == 0, true
}
