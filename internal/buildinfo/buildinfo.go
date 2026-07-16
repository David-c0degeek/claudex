// Package buildinfo exposes version and build metadata for the claudex binary.
//
// The values default to a development build and can be overridden at link time
// with -ldflags "-X github.com/David-c0degeek/claudex/internal/buildinfo.version=..."
// When the commit is not injected, it is recovered from the embedded VCS
// stamp that the Go toolchain records for builds made inside a git tree.
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

// Version returns a single-line, human-readable version string, always
// prefixed with the program name.
func Version() string {
	c := commit
	if c == "" {
		c = vcsRevision()
	}
	if len(c) > 12 {
		c = c[:12]
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

// vcsRevision reads the git revision the toolchain embedded at build time,
// returning "" when it is unavailable (for example under `go test`).
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
