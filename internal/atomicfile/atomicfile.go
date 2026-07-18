// Package atomicfile replaces a file's contents via a temp file, an fsync, and a
// rename, so that a failure before the rename leaves the previous file
// byte-identical and the target is never written in place.
//
// Consistency guarantee, stated honestly per platform:
//   - POSIX: rename(2) is an atomic replace — a concurrent reader sees either the
//     old bytes or the complete new bytes, never a torn intermediate. A directory
//     fsync makes the replacement durable across power loss.
//   - Windows: os.Rename uses MoveFileEx(REPLACE_EXISTING), which Go's own
//     contract explicitly does NOT guarantee to be atomic. This package therefore
//     makes NO old-or-new promise on Windows and does NOT implement recovery. A
//     caller that needs crash-consistency on Windows must NOT overwrite its root
//     of trust through Write; it must use the immutable-generation protocol:
//     write each new state as a fresh, checksummed
//     generation file (never overwriting the last valid one) and, on recovery,
//     enumerate and select the highest valid generation. A torn new generation is
//     detected by its checksum and ignored, leaving the previous one intact.
//
// So this is a low-level write primitive, not a crash-safe store: it is safe to
// use directly for POSIX-atomic replaces and for writing new immutable
// generation files (whose torn-write case the generation protocol handles). It
// must not be used to overwrite the single authoritative state/journal file on
// Windows.
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

// Write writes data to path with the given permissions, replacing any existing
// file. The replace is atomic on POSIX and best-effort on Windows (see the
// package doc — Windows callers needing crash-consistency use the generation
// protocol). A failure before the internal rename leaves any existing file
// untouched. If the data is committed but the final directory sync fails, Write
// returns a *PostCommitSyncError.
func Write(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, defaultOps)
}

// SyncDir fsyncs the directory dir so that entries created or renamed inside it are
// durable across power loss. On POSIX this is a real directory fsync; on Windows it
// attempts a FlushFileBuffers on the directory handle and treats the OS's
// no-directory-flush limitation as satisfied (durability there rests on
// MOVEFILE_WRITE_THROUGH renames plus NTFS metadata journaling — see the package
// doc). It is idempotent and re-runnable under a caller-held lock, so a store can
// re-confirm durability after a prior sync failure or across a process restart.
func SyncDir(dir string) error { return syncDir(dir) }

// MkdirAllDurable creates dir and every missing ancestor, fsyncing each created
// level's PARENT so the new directory entry itself is durable (not just entries
// later written inside it). An existing path that is not a directory is an error.
// It never fsyncs a level it did not create, so an already-durable tree costs one
// Stat.
func MkdirAllDurable(dir string, perm os.FileMode) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%w: %s", ErrNotDirectory, dir)
		}
		return nil // already exists; its entry was made durable when it was created
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := MkdirAllDurable(parent, perm); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, perm); err != nil && !os.IsExist(err) {
		return err
	}
	// Persist dir's own entry in its parent.
	return syncDir(parent)
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
