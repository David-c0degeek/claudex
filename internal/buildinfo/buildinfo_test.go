package buildinfo

import (
	"strings"
	"testing"
)

// withVars sets the package build vars for the duration of a test and restores
// them afterwards.
func withVars(t *testing.T, v, c, d string) {
	t.Helper()
	ov, oc, od := version, commit, date
	t.Cleanup(func() { version, commit, date = ov, oc, od })
	version, commit, date = v, c, d
}

func TestVersionAlwaysPrefixed(t *testing.T) {
	if got := Version(); !strings.HasPrefix(got, "claudex ") {
		t.Fatalf("Version() = %q, want prefix %q", got, "claudex ")
	}
}

func TestVersionFullFormatAndCommitTruncation(t *testing.T) {
	withVars(t, "1.2.3", "abcdef1234567890deadbeef", "2026-01-01")
	got := Version()
	want := "claudex 1.2.3 (abcdef123456, 2026-01-01)" // commit truncated to 12
	if got != want {
		t.Fatalf("Version() = %q, want %q", got, want)
	}
}

func TestVersionWithoutDate(t *testing.T) {
	withVars(t, "1.2.3", "abcdef1234567890", "")
	if got, want := Version(), "claudex 1.2.3 (abcdef123456)"; got != want {
		t.Fatalf("Version() = %q, want %q", got, want)
	}
}

func TestVersionInjectedCommitNotOverriddenByVCS(t *testing.T) {
	// An explicitly injected commit must be used verbatim (truncated), never
	// replaced by the ambient VCS stamp.
	withVars(t, "9.9.9", "0123456789ab", "")
	if got, want := Version(), "claudex 9.9.9 (0123456789ab)"; got != want {
		t.Fatalf("Version() = %q, want %q", got, want)
	}
}
