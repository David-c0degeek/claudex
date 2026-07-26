package proctree

import "os"

// syncRootDir is a no-op on Windows: directories cannot be opened for the flush that fsync performs
// on Unix, and the supervisor that publishes receipts is Linux-only. Returning nil here is not a
// claim that the entry is durable on Windows — nothing on Windows publishes one.
func syncRootDir(*os.Root) error { return nil }
