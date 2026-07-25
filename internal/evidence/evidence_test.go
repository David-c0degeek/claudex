package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	if err := VerifyRef(dir, ref, testBounds, r.Expectation()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// The packet root holds exactly the manifest + the 3 unique blobs.
	names := dirNames(t, filepath.Join(dir, "turn-1"))
	if len(names) != 4 || !names[ManifestName] {
		t.Fatalf("packet inventory = %v, want manifest + 3 blobs", names)
	}
	// Ordinary success reclaims the turn's staging area: the read-only staged links are removed
	// WITHOUT relaxing the permissions they share with the committed packet payload.
	if _, serr := os.Lstat(filepath.Join(dir, "staging", "turn-1")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("staging/turn-1 survived a successful publish (lstat err = %v)", serr)
	}
	for n := range names {
		if n == ManifestName {
			continue
		}
		f, oerr := os.OpenFile(filepath.Join(dir, "turn-1", n), os.O_WRONLY, 0)
		if oerr == nil {
			f.Close()
			t.Fatalf("committed packet blob %s is writable after publication", n)
		}
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
	r := planDraftRecipe(reader)
	ref := mustProduce(t, dir, r, reader)

	mp := filepath.Join(dir, ref.ManifestRelPath)
	b, _ := os.ReadFile(mp)
	tampered := strings.Replace(string(b), "PLAN_DRAFT", "CHECKPOINT", 1)
	os.Remove(mp)                      // the committed packet is read-only; replace it
	writeFile(t, mp, []byte(tampered)) // same length, different content -> different root digest
	if err := VerifyRef(dir, ref, testBounds, r.Expectation()); !errors.Is(err, ErrVerify) {
		t.Fatalf("tampered manifest verify = %v, want ErrVerify", err)
	}
}

func TestVerifyRejectsWrongRoot(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	r := planDraftRecipe(reader)
	ref := mustProduce(t, dir, r, reader)
	ref.RootDigest = oid(2) + oid(2)[:24] // a different 64-hex digest
	if err := VerifyRef(dir, ref, testBounds, r.Expectation()); !errors.Is(err, ErrVerify) {
		t.Fatalf("wrong-root verify = %v, want ErrVerify", err)
	}
}

func TestVerifyRejectsUnlistedAndMissing(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	r := planDraftRecipe(reader)
	ref := mustProduce(t, dir, r, reader)

	// A foreign entry planted INSIDE the committed packet root (including a crash-temp sibling) is
	// unlisted -> fail closed.
	foreign := filepath.Join(dir, "turn-1", ".claudex-tmp-foreign")
	writeFile(t, foreign, []byte("junk"))
	if err := VerifyRef(dir, ref, testBounds, r.Expectation()); !errors.Is(err, ErrVerify) {
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
	if err := VerifyRef(dir, ref, testBounds, r.Expectation()); !errors.Is(err, ErrVerify) {
		t.Fatalf("missing-blob verify = %v, want ErrVerify", err)
	}
}

// A packet that is internally self-consistent is NOT enough: verification is held to the identity
// the caller independently expects, so a packet cut from another run, turn, phase, or source object
// fails closed even though its own digest matches.
func TestVerifyRejectsForeignIdentity(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	r := planDraftRecipe(reader)
	ref := mustProduce(t, dir, r, reader)

	for name, mutate := range map[string]func(x *Expectation){
		"run":    func(x *Expectation) { x.RunID = "run-2" },
		"phase":  func(x *Expectation) { x.Phase = "CHECKPOINT" },
		"commit": func(x *Expectation) { x.Source.Commit = oid(200) },
		"tree":   func(x *Expectation) { x.Source.Tree = oid(201) },
	} {
		expect := r.Expectation()
		mutate(&expect)
		if err := VerifyRef(dir, ref, testBounds, expect); !errors.Is(err, ErrVerify) {
			t.Fatalf("wrong-%s expectation verify = %v, want ErrVerify", name, err)
		}
	}
	// A ref whose derived path names a different turn than the expectation is refused before any read.
	otherTurn := EvidenceRef{ManifestRelPath: PacketManifestRel("turn-9"), RootDigest: ref.RootDigest}
	if err := VerifyRef(dir, otherTurn, testBounds, r.Expectation()); !errors.Is(err, ErrVerify) {
		t.Fatalf("turn-mismatch verify = %v, want ErrVerify", err)
	}
}

// A canonical manifest that carries the right turn and hashes to the digest it is checked against is
// still refused unless it is STRUCTURALLY complete. Without this, an empty-identity packet with no
// entries would verify purely because it agreed with itself.
func TestVerifyRejectsStructurallyEmptyManifest(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "turn-1"))
	// Exactly what canonical() would emit for a zero-valued manifest with only a turn id: field
	// order, compact encoding, and a null entry list.
	empty := []byte(`{"schema_version":1,"run_id":"","turn_id":"turn-1","phase":"","source":{"commit":"","tree":""},"entries":null}`)
	writeFile(t, filepath.Join(dir, "turn-1", ManifestName), empty)

	ref := EvidenceRef{ManifestRelPath: PacketManifestRel("turn-1"), RootDigest: rootDigest(empty)}
	expect := Expectation{RunID: "run-1", TurnID: "turn-1", Phase: "PLAN_DRAFT", Source: SourceObject{Commit: oid(100), Tree: oid(101)}}
	if err := VerifyRef(dir, ref, testBounds, expect); !errors.Is(err, ErrVerify) {
		t.Fatalf("structurally empty manifest verify = %v, want ErrVerify", err)
	}
}

