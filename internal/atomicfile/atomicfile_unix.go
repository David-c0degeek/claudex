//go:build !windows

package atomicfile

import "os"

// replace renames oldpath onto newpath. POSIX rename(2) is an atomic replace.
func replace(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// isTransientOpen is always false on POSIX: rename and link are atomic and a
// reader never observes a sharing/access transient.
func isTransientOpen(error) bool { return false }

// syncRootDir fsyncs a directory within a confined root ("" is the root itself).
func syncRootDir(root *os.Root, name string) error {
	if name == "" {
		name = "."
	}
	d, err := root.Open(name)
	if err != nil {
		return err
	}
	serr := d.Sync()
	_ = d.Close()
	return serr
}

// syncDir fsyncs the directory so the rename is durable across a power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
