package state

// Exported grammar helpers so other packages reuse the exact locator and digest
// rules the state store enforces, rather than maintaining a divergent copy.

// IsHex64 reports whether s is exactly 64 lower-hex characters (a sha256 digest).
func IsHex64(s string) bool { return isHex64(s) }

// IsLocalRelPath reports whether p is a canonical, forward-slash, relative,
// traversal-free, platform-local path — the same rule the state store applies to
// stored locators. It rejects absolute, volume-qualified, backslash, colon, NUL,
// and `..` forms.
func IsLocalRelPath(p string) bool { return isLocalRelPath(p) }