// Two paths MAY share one content-addressed blob — identical content is stored once — but then they
// describe the same bytes and must claim the same size. A manifest listing a digest twice with
// different sizes would otherwise pass: only one size survives the per-blob fold, so the other
// entry's declared size is verified against nothing while the digest, bounds, and inventory checks
// all succeed. Both halves are built here through the wire encoder, so the bytes are canonical and
// only the size rule can distinguish them.
func TestVerifySharedBlobSizes(t *testing.T) {
	content := []byte("ab") // 2 bytes
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	plant := func(t *testing.T, sizeA, sizeB int64) (string, EvidenceRef, Expectation) {
		t.Helper()
		dir := t.TempDir()
		mustMkdirAll(t, filepath.Join(dir, "turn-1"))
		writeFile(t, filepath.Join(dir, "turn-1", digest), content)
		// Entries in canonical order (sorted by raw path bytes), encoded exactly as canonical() does.
		w := wireManifest{
			SchemaVersion: manifestSchemaVersion,
			RunID:         "run-1",
			TurnID:        "turn-1",
			Phase:         "PLAN_DRAFT",
			Source:        SourceObject{Commit: oid(100), Tree: oid(101)},
			Entries: []wireEntry{
				{PathB64: base64.StdEncoding.EncodeToString([]byte("a")), Mode: "100644", Kind: EntryFile, SHA256: digest, Size: sizeA},
				{PathB64: base64.StdEncoding.EncodeToString([]byte("b")), Mode: "100644", Kind: EntryFile, SHA256: digest, Size: sizeB},
			},
		}
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatalf("marshal wire manifest: %v", err)
		}
		writeFile(t, filepath.Join(dir, "turn-1", ManifestName), raw)
		ref := EvidenceRef{ManifestRelPath: PacketManifestRel("turn-1"), RootDigest: rootDigest(raw)}
		expect := Expectation{RunID: "run-1", TurnID: "turn-1", Phase: "PLAN_DRAFT", Source: SourceObject{Commit: oid(100), Tree: oid(101)}}
		return dir, ref, expect
	}

	// Agreeing sizes: legitimate dedup, must verify. This proves the adversarial half below is
	// rejected for the size disagreement and not for some unrelated reason.
	dir, ref, expect := plant(t, 2, 2)
	if err := VerifyRef(dir, ref, testBounds, expect); err != nil {
		t.Fatalf("shared blob with agreeing sizes = %v, want success", err)
	}
	// Disagreeing sizes: the first entry's claimed size is false, so the packet fails closed.
	dir, ref, expect = plant(t, 1, 2)
	if err := VerifyRef(dir, ref, testBounds, expect); !errors.Is(err, ErrVerify) {
		t.Fatalf("shared blob with disagreeing sizes = %v, want ErrVerify", err)
	}
}

