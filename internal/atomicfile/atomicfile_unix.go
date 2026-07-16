//go:build !windows

package atomicfile

import "os"

// replace renames oldpath onto newpath. POSIX rename(2) is an atomic replace.
func replace(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
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
