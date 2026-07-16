// Package atomicfile replaces a file's contents via a temp file, an fsync, and a
// rename, so that a failure before the rename leaves the previous file
// byte-identical and the target is never written in place.
//
// Consistency guarantee, stated honestly per platform (D015):
//   - POSIX: rename(2) is an atomic replace — a concurrent reader sees either the
//     old bytes or the complete new bytes, never a torn intermediate. A directory
//     fsync makes the replacement durable across power loss.
//   - Windows: os.Rename uses MoveFileEx(REPLACE_EXISTING), which Go's own
//     contract explicitly does NOT guarantee to be atomic. This package therefore
//     does not promise old-or-new semantics on Windows. Crash-consistency of
//     Windows state is provided by the caller's transaction journal + checksum
//     reconciliation on recovery (subject 01), not by this leaf.
//
// Neither platform's default path guarantees power-loss durability on Windows
// (that would need MOVEFILE_WRITE_THROUGH). Local filesystems only.
//
// This package never sweeps its directory: a crash can leave a `.claudex-tmp-*`
// file, and only the state/recovery layer — holding the exclusive run lock — may
// safely remove stale temps from a directory it owns.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const tmpPrefix = ".claudex-tmp-"

// renameBackoff is the pause between transient Windows rename retries. It is a
// package var so tests can shrink it; POSIX does not use it.
var renameBackoff = 10 * time.Millisecond

// PostCommitSyncError reports that the new file was put in place (the data IS
// committed and visible) but the follow-up durability sync failed. Callers must
// treat the write as committed and must NOT retry it.
type PostCommitSyncError struct {
	Path string
	Err  error
}

func (e *PostCommitSyncError) Error() string {
	return fmt.Sprintf("atomicfile: %s committed but directory sync failed: %v", e.Path, e.Err)
}
func (e *PostCommitSyncError) Unwrap() error { return e.Err }

// Committed reports that the write reached the visible/committed state.
func (e *PostCommitSyncError) Committed() bool { return true }

// tmpFile is the subset of *os.File the write path needs, so tests can inject
// failures at any step.
type tmpFile interface {
	Write([]byte) (int, error)
	Sync() error
	Chmod(os.FileMode) error
	Close() error
	Name() string
}

// ops is the injectable seam over the filesystem operations.
type ops struct {
	createTemp func(dir, pattern string) (tmpFile, error)
	rename     func(oldpath, newpath string) error
	syncDir    func(dir string) error
	remove     func(name string) error
}

var defaultOps = ops{
	createTemp: func(dir, pattern string) (tmpFile, error) { return os.CreateTemp(dir, pattern) },
	rename:     replace, // platform-specific
	syncDir:    syncDir, // platform-specific
	remove:     os.Remove,
}

// Write atomically (POSIX) or recoverably (Windows) writes data to path with the
// given permissions, replacing any existing file. A failure before the internal
// rename leaves any existing file untouched. If the data is committed but the
// final directory sync fails, Write returns a *PostCommitSyncError.
func Write(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, defaultOps)
}

func write(path string, data []byte, perm os.FileMode, o ops) (err error) {
	dir := filepath.Dir(path)

	tmp, err := o.createTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = o.remove(name)
		}
	}()

	// Chmod before Sync so the requested mode is covered by the fsync.
	if err = tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
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

	// The target is untouched until this point, so every failure above leaves an
	// existing file byte-identical.
	if err = o.rename(name, path); err != nil {
		return err
	}
	committed = true

	if serr := o.syncDir(dir); serr != nil {
		return &PostCommitSyncError{Path: path, Err: serr}
	}
	return nil
}
