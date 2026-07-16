package state

// Exported grammar helpers so other packages reuse the exact locator and digest
// rules the state store enforces, rather than maintaining a divergent copy.

// IsHex64 reports whether s is exactly 64 lower-hex characters (a sha256 digest).
func IsHex64(s string) bool { return isHex64(s) }

// IsRunID reports whether s is a canonical, filename-safe identifier — the same
// grammar the state store applies to run, turn, and gate ids. Other packages
// reuse it for artifact keys rather than a divergent validator.
func IsRunID(s string) bool { return validRunID(s) }

// IsSessionID reports whether s is a canonical coordinator-minted session id
// ("sess-" + 32 lowercase hex). Session ids become directory names and cross the
// protocol wire, so this is stricter than the general id grammar (lowercase-only)
// to avoid case aliasing on a case-insensitive filesystem. Every session-path and
// assignment boundary uses this, not IsRunID.
func IsSessionID(s string) bool { return isSessionID(s) }

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

// agentPhases are the phases in which an assigned agent turn is outstanding; a
// submit accepts exactly one such turn. This is the authoritative set the
// transport turn-spec registry must equal (a parity test enforces it), kept in
// state so acceptance can be validated without importing transport.
var agentPhases = map[Phase]bool{
	PhasePlanDraft: true, PhasePlanCritique: true, PhasePlanRevise: true,
	PhaseImplementStep: true, PhaseCheckpoint: true, PhaseFix: true, PhaseVerify: true,
}

// IsAgentPhase reports whether phase p has an outstanding agent turn that a
// submit consumes. It is the single actionable-phase grammar; the transport
// turn-spec registry mirrors exactly this set.
func IsAgentPhase(p Phase) bool { return agentPhases[p] }
