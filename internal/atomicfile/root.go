package atomicfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"time"
)

func sleepBackoff() { time.Sleep(renameBackoff) }

// maxRootAttempts bounds the transient-error retry loop for rooted moves/reads.
const maxRootAttempts = 10

var (
	// ErrNotRegular means a path that should be a regular file is not (a directory,
	// FIFO, device, or symlink), so it is refused as content.
	ErrNotRegular = errors.New("atomicfile: not a regular file")
	// ErrNotDirectory means a path expected to be a directory exists as something
	// else, so it cannot back a durable directory entry.
	ErrNotDirectory = errors.New("atomicfile: existing path is not a directory")
)

// rootOps is the injectable seam over the durability-sync operation for rooted
// publication, so tests can force a post-visibility sync failure and prove the
// committed-error and re-confirm semantics.
type rootOps struct {
	syncDir func(root *os.Root, name string) error
}

var defaultRootOps = rootOps{syncDir: syncRootDir}

// randName returns a unique temp name in dir. It fails closed if the OS RNG
// fails: identity generation must not silently become all-zero.
func randName(dir string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("atomicfile: random temp name: %w", err)
	}
	n := tmpPrefix + hex.EncodeToString(b[:])
	if dir == "" || dir == "." {
		return n, nil
	}
	return dir + "/" + n, nil
}

// InstallInRoot publishes name (a root-relative slash path) as a NEW immutable
// file: it writes a temp, fsyncs it, hard-links it into place (no-clobber), then
// fsyncs the file's directory and the root. If name already exists it returns an
// error satisfying errors.Is(err, fs.ErrExist); a concurrent reader sees the
// complete file or nothing. A committed-but-unsynced result is a
// *PostCommitSyncError.
func InstallInRoot(root *os.Root, name string, data []byte, perm os.FileMode) error {
	return publishInRoot(root, name, data, perm, root.Link, false, defaultRootOps)
}

// ReplaceInRoot atomically replaces name with data (a mutable, rebuildable
// projection), fsyncing the file, its directory, and the root. On Windows the OS
// provides no old-or-new guarantee (see the package doc).
func ReplaceInRoot(root *os.Root, name string, data []byte, perm os.FileMode) error {
	return publishInRoot(root, name, data, perm, root.Rename, true, defaultRootOps)
}

func publishInRoot(root *os.Root, name string, data []byte, perm os.FileMode, move func(oldpath, newpath string) error, consumesTmp bool, o rootOps) (err error) {
	dir := path.Dir(name)
	if dir == "." {
		dir = ""
	}
	tmp, err := randName(dir)
	if err != nil {
		return err
	}
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	moved := false
	defer func() {
		if !moved {
			_ = root.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = moveWithRetry(move, tmp, name); err != nil {
		return err // fs.ErrExist for a no-clobber conflict, else a real error
	}
	moved = true
	if !consumesTmp {
		_ = root.Remove(tmp) // the link left the temp behind
	}
	if serr := syncRootAndDir(root, dir, o); serr != nil {
		return &PostCommitSyncError{Path: name, Err: serr}
	}
	return nil
}

func moveWithRetry(move func(oldpath, newpath string) error, oldpath, newpath string) error {
	for i := 0; ; i++ {
		err := move(oldpath, newpath)
		if err == nil {
			return nil
		}
		if errors.Is(err, fs.ErrExist) {
			return err // a no-clobber conflict is not transient
		}
		if i >= maxRootAttempts-1 || !isTransientOpen(err) {
			return err
		}
		sleepBackoff()
	}
}

// SyncInRoot durably re-confirms an already-published file: it fsyncs the file,
// its directory, and the root, so an idempotent re-publish after a prior
// directory-sync failure still guarantees durability. Because the file is already
// visible, any failure after opening it is a committed *PostCommitSyncError, and
// the writable open (required by Windows FlushFileBuffers) uses the bounded
// sharing/access retry.
func SyncInRoot(root *os.Root, name string) error {
	return syncInRoot(root, name, defaultRootOps)
}

func syncInRoot(root *os.Root, name string, o rootOps) error {
	// A writable handle is required: on Windows FlushFileBuffers needs write
	// access, so a read-only handle's Sync fails with "Access is denied".
	var f *os.File
	for i := 0; ; i++ {
		ff, err := root.OpenFile(name, os.O_RDWR, 0)
		if err == nil {
			f = ff
			break
		}
		if errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if i >= maxRootAttempts-1 || !isTransientOpen(err) {
			return err
		}
		sleepBackoff()
	}
	serr := f.Sync()
	_ = f.Close()
	if serr != nil {
		return &PostCommitSyncError{Path: name, Err: serr} // file already visible
	}
	dir := path.Dir(name)
	if dir == "." {
		dir = ""
	}
	if derr := syncRootAndDir(root, dir, o); derr != nil {
		return &PostCommitSyncError{Path: name, Err: derr}
	}
	return nil
}

// MkdirInRoot creates a directory and fsyncs the root so the new entry is
// durable. An existing path is re-confirmed: it must be a directory (else
// ErrNotDirectory), and the root is re-synced so a directory left visible by a
// prior sync failure becomes durable. A sync failure after visibility is a
// committed *PostCommitSyncError.
func MkdirInRoot(root *os.Root, name string, perm os.FileMode) error {
	return mkdirInRoot(root, name, perm, defaultRootOps)
}

func mkdirInRoot(root *os.Root, name string, perm os.FileMode, o rootOps) error {
	if err := root.Mkdir(name, perm); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, serr := root.Stat(name)
		if serr != nil {
			return serr
		}
		if !info.IsDir() {
			return ErrNotDirectory
		}
	}
	// Re-confirm root durability whether freshly created or already visible.
	if serr := o.syncDir(root, ""); serr != nil {
		return &PostCommitSyncError{Path: name, Err: serr}
	}
	return nil
}

// ReadInRoot reads name after confirming it is a regular file and bounding its
// size, retrying briefly on transient Windows sharing/access errors. A symlink,
// directory, FIFO, device, or over-limit file is an ErrNotRegular corruption
// error, and a FIFO never blocks (opened non-blocking on POSIX).
func ReadInRoot(root *os.Root, name string, maxBytes int64) ([]byte, error) {
	for i := 0; ; i++ {
		b, err := readRegularInRoot(root, name, maxBytes)
		if err == nil {
			return b, nil
		}
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNotRegular) {
			return nil, err
		}
		if i >= maxRootAttempts-1 || !isTransientOpen(err) {
			return nil, err
		}
		sleepBackoff()
	}
}

func readRegularInRoot(root *os.Root, name string, maxBytes int64) ([]byte, error) {
	// Reject a symlink before opening. On POSIX O_NOFOLLOW also enforces this at
	// open; on Windows os.Root refuses reparse points during traversal. Lstat is
	// the portable pre-check.
	li, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return nil, ErrNotRegular
	}
	f, err := root.OpenFile(name, os.O_RDONLY|readOpenExtraFlags, 0)
	if err != nil {
		if isSymlinkOpenErr(err) {
			return nil, ErrNotRegular
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegular // FIFO, device, or directory
	}
	if info.Size() > maxBytes {
		return nil, ErrNotRegular
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, ErrNotRegular
	}
	return b, nil
}

// syncRootAndDir fsyncs the file's directory (if any) and the root itself, so a
// newly created entry is durably committed in both.
func syncRootAndDir(root *os.Root, dir string, o rootOps) error {
	if dir != "" {
		if err := o.syncDir(root, dir); err != nil {
			return err
		}
	}
	return o.syncDir(root, "")
}
