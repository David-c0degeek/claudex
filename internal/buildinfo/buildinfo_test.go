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

func TestFormat(t *testing.T) {
	cases := []struct {
		name                  string
		version, commit, date string
		dirty                 bool
		want                  string
	}{
		{"dev-no-commit", "dev", "", "", false, "claudex dev"},
		{"commit-only", "1.0.0", "abcdef1234567890", "", false, "claudex 1.0.0 (abcdef123456)"},
		{"commit-and-date", "1.0.0", "abcdef1234567890", "2026-01-01", false, "claudex 1.0.0 (abcdef123456, 2026-01-01)"},
		{"dirty-commit", "1.0.0", "abcdef1234567890", "", true, "claudex 1.0.0 (abcdef123456-dirty)"},
		{"dirty-with-date", "1.0.0", "abcdef1234567890", "2026-01-01", true, "claudex 1.0.0 (abcdef123456-dirty, 2026-01-01)"},
		{"dirty-without-commit-omits-marker", "dev", "", "", true, "claudex dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := format(c.version, c.commit, c.date, c.dirty); got != c.want {
				t.Fatalf("format(%q,%q,%q,%v) = %q, want %q", c.version, c.commit, c.date, c.dirty, got, c.want)
			}
		})
	}
}
