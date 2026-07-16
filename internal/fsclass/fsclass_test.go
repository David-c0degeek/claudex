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

func TestClassifyTempDirIsSupportedLocal(t *testing.T) {
	// The test temp dir is on a local filesystem on every supported platform.
	res, err := Classify(t.TempDir())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Class != SupportedLocal {
		t.Fatalf("Classify(tempdir) = %s (%s), want supported-local", res.Class, res.Reason)
	}
	if res.Reason == "" {
		t.Fatalf("Classify returned an empty reason")
	}
}

func TestClassifyNonexistentPathUsesAncestor(t *testing.T) {
	// A path that does not exist yet classifies by its nearest existing ancestor
	// (the run directory is created after this check at bootstrap).
	deep := filepath.Join(t.TempDir(), "not", "created", "yet", "state")
	res, err := Classify(deep)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Class != SupportedLocal {
		t.Fatalf("Classify(nonexistent under tempdir) = %s, want supported-local", res.Class)
	}
}

func TestNearestExistingResolvesToAncestor(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, "a", "b", "c")
	got, err := nearestExisting(deep)
	if err != nil {
		t.Fatalf("nearestExisting: %v", err)
	}
	if got != base {
		t.Fatalf("nearestExisting(%q) = %q, want %q", deep, got, base)
	}
}
