//go:build windows

package fsclass

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modCldAPI                   = windows.NewLazySystemDLL("cldapi.dll")
	procCfGetSyncRootInfoByPath = modCldAPI.NewProc("CfGetSyncRootInfoByPath")
)

// cloudState is the tri-state result of a sync-root query.
type cloudState int

const (
	cloudNotUnder      cloudState = iota // proven NOT under a sync root
	cloudUnder                           // proven under a sync root
	cloudIndeterminate                   // query failed for an ambiguous reason
)

// HRESULTs of interest from CfGetSyncRootInfoByPath.
const (
	hrOK               = 0
	hrNotUnderSyncRoot = 0x8007018C // HRESULT_FROM_WIN32(ERROR_CLOUD_FILE_NOT_UNDER_SYNC_ROOT)
	hrInvalidFunction  = 0x80070001 // ERROR_INVALID_FUNCTION: volume has no Cloud Filter — reliable negative
	hrNotSupported     = 0x80070032 // ERROR_NOT_SUPPORTED
)

// cloudQuery is overridable in tests.
var cloudQuery = queryCloudSyncRoot

func classify(path string) (Result, error) {
	p, err := nearestExisting(path)
	if err != nil {
		return Result{}, err
	}

	vol := filepath.VolumeName(p)
	if strings.HasPrefix(vol, `\\`) {
		return Result{Class: KnownUnsupported, Reason: "UNC network path " + vol}, nil
	}

	// A known OneDrive environment root is a reliable positive, independent of
	// the Cloud Filter API (which may be non-functional on the volume).
	if root, ok := underEnvSyncRoot(p); ok {
		return Result{Class: KnownUnsupported, Reason: "under sync root " + root}, nil
	}

	switch state, hr := cloudQuery(p); state {
	case cloudUnder:
		return Result{Class: KnownUnsupported, Reason: "cloud sync root " + p}, nil
	case cloudIndeterminate:
		// The query neither confirmed nor cleanly denied a sync root; do not
		// assume the path is safe.
		return Result{Class: Unknown, Reason: cloudReason(hr)}, nil
	default: // cloudNotUnder
		return classifyDriveType(driveType(vol)), nil
	}
}

func cloudReason(hr uintptr) string {
	return "cloud sync-root query indeterminate (hr=0x" + strings.ToUpper(hexU32(uint32(hr))) + ")"
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

// underEnvSyncRoot reports whether p is inside one of the standard OneDrive
// environment roots. This is detectable even when the Cloud Filter API cannot
// answer.
func underEnvSyncRoot(p string) (string, bool) {
	for _, env := range []string{"OneDrive", "OneDriveConsumer", "OneDriveCommercial"} {
		root := os.Getenv(env)
		if root == "" {
			continue
		}
		if pathWithin(p, root) {
			return root, true
		}
	}
	return "", false
}

// pathWithin reports whether p is root or a descendant of root, case-insensitively
// (Windows paths are case-insensitive), on a path-component boundary.
func pathWithin(p, root string) bool {
	pc := strings.ToLower(filepath.Clean(p))
	rc := strings.ToLower(filepath.Clean(root))
	if pc == rc {
		return true
	}
	return strings.HasPrefix(pc, rc+string(filepath.Separator))
}

// queryCloudSyncRoot classifies a path via CfGetSyncRootInfoByPath into the
// tri-state. S_OK proves under-root; the documented not-under result and
// ERROR_INVALID_FUNCTION / ERROR_NOT_SUPPORTED (the volume has no Cloud Filter)
// are reliable negatives; anything else is indeterminate.
func queryCloudSyncRoot(path string) (cloudState, uintptr) {
	if err := procCfGetSyncRootInfoByPath.Find(); err != nil {
		// API unavailable (older Windows): treat as a negative — the OS cannot
		// have a Cloud Files sync root without the API present.
		return cloudNotUnder, 0
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return cloudIndeterminate, 0
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
	switch uint32(hr) {
	case hrOK:
		return cloudUnder, hr
	case hrNotUnderSyncRoot, hrInvalidFunction, hrNotSupported:
		return cloudNotUnder, hr
	default:
		return cloudIndeterminate, hr
	}
}

const hexdigits = "0123456789abcdef"

func hexU32(v uint32) string {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = hexdigits[v&0xf]
		v >>= 4
	}
	return string(b[:])
}
