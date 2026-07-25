package reviewpacket

import (
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/evidence"
)

func rawRec(srcMode, dstMode, srcOID, dstOID, status, path string) []byte {
	return []byte(":" + srcMode + " " + dstMode + " " + srcOID + " " + dstOID + " " + status + "\x00" + path + "\x00")
}

func TestParseRawDiff(t *testing.T) {
	zero := strings.Repeat("0", 40)
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)

	out := rawRec("000000", "100644", zero, a, "A", "added.go")
	out = append(out, rawRec("100644", "100755", a, b, "M", "dir/changed.sh")...)
	out = append(out, rawRec("100644", "000000", b, zero, "D", "gone.txt")...)

	entries, err := parseRawDiff(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	// An addition materializes the new blob under the DESTINATION mode.
	if entries[0].Kind != evidence.EntryFile || entries[0].Mode != "100644" || entries[0].Source.CommitBlobOID != a {
		t.Fatalf("added entry = %+v", entries[0])
	}
	// A mode change is carried through: the reviewer sees the file became executable.
	if entries[1].Mode != "100755" || entries[1].Source.CommitBlobOID != b || entries[1].GitPath != "dir/changed.sh" {
		t.Fatalf("modified entry = %+v", entries[1])
	}
	// A deletion carries no blob and records the PRE-IMAGE mode.
	if entries[2].Kind != evidence.EntryDeletion || entries[2].Mode != "100644" ||
		entries[2].Source.CommitBlobOID != "" || entries[2].GitPath != "gone.txt" {
		t.Fatalf("deleted entry = %+v", entries[2])
	}
}

// -z is what makes the change list lossless: without it git C-quotes any path with a newline or a
// high byte, and the manifest records exact bytes.
func TestParseRawDiffPathsAreLossless(t *testing.T) {
	a := strings.Repeat("a", 40)
	raw := "dir/x\x80\ty\nz"
	entries, err := parseRawDiff(rawRec("000000", "100644", strings.Repeat("0", 40), a, "A", raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 || entries[0].GitPath != raw {
		t.Fatalf("path = %q, want the exact bytes %q", entries[0].GitPath, raw)
	}
}

func TestParseRawDiffFailsClosed(t *testing.T) {
	zero, a := strings.Repeat("0", 40), strings.Repeat("a", 40)
	cases := map[string][]byte{
		// A submodule has no blob to materialize; omitting it would silently drop a change the
		// reviewer was told to review.
		"submodule change": rawRec("000000", "160000", zero, a, "A", "vendor/mod"),
		// Statuses --no-renames suppresses, or that mean the range is not reviewable.
		"rename status":            rawRec("100644", "100644", a, a, "R100", "moved.go"),
		"unmerged status":          rawRec("100644", "100644", a, a, "U", "conflict.go"),
		"malformed destination id": rawRec("000000", "100644", zero, "nope", "A", "x.go"),
		"missing leading colon":    []byte("100644 100644 " + a + " " + a + " M\x00x.go\x00"),
		"truncated metadata":       []byte(":100644 100644 M\x00x.go\x00"),
		"empty path":               rawRec("000000", "100644", zero, a, "A", ""),
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRawDiff(out); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
	// No records at all is an empty change list, not an error: an empty range is legal.
	entries, err := parseRawDiff(nil)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty output = %v entries, err %v", len(entries), err)
	}
}
