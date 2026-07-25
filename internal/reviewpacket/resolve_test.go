package reviewpacket

import (
	"strings"
	"testing"
)

// ls-tree -z output is the lossless form: a selector is an exact leaf, so exactly one record is a
// resolution and anything else is a failure rather than a silent choice.
func TestSoleTreeRecord(t *testing.T) {
	rec := func(mode, typ, oid, path string) []byte {
		return []byte(mode + " " + typ + " " + oid + "\t" + path + "\x00")
	}
	oid := strings.Repeat("a", 40)

	got, err := soleTreeRecord(rec("100644", "blob", oid, "dir/file.go"))
	if err != nil {
		t.Fatalf("single record: %v", err)
	}
	if got.mode != "100644" || got.objType != "blob" || got.oid != oid || got.path != "dir/file.go" {
		t.Fatalf("record = %+v", got)
	}

	// A path carrying bytes git would C-quote in the default format survives verbatim under -z.
	raw := "dir/x\x80\ty"
	got, err = soleTreeRecord(rec("100755", "blob", oid, raw))
	if err != nil {
		t.Fatalf("lossless record: %v", err)
	}
	if got.path != raw {
		t.Fatalf("path = %q, want the exact bytes %q", got.path, raw)
	}

	if _, err := soleTreeRecord(nil); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("empty output err = %v, want an absent-selector failure", err)
	}
	two := append(rec("100644", "blob", oid, "a"), rec("100644", "blob", oid, "b")...)
	if _, err := soleTreeRecord(two); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("multi-record err = %v, want a non-leaf failure", err)
	}
	if _, err := soleTreeRecord([]byte("garbage\x00")); err == nil {
		t.Fatal("an unparsable record must fail closed")
	}
	// A directory selector resolves to a tree, which resolveBlob refuses as a non-leaf.
	got, err = soleTreeRecord(rec("040000", "tree", oid, "dir"))
	if err != nil {
		t.Fatalf("tree record: %v", err)
	}
	if got.objType != "tree" {
		t.Fatalf("objType = %q, want tree", got.objType)
	}
}
