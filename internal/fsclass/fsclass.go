// Package fsclass classifies a filesystem path so the coordinator only stores
// durable state where its advisory-lock and file-replace guarantees actually
// hold: local filesystems (D015).
//
// Classification is honest about its limits. Network filesystems (SMB/NFS) and
// remote-mapped drives are usually detectable and reported as KnownUnsupported.
// A local fixed disk is SupportedLocal. But third-party user-space sync roots
// (OneDrive/Dropbox-style) that present as an ordinary local directory cannot be
// detected in general, so anything the OS cannot positively classify is Unknown,
// and the caller applies its Unknown policy (refuse by default, or proceed only
// on explicit acknowledgement). There is no claim of perfect detection.
package fsclass

import (
	"os"
	"path/filepath"
)

// Class is the durability-support classification of a path's filesystem.
type Class int

const (
	// SupportedLocal: a local filesystem where lock/replace guarantees hold.
	SupportedLocal Class = iota
	// KnownUnsupported: a filesystem detected as network/remote where the
	// guarantees do not hold.
	KnownUnsupported
	// Unknown: the OS did not positively classify it; treat per policy.
	Unknown
)

func (c Class) String() string {
	switch c {
	case SupportedLocal:
		return "supported-local"
	case KnownUnsupported:
		return "known-unsupported"
	default:
		return "unknown"
	}
}

// Result is a classification plus a human-readable reason for status/doctor.
type Result struct {
	Class  Class
	Reason string
}

// Classify reports the durability-support class of the filesystem backing path.
// The path need not exist yet; the nearest existing ancestor is classified so a
// not-yet-created run directory can be checked at bootstrap.
func Classify(path string) (Result, error) {
	return classify(path)
}

// nearestExisting returns the nearest ancestor of path (including path itself)
// that exists on disk, with symlinks/reparse points resolved so a junction
// cannot hide a remote target behind a local-looking volume. It walks up only on
// "does not exist"; a permission or I/O error is returned, never walked past.
func nearestExisting(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		_, err := os.Lstat(p)
		if err == nil {
			// Resolve reparse points/symlinks and classify the real target.
			if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
				return resolved, nil
			}
			return p, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p, nil // reached the volume/filesystem root
		}
		p = parent
	}
}
