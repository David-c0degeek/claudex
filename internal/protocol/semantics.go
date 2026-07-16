package protocol

import "fmt"

// Plan step bounds, shared by the schemas (minItems/maxItems on plan.v1 and
// plan_revision.v1 steps) and by state/engine materialization. A parity test
// binds the schema literals to these constants so they cannot drift.
const (
	MinPlanSteps = 1
	MaxPlanSteps = 64
)

// maxKeyLen bounds a canonical key.
const maxKeyLen = 128

// IsKey reports whether s is a canonical lowercase-hyphen key: `[a-z0-9]+(-[a-z0-9]+)*`,
// bounded. Finding/check/response keys use this — the schema descriptions do not
// enforce the grammar, so semantic validation does.
func IsKey(s string) bool {
	if len(s) == 0 || len(s) > maxKeyLen {
		return false
	}
	afterHyphen := true // a leading hyphen is invalid
	for _, c := range s {
		switch {
		case c == '-':
			if afterHyphen {
				return false // leading or doubled hyphen
			}
			afterHyphen = true
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			afterHyphen = false
		default:
			return false
		}
	}
	return !afterHyphen // no trailing hyphen
}

// ValidateKeySet enforces the key grammar and rejects duplicates for a set of
// keys (plan-critique findings, implementation checks, or plan-revision
// responses). Errors are value-free (no key echoed).
func ValidateKeySet(field string, keys []string) error {
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if !IsKey(k) {
			return fmt.Errorf("protocol: %s has a non-canonical key", field)
		}
		if seen[k] {
			return fmt.Errorf("protocol: %s has a duplicate key", field)
		}
		seen[k] = true
	}
	return nil
}

// ValidateStepTitles enforces the shared step-count bound and that step titles
// are exactly unique (so an implementation-check target_step names one step).
func ValidateStepTitles(titles []string) error {
	if len(titles) < MinPlanSteps || len(titles) > MaxPlanSteps {
		return fmt.Errorf("protocol: step count is out of the %d..%d range", MinPlanSteps, MaxPlanSteps)
	}
	seen := make(map[string]bool, len(titles))
	for _, t := range titles {
		if seen[t] {
			return fmt.Errorf("protocol: duplicate step title")
		}
		seen[t] = true
	}
	return nil
}
