//go:build !windows

package proctree

import "os"

// syncRootDir fsyncs the attempt directory itself, so the receipt's directory ENTRY is durable and
// not merely its bytes. Opening "." through the root keeps this anchored to the same directory the
// receipt was written into rather than re-resolving a path.
func syncRootDir(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
