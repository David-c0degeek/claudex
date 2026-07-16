//go:build !windows

package transport

import "os"

// syncDirHandle fsyncs a directory handle so a new entry is durable across a
// power loss.
func syncDirHandle(d *os.File) error { return d.Sync() }
