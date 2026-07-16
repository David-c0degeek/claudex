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

// IsTerminalLifecycle reports whether the run has ended — completed, cancelled,
// or failed — and so needs no active agent turn or gate.
func IsTerminalLifecycle(l Lifecycle) bool {
	switch l {
	case LifecycleCompleted, LifecycleCancelled, LifecycleFailedTerminal, LifecycleFailedRetryable:
		return true
	}
	return false
}

// IsFailureLifecycle reports whether the run ended in a failure that requires a
// failure projection.
func IsFailureLifecycle(l Lifecycle) bool {
	return l == LifecycleFailedTerminal || l == LifecycleFailedRetryable
}

// IsPausedLifecycle reports whether the run is paused waiting on a gate (human
// decision, budget, or rate limit) rather than an agent turn.
func IsPausedLifecycle(l Lifecycle) bool {
	switch l {
	case LifecyclePaused, LifecyclePausedBudget, LifecycleRateLimited:
		return true
	}
	return false
}
