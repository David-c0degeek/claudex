package oslock

// isSymlinkOpenErr is always false on Windows, where O_NOFOLLOW does not exist. The Lstat check and
// the post-open identity comparison still reject a substitute.
func isSymlinkOpenErr(error) bool { return false }
