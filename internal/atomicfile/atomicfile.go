// Package atomicfile replaces a file's contents via a temp file, an fsync, and a
// rename, so that a failure before the rename leaves the previous file
// byte-identical and the target is never written in place.
//
// Consistency and durability, stated honestly per platform:
//   - POSIX: rename(2) is an atomic replace — a concurrent reader sees either the
//     old bytes or the complete new bytes, never a torn intermediate. A directory
//     fsync (SyncDir) makes the replacement durable across power loss, and
//     MkdirAllDurable fsyncs each new directory's parent to make its entry durable.
//   - Windows: os.Rename's default MoveFileEx(REPLACE_EXISTING) is NOT guaranteed
//     atomic and is not power-loss durable. This package therefore does NOT use it:
//     file moves force durability with MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH)
//     and fresh directories/files are published with a WRITE_THROUGH no-clobber
//     rename. A directory's ENTRY is (re)confirmed durable by FLUSHING the parent
//     directory itself — a WRITABLE directory handle (opened FILE_APPEND_DATA|
//     SYNCHRONIZE, the minimal access FlushFileBuffers needs) CAN be flushed; the
//     ERROR_ACCESS_DENIED seen earlier was only from a READ-ONLY handle. So recovery
//     re-confirms authoritatively at any time by re-flushing the parent, not merely
//     by inferring durability from a prior write. This package still makes no
//     old-or-new atomicity promise on Windows and does not implement recovery of an
//     overwritten authoritative file: crash-consistency there uses the
//     immutable-generation protocol (write each new state as a fresh, checksummed
//     generation file, never overwriting the last valid one; on recovery enumerate
//     and select the highest valid generation; a torn new generation fails its
//     checksum and is ignored, leaving the previous one intact).
//
// So this is a low-level write primitive, not a crash-safe store: it is safe to
// use directly for POSIX-atomic replaces and for writing new immutable
// generation files (whose torn-write case the generation protocol handles). It
// must not be used to overwrite the single authoritative state/journal file on
// Windows.
//
// Local filesystems only.
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

// SyncDir is an intentionally BEST-EFFORT, read-only directory sync helper: on POSIX it
// fsyncs the directory; on Windows the read-only directory-handle flush is refused by the OS
// (ERROR_ACCESS_DENIED, treated as satisfied). It is NOT the durability barrier — it is used
// only where entries are independently durable (write-through moves) or for a content re-sync.
// To FORCE a directory's entry durable, use ParentBarrier (a real writable-handle flush).
func SyncDir(dir string) error { return syncDir(dir) }

// Replace atomically renames oldPath onto newPath, replacing any existing newPath. On POSIX this
// is rename(2) (an atomic replace); on Windows it is MoveFileEx with
// MOVEFILE_REPLACE_EXISTING|MOVEFILE_WRITE_THROUGH (retrying only transient sharing/access errors).
// The caller forces the containing directory durable separately (via ParentBarrier) — this is the
// atomic-publish primitive, not the durability barrier.
func Replace(oldPath, newPath string) error { return replace(oldPath, newPath) }

// ParentBarrier forces the immediate parent directory of `dir` (and thus `dir`'s own entry)
// durable to disk with a REAL, re-runnable barrier: on POSIX it fsyncs the parent; on Windows
// it FLUSHES a WRITABLE parent-directory handle (opened FILE_APPEND_DATA|SYNCHRONIZE, no
// FILE_SHARE_DELETE) via FlushFileBuffers. It is idempotent — used to (re)confirm an existing
// directory's entry durable, never a swallowed no-op.
func ParentBarrier(dir string) error { return parentBarrier(dir) }

// ensureDirDurable makes dir exist with its entry DURABLE in its (already-durable)
// parent, idempotently and re-runnably: a missing dir is created and its entry forced to
// disk; an existing dir's entry is RE-confirmed durable without recreating. It is a
// package var so a test can inject a fail-once-then-succeed confirmer. Production wires
// the platform primitive (POSIX: create then fsync the parent; Windows: publish a fresh
// directory via a write-through no-clobber rename, then flush the writable parent handle).
var ensureDirDurable = ensureDirDurableImpl

// MkdirAllDurable creates dir and every missing ancestor, durably publishing each new
// directory entry, and RE-CONFIRMS an existing directory's entry durability on every call
// — so a retry after a prior partial (created-but-confirm-failed) attempt re-forces the
// entry rather than blessing a visible-but-unconfirmed directory. It never recreates an
// existing directory, and it does not recurse into a pre-existing ancestor (that was made
// durable when it was created); a retry after a mid-chain failure still re-confirms the
// directories it created, because the recursion for the missing leaf descends into them.
// An existing non-directory path is an error.
func MkdirAllDurable(dir string, perm os.FileMode) error {
	return mkdirAllDurable(dir, perm, ensureDirDurable)
}

func mkdirAllDurable(dir string, perm os.FileMode, ensure func(string, os.FileMode) error) error {
	parent := filepath.Dir(dir)
	if parent == dir {
		return nil // filesystem/volume root: assumed durable, nothing to publish
	}
	fi, err := os.Lstat(dir)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("%w: %s", ErrNotDirectory, dir)
		}
		// Exists: re-confirm its entry durable (re-runnable), without recreating and
		// without recursing into a pre-existing ancestor.
		return ensure(dir, perm)
	case os.IsNotExist(err):
		if e := mkdirAllDurable(parent, perm, ensure); e != nil {
			return e
		}
		return ensure(dir, perm)
	default:
		return err
	}
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
