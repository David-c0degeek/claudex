// Package buildinfo exposes version and build metadata for the claudex binary.
//
// The values default to a development build and can be overridden at link time
// with -ldflags "-X github.com/David-c0degeek/claudex/internal/buildinfo.version=..."
// When the commit is not injected, it is recovered from the embedded VCS stamp
// that the Go toolchain records for builds made inside a git tree, including a
// "-dirty" marker when the tree had uncommitted changes at build time.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// Overridable at link time via -ldflags "-X ...=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Version returns a single-line, human-readable version string, always prefixed
// with the program name. A commit built from a dirty tree is suffixed "-dirty"
// so a development binary never presents a clean commit identity.
func Version() string {
	c := commit
	dirty := false
	if c == "" {
		c, dirty = vcsInfo()
	}
	if len(c) > 12 {
		c = c[:12]
	}
	if dirty && c != "" {
		c += "-dirty"
	}

	switch {
	case c == "":
		return fmt.Sprintf("claudex %s", version)
	case date == "":
		return fmt.Sprintf("claudex %s (%s)", version, c)
	default:
		return fmt.Sprintf("claudex %s (%s, %s)", version, c, date)
	}
}

// vcsInfo reads the git revision and dirty flag the toolchain embedded at build
// time, returning zero values when unavailable (for example under `go test`).
func vcsInfo() (revision string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return revision, modified
}