// A manifest records an ORIGINAL Git mode, not an arbitrary octal-shaped token, and what is legal
// depends on the entry kind: a materialized payload must be a blob the packet can hold, while a
// deletion records only a pre-image and may name a submodule.
func TestEntryModesAreTheRealGitSet(t *testing.T) {
	for _, tc := range []struct {
		mode string
		kind EntryKind
		ok   bool
	}{
		{"100644", EntryFile, true},
		{"100755", EntryFile, true},
		{"120000", EntryFile, true},     // symlink: the blob content is the target path
		{"160000", EntryFile, false},    // gitlink: names a commit, there is no blob to materialize
		{"160000", EntryDeletion, true}, // a deleted path may have been a submodule
		{"040000", EntryFile, false},    // a tree is not a leaf
		{"040000", EntryDeletion, false},
		{"000000", EntryFile, false}, // octal-shaped, not a Git mode
		{"777777", EntryFile, false},
		{"644", EntryFile, false},
	} {
		if got := isEntryMode(tc.kind, tc.mode); got != tc.ok {
			t.Errorf("isEntryMode(%q, %q) = %v, want %v", tc.kind, tc.mode, got, tc.ok)
		}
	}
	// The recipe validator refuses one end to end, before any durable effect.
	reader := fakeReader{}
	r := planDraftRecipe(reader)
	r.Entries[0].Mode = "777777"
	if _, err := Produce(context.Background(), t.TempDir(), r, reader); !errors.Is(err, ErrRecipe) {
		t.Fatalf("bogus mode produce = %v, want ErrRecipe", err)
	}
}

