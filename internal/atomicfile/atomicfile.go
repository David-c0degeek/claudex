// Package atomicfile writes files atomically on a local filesystem: a reader
// sees either the old bytes or the complete new bytes, never a torn
// intermediate. A failed write leaves the previous file byte-identical.
//
// The guarantee is process-crash atomicity. Power-loss durability additionally
// depends on the fsyncs performed here (file on all platforms, directory on
// POSIX); on Windows directory metadata is not flushed (see docs/decisions.md
// D015). Local filesystems only — advisory guarantees do not hold on network or
// sync roots (D015).
package atomicfile

import (
	"os"
	"path/filepath"
)

const tmpPrefix = ".claudex-tmp-"

// Write atomically writes data to path with the given permissions, replacing
// any existing file.
func Write(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err = replace(tmpName, path); err != nil {
		return err
	}
	cleanup = false // the temp file has been renamed away

	return syncDir(dir)
}
