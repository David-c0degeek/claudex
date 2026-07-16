//go:build windows

package fsclass

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
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
	if vol == "" {
		return Result{Class: Unknown, Reason: "no volume for path"}, nil
	}

	rootp, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return Result{}, err
	}
	switch windows.GetDriveType(rootp) {
	case windows.DRIVE_FIXED, windows.DRIVE_RAMDISK, windows.DRIVE_REMOVABLE:
		return Result{Class: SupportedLocal, Reason: "local drive " + vol}, nil
	case windows.DRIVE_REMOTE:
		return Result{Class: KnownUnsupported, Reason: "network drive " + vol}, nil
	default: // DRIVE_UNKNOWN, DRIVE_NO_ROOT_DIR, DRIVE_CDROM
		return Result{Class: Unknown, Reason: "undetermined drive type for " + vol}, nil
	}
}
