package fsclass

import (
	"path/filepath"
	"testing"
)

func TestClassString(t *testing.T) {
	cases := map[Class]string{
		SupportedLocal:   "supported-local",
		KnownUnsupported: "known-unsupported",
		Unknown:          "unknown",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Fatalf("Class(%d).String() = %q, want %q", c, got, want)
		}
	}
}

// TestClassifySmoke is an integration smoke over the workstation's temp volume.
// It does not assert a specific class — the class depends on the environment's
// filesystem (tmpfs, overlay, a synced home, ...); it only checks that Classify
// returns a valid class and a reason without error.
func TestClassifySmoke(t *testing.T) {
	res, err := Classify(t.TempDir())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Class < SupportedLocal || res.Class > Unknown {
		t.Fatalf("Classify returned an invalid class %d", res.Class)
	}
	if res.Reason == "" {
		t.Fatalf("Classify returned an empty reason")
	}
	t.Logf("temp volume classified as %s (%s)", res.Class, res.Reason)
}

func TestClassifyNonexistentPathUsesAncestor(t *testing.T) {
	deep := filepath.Join(t.TempDir(), "not", "created", "yet", "state")
	if _, err := Classify(deep); err != nil {
		t.Fatalf("Classify(nonexistent under tempdir): %v", err)
	}
}

func TestNearestExistingResolvesToAncestor(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, "a", "b", "c")
	got, err := nearestExisting(deep)
	if err != nil {
		t.Fatalf("nearestExisting: %v", err)
	}
	// EvalSymlinks may canonicalize the temp path (e.g. /var -> /private/var on
	// macOS), so compare the resolved forms.
	wantResolved, _ := filepath.EvalSymlinks(base)
	if got != base && got != wantResolved {
		t.Fatalf("nearestExisting(%q) = %q, want %q (or %q)", deep, got, base, wantResolved)
	}
}
