//go:build windows

package transport

import "os"

// syncDirHandle is a no-op on Windows: there is no directory fsync (see
// internal/atomicfile), and the OS provides no old-or-new atomic-replace or
// directory-durability guarantee to confirm.
func syncDirHandle(*os.File) error { return nil }