// Repository paths are arbitrary byte strings. encoding/json would coerce them to valid UTF-8 and
// collapse distinct paths onto U+FFFD, so the manifest stores them losslessly: two paths differing
// only in an invalid byte must stay distinct entries and must produce distinct packet digests.
func TestManifestPathsAreLossless(t *testing.T) {
	pathA, pathB := "dir/x\x80", "dir/x\x81"
	recipeFor := func(turnID string, paths ...string) Recipe {
		r := Recipe{
			RunID:  "run-1",
			TurnID: turnID,
			Phase:  "PLAN_DRAFT",
			Source: SourceObject{Commit: oid(100), Tree: oid(101)},
			Bounds: testBounds,
		}
		for _, p := range paths {
			r.Entries = append(r.Entries, RecipeEntry{
				GitPath: p, Mode: "100644", Kind: EntryFile,
				Source: BlobSource{Inline: []byte("same bytes\n")},
			})
		}
		return r
	}

	// Both paths in ONE packet: they must survive as two distinct entries. A lossy encoding would
	// decode back to a single duplicated path and fail verification.
	dir := t.TempDir()
	both := recipeFor("turn-1", pathA, pathB)
	ref := mustProduce(t, dir, both, nil)
	if err := VerifyRef(dir, ref, testBounds, both.Expectation()); err != nil {
		t.Fatalf("verify lossless packet: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, ref.ManifestRelPath))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m, _, err := parseCanonical(stored)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(m.Entries) != 2 || m.Entries[0].GitPath != pathA || m.Entries[1].GitPath != pathB {
		t.Fatalf("paths did not round-trip: %q", []string{m.Entries[0].GitPath, m.Entries[1].GitPath})
	}

	// The two paths must not alias: identical content under either path yields a different digest.
	refA := mustProduce(t, t.TempDir(), recipeFor("turn-1", pathA), nil)
	refB := mustProduce(t, t.TempDir(), recipeFor("turn-1", pathB), nil)
	if refA.RootDigest == refB.RootDigest {
		t.Fatalf("distinct paths produced the same packet digest %s", refA.RootDigest)
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
	// staging temp.
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
	if err := VerifyRef(dir, ref, testBounds, r.Expectation()); err != nil {
		t.Fatalf("post-recovery verify: %v", err)
	}
}

// The physical halt that a name-only staging check would mishandle: the crash left FINALIZED staging
// sources (not temps) holding the WRONG bytes under the exact names the publisher is about to link —
// including manifest.v1.json, whose name is fixed rather than content-addressed. Trusting the name
// would hard-link stale bytes in as the packet's commit point and return a digest of different
// content. Recovery must re-read, discard, and restage, so the returned ref verifies.
func TestRecoveryFinalizedStagingSourceReplaced(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	r := planDraftRecipe(reader)
	ref := mustProduce(t, dir, r, reader)

	// Halt the packet just before its commit point, then plant finalized staging sources with wrong
	// content: one for the manifest, one under a real blob's content-addressed name.
	blobName := ""
	for n := range dirNames(t, filepath.Join(dir, "turn-1")) {
		if n != ManifestName {
			blobName = n
			break
		}
	}
	if blobName == "" {
		t.Fatal("no committed blob to model")
	}
	os.Remove(filepath.Join(dir, ref.ManifestRelPath))
	os.Remove(filepath.Join(dir, "turn-1", blobName)) // this blob's link never happened
	staging := filepath.Join(dir, "staging", "turn-1")
	mustMkdirAll(t, staging)
	// Staged files are published read-only, so recovery must be able to discard a 0o400 entry (on
	// Windows the read-only attribute alone refuses the delete).
	writeFilePerm(t, filepath.Join(staging, ManifestName), []byte(`{"schema_version":1,"stale":true}`), 0o400)
	writeFilePerm(t, filepath.Join(staging, blobName), []byte("not the bytes this digest names"), 0o400)

	ref2, err := Produce(context.Background(), dir, r, reader)
	if err != nil {
		t.Fatalf("recovery re-produce: %v", err)
	}
	if ref2 != ref {
		t.Fatalf("recovery ref changed: %+v vs %+v", ref2, ref)
	}
	if err := VerifyRef(dir, ref2, testBounds, r.Expectation()); err != nil {
		t.Fatalf("post-recovery verify: %v", err)
	}
}

// A manifest-absent packet root is a recoverable prefix of THIS packet only. A foreign entry in it
// must be refused BEFORE the manifest is linked: after that commit point the packet is immutable, so
// a stray committed alongside it would make the packet unverifiable forever.
func TestForeignPacketPrefixFailsClosed(t *testing.T) {
	dir := t.TempDir()
	reader := fakeReader{}
	r := planDraftRecipe(reader)
	ref := mustProduce(t, dir, r, reader)

	os.Remove(filepath.Join(dir, ref.ManifestRelPath))
	writeFile(t, filepath.Join(dir, "turn-1", "foreign-entry"), []byte("from some other packet"))

	if _, err := Produce(context.Background(), dir, r, reader); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("foreign prefix produce = %v, want ErrCorrupt", err)
	}
	// The commit point was never reached.
	if _, err := os.Lstat(filepath.Join(dir, ref.ManifestRelPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest was committed over a foreign prefix (lstat err = %v)", err)
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
	writeFilePerm(t, path, data, 0o644)
}

func writeFilePerm(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdirall %s: %v", dir, err)
	}
}

// The packet holds frozen RUN inputs and repository content in ONE manifest path space, so the
// context entries need names no repository selector can also produce. The reservation lives in the
// selector grammar, which makes the collision impossible by construction rather than a duplicate
// discovered late inside packet validation.
func TestSelectorPathReservesTheContextNamespace(t *testing.T) {
	for _, p := range []string{
		ReservedContextPrefix + "task.json",
		ReservedContextPrefix + "policy.json",
		ReservedContextPrefix + "anything/else",
	} {
		if IsSelectorPath(p) {
			t.Errorf("IsSelectorPath(%q) = true, want the reserved namespace refused", p)
		}
		// The reserved paths are still valid ENTRY paths — the producer records context under them.
		if !isRawGitPath(p) {
			t.Errorf("isRawGitPath(%q) = false, want the producer able to record context there", p)
		}
	}
	// A similarly-named path outside the reserved prefix stays selectable.
	for _, p := range []string{".claudexrc", "docs/.claudex/notes.md", ".claudex"} {
		if !IsSelectorPath(p) {
			t.Errorf("IsSelectorPath(%q) = false, want a non-reserved path accepted", p)
		}
	}
}
