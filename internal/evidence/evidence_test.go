package evidence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testBounds = Bounds{MaxTotalBytes: 262144, MaxFileBytes: 98304, MaxRequests: 8}

// fakeReader is an in-memory ObjectReader keyed by object id.
type fakeReader map[string][]byte

func (f fakeReader) BlobSize(_ context.Context, oid string) (int64, error) {
	b, ok := f[oid]
	if !ok {
		return 0, fmt.Errorf("fakeReader: no blob %s", oid)
	}
	return int64(len(b)), nil
}
func (f fakeReader) Blob(_ context.Context, oid string) ([]byte, error) {
	b, ok := f[oid]
	if !ok {
		return nil, fmt.Errorf("fakeReader: no blob %s", oid)
	}
	return b, nil
}

func oid(n int) string { return fmt.Sprintf("%040x", n) }

// planDraftRecipe is a representative PLAN_DRAFT packet: inline frozen task/policy + a relevant repo
// file read from the committed source object.
func planDraftRecipe(reader fakeReader) Recipe {
	repoOID := oid(1)
	reader[repoOID] = []byte("package main\nfunc main() {}\n")
	return Recipe{
		RunID:  "run-1",
		TurnID: "turn-1",
		Phase:  "PLAN_DRAFT",
		Source: SourceObject{Commit: oid(100), Tree: oid(101)},
		Entries: []RecipeEntry{
			{GitPath: "task.md", Mode: "100644", Kind: EntryFile, Source: BlobSource{Inline: []byte("# task\nbuild it\n")}},
			{GitPath: "config.json", Mode: "100644", Kind: EntryFile, Source: BlobSource{Inline: []byte(`{"base":"main"}`)}},
			{GitPath: "cmd/main.go", Mode: "100644", Kind: EntryFile, Source: BlobSource{CommitBlobOID: repoOID}},
		},
		Bounds: testBounds,
	}
}

func mustProduce(t *testing.T, dir string, r Recipe, reader ObjectReader) EvidenceRef {
	t.Helper()
	ref, err := Produce(context.Background(), dir, r, reader)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	return ref
}

func TestProduceVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	r := planDraftRecipe(reader)
	ref := mustProduce(t, dir, r, reader)

	if ref.ManifestRelPath != "turn-1/"+ManifestName || !isOID64(ref.RootDigest) {
		t.Fatalf("ref = %+v", ref)
	}
	if err := VerifyRef(dir, ref, testBounds); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// The packet root holds exactly the manifest + the 3 unique blobs; staging is gone.
	names := dirNames(t, filepath.Join(dir, "turn-1"))
	if len(names) != 4 || !names[ManifestName] {
		t.Fatalf("packet inventory = %v, want manifest + 3 blobs", names)
	}
}

func TestProduceIdempotentAndDeterministic(t *testing.T) {
	reader := fakeReader{}
	r := planDraftRecipe(reader)

	dir1 := t.TempDir()
	ref1 := mustProduce(t, dir1, r, reader)
	ref1b := mustProduce(t, dir1, r, reader) // re-produce: idempotent, no rewrite
	if ref1 != ref1b {
		t.Fatalf("re-produce changed the ref: %+v vs %+v", ref1, ref1b)
	}
	// Same recipe under a different root -> identical digest (deterministic).
	dir2 := t.TempDir()
	ref2 := mustProduce(t, dir2, r, reader)
	if ref2.RootDigest != ref1.RootDigest {
		t.Fatalf("digest not deterministic: %s vs %s", ref1.RootDigest, ref2.RootDigest)
	}
}

func TestVerifyRejectsTamperedManifest(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	ref := mustProduce(t, dir, planDraftRecipe(reader), reader)

	mp := filepath.Join(dir, ref.ManifestRelPath)
	b, _ := os.ReadFile(mp)
	tampered := strings.Replace(string(b), "PLAN_DRAFT", "CHECKPOINT", 1)
	os.Remove(mp)                      // the committed packet is read-only; replace it
	writeFile(t, mp, []byte(tampered)) // same length, different content -> different root digest
	if err := VerifyRef(dir, ref, testBounds); !errors.Is(err, ErrVerify) {
		t.Fatalf("tampered manifest verify = %v, want ErrVerify", err)
	}
}

