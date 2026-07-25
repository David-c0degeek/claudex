package reviewpacket

import (
	"bytes"
	"fmt"
)

// treeRecord is one parsed `git ls-tree -z` entry.
type treeRecord struct {
	mode    string
	objType string
	oid     string
	path    string
}

// soleTreeRecord parses NUL-terminated `git ls-tree -z` output and requires it to describe EXACTLY
// one entry. A selector is an exact leaf, so anything else is a resolution failure rather than a
// choice to make silently: no records means the path is absent from the source tree, and more than
// one means the selector matched a set (a directory listing or a pathspec) instead of naming a
// single file.
//
// The -z form is what makes this lossless. Without it git C-quotes any path containing a newline,
// quote, or high byte, and the packet must record the path's exact bytes.
//
// Record grammar: "<mode> SP <type> SP <oid> TAB <path>", terminated by NUL. The path is split on
// the FIRST tab only, so a tab inside a filename survives.
func soleTreeRecord(out []byte) (treeRecord, error) {
	records := bytes.Split(out, []byte{0})
	var found treeRecord
	n := 0
	for _, rec := range records {
		if len(rec) == 0 {
			continue // the trailing terminator, or empty output
		}
		parsed, err := parseTreeRecord(rec)
		if err != nil {
			return treeRecord{}, err
		}
		n++
		if n > 1 {
			return treeRecord{}, fmt.Errorf("matched more than one entry in the source tree")
		}
		found = parsed
	}
	if n == 0 {
		return treeRecord{}, fmt.Errorf("is absent from the source tree")
	}
	return found, nil
}

func parseTreeRecord(rec []byte) (treeRecord, error) {
	tab := bytes.IndexByte(rec, '\t')
	if tab < 0 {
		return treeRecord{}, fmt.Errorf("produced an unparsable source-tree record")
	}
	meta, path := rec[:tab], rec[tab+1:]
	fields := bytes.Split(meta, []byte{' '})
	if len(fields) != 3 || len(path) == 0 {
		return treeRecord{}, fmt.Errorf("produced an unparsable source-tree record")
	}
	return treeRecord{
		mode:    string(fields[0]),
		objType: string(fields[1]),
		oid:     string(fields[2]),
		path:    string(path),
	}, nil
}
