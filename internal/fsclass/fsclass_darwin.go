//go:build darwin

package fsclass

import (
	"strings"

	"golang.org/x/sys/unix"
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
	return classifyDarwinName(fstypeName(st.Fstypename[:])), nil
}

// classifyDarwinName maps a macOS statfs f_fstypename to a class. Pure/testable.
func classifyDarwinName(name string) Result {
	switch name {
	case "apfs", "hfs", "hfsx", "exfat", "msdos":
		return Result{Class: SupportedLocal, Reason: "local filesystem " + name}
	case "nfs", "smbfs", "webdav", "afpfs", "ftp":
		return Result{Class: KnownUnsupported, Reason: "network filesystem " + name}
	default:
		return Result{Class: Unknown, Reason: "unrecognized filesystem " + name}
	}
}

// fstypeName turns the NUL-terminated Fstypename array into a string. The array
// element type differs across Go/x-sys versions (int8 vs byte), so it ranges and
// converts each element, which compiles for either.
func fstypeName[T ~int8 | ~byte](b []T) string {
	var sb strings.Builder
	for _, c := range b {
		if c == 0 {
			break
		}
		sb.WriteByte(byte(c))
	}
	return sb.String()
}