func TestVerifyRejectsWrongRoot(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	ref := mustProduce(t, dir, planDraftRecipe(reader), reader)
	ref.RootDigest = oid(2) + oid(2)[:24] // a different 64-hex digest
	if err := VerifyRef(dir, ref, testBounds); !errors.Is(err, ErrVerify) {
		t.Fatalf("wrong-root verify = %v, want ErrVerify", err)
	}
}

func TestVerifyRejectsUnlistedAndMissing(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	ref := mustProduce(t, dir, planDraftRecipe(reader), reader)

	// A foreign entry planted INSIDE the committed packet root (including a crash-temp sibling) is
	// unlisted -> fail closed.
	foreign := filepath.Join(dir, "turn-1", ".claudex-tmp-foreign")
	writeFile(t, foreign, []byte("junk"))
	if err := VerifyRef(dir, ref, testBounds); !errors.Is(err, ErrVerify) {
		t.Fatalf("unlisted-entry verify = %v, want ErrVerify", err)
	}
	os.Remove(foreign)

	// Removing a listed blob -> fail closed.
	names := dirNames(t, filepath.Join(dir, "turn-1"))
	for n := range names {
		if n != ManifestName {
			os.Remove(filepath.Join(dir, "turn-1", n))
			break
		}
	}
	if err := VerifyRef(dir, ref, testBounds); !errors.Is(err, ErrVerify) {
		t.Fatalf("missing-blob verify = %v, want ErrVerify", err)
	}
}

func TestBoundsFailClosed(t *testing.T) {
	base := func() (fakeReader, Recipe) {
		reader := fakeReader{}
		return reader, planDraftRecipe(reader)
	}
	// per-file: an oversize inline entry.
	reader, r := base()
	r.Bounds.MaxFileBytes = 4
	if _, err := Produce(context.Background(), t.TempDir(), r, reader); !errors.Is(err, ErrBounds) {
		t.Fatalf("file-bound = %v, want ErrBounds", err)
	}
	// total: manifest + payload over the total.
	reader, r = base()
	r.Bounds.MaxTotalBytes = 40
	r.Bounds.MaxFileBytes = 40
	if _, err := Produce(context.Background(), t.TempDir(), r, reader); !errors.Is(err, ErrBounds) {
		t.Fatalf("total-bound = %v, want ErrBounds", err)
	}
	// requests: more logical entries than allowed.
	reader, r = base()
	r.Bounds.MaxRequests = 2
	if _, err := Produce(context.Background(), t.TempDir(), r, reader); !errors.Is(err, ErrBounds) {
		t.Fatalf("request-bound = %v, want ErrBounds", err)
	}
}

// A crash before the manifest link leaves a recoverable prefix (blobs, no manifest) plus a leftover
// staging temp; re-producing completes it and the packet verifies, and the leftover staging temp is
// invisible to the exhaustive verifier.
func TestRecoveryPrefixAndStagingTemp(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	r := planDraftRecipe(reader)
	ref := mustProduce(t, dir, r, reader)

	// Simulate the crash halt: remove the committed manifest (blobs remain) and plant a leftover
	// staging temp AND a leftover staged manifest source (the real linked-target-plus-source halt).
	os.Remove(filepath.Join(dir, ref.ManifestRelPath))
	mustMkdirAll(t, filepath.Join(dir, "staging", "turn-1"))
	writeFile(t, filepath.Join(dir, "staging", "turn-1", ".claudex-tmp-leftover"), []byte("temp"))

	ref2, err := Produce(context.Background(), dir, r, reader)
	if err != nil {
		t.Fatalf("recovery re-produce: %v", err)
	}
	if ref2 != ref {
		t.Fatalf("recovery ref changed: %+v vs %+v", ref2, ref)
	}
	if err := VerifyRef(dir, ref, testBounds); err != nil {
		t.Fatalf("post-recovery verify: %v", err)
	}
}

// --- helpers ---

func dirNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	m := make(map[string]bool, len(entries))
	for _, e := range entries {
		m[e.Name()] = true
	}
	return m
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdirall %s: %v", dir, err)
	}
}
