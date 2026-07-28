package config

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// ResolvedExecution is the environment the mechanical test command will actually run with, resolved
// ONCE at first attach and frozen.
//
// It exists because naming a variable is not the same as freezing it. `env.inherit` makes inheritance
// INTENTIONAL — the operator said this name may be seen — but the VALUE still comes from the host, so
// without resolving it here every attempt would read whatever the ambient environment happened to hold
// at that moment. Binding the value's identity afterwards would only detect drift once it had already
// changed what executed.
//
// It is a SEPARATE shape from EffectivePolicy rather than resolved values written into it, and that is
// forced: BootstrapIntent requires the re-parsed policy snapshot to equal EffectivePolicy exactly, so
// putting host values inside the policy would break the one invariant proving the frozen policy is the
// document the operator supplied.
//
// It deliberately carries NO argv and NO executable path. Those are ResolvedCommand, produced per
// attempt, because a workspace-relative test executable such as ./scripts/test may not exist when the
// worktree is provisioned — the accepted implementation commit can create it later — so resolving it
// at attach would either fail a valid policy or bind a path that changes meaning.
type ResolvedExecution struct {
	// Env is the resolved environment, sorted by name. Sorted so the shape is canonical and two
	// resolutions of the same policy on the same host are byte-identical.
	Env []EnvAssignment `json:"env"`
	// ScratchHome, ScratchCache and ScratchTemp are tool-owned per-run directories. They exist because
	// HOME is not inherited: tools that insist on one get a directory inside the run rather than the
	// operator's, which is what keeps credential-bearing config out of the command's reach.
	ScratchHome  string `json:"scratch_home"`
	ScratchCache string `json:"scratch_cache"`
	ScratchTemp  string `json:"scratch_temp"`
}

// Resolution ceilings, validated at attach BEFORE anything is journaled.
//
// The policy source is bounded at 16 KiB, but an INHERITED value is not bounded by it at all — it comes
// from the host and can be arbitrarily large — while a prepared-transaction payload caps at 64 KiB. So
// a policy that is itself small could resolve to an intent that cannot be journaled, discovered only
// once the run was already being created.
const (
	MaxResolvedEnvNames     = 64
	MaxResolvedEnvValue     = 4096
	MaxResolvedEnvTotalByte = 32768
)

// EnvLookup reads an ambient variable. It is a parameter so resolution is testable without mutating the
// process environment, and so the one place that touches ambient state is explicit.
type EnvLookup func(name string) (string, bool)

// AuthorizedEnvNames is the closed set of names a run may carry: what the operator allowlisted, plus the
// enumerated platform-required set for this GOOS.
//
// The union is the AUTHORITY, stated here rather than assembled during resolution. An earlier draft had
// resolution quietly add the Windows names, which contradicted the policy's own claim that nothing
// unnamed reaches the command — recording an implicitly added value does not make it authorized.
func AuthorizedEnvNames(g TestGate, goos string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range g.Env.Inherit {
		add(n)
	}
	for _, e := range g.Env.Set {
		add(e.Name)
	}
	for _, n := range PlatformRequired(goos) {
		add(n)
	}
	sort.Strings(out)
	return out
}

// ResolveExecution freezes the environment for a run.
//
// An inherited name that is ABSENT from the host is simply absent from the result rather than resolved
// to an empty string. The two are distinguishable to a program — `os.LookupEnv` reports which — and
// inventing an empty value would hand the command a variable the host never had.
func ResolveExecution(g TestGate, goos string, look EnvLookup, scratchHome, scratchCache, scratchTemp string) (ResolvedExecution, error) {
	var re ResolvedExecution
	if g.Disabled {
		// Nothing will run, so there is nothing to resolve. The scratch paths are still recorded so the
		// shape is uniform and a later enable is a policy change rather than a shape change.
		re.Env = []EnvAssignment{}
	} else {
		set := make(map[string]string, len(g.Env.Set))
		for _, e := range g.Env.Set {
			set[e.Name] = e.Value
		}
		for _, n := range AuthorizedEnvNames(g, goos) {
			if v, ok := set[n]; ok {
				re.Env = append(re.Env, EnvAssignment{Name: n, Value: v})
				continue
			}
			if v, ok := look(n); ok {
				re.Env = append(re.Env, EnvAssignment{Name: n, Value: v})
			}
		}
		if re.Env == nil {
			re.Env = []EnvAssignment{}
		}
	}
	re.ScratchHome, re.ScratchCache, re.ScratchTemp = scratchHome, scratchCache, scratchTemp
	if err := re.ValidateFor(g, goos); err != nil {
		return ResolvedExecution{}, err
	}
	return re, nil
}

// ValidateFor proves a resolved execution is authorized by the gate it claims to come from, and that it
// fits the ceilings.
//
// It runs at attach before journaling AND again when the state-bound copy is read, because the value
// recovery acts on must be the one attach authorized rather than one that merely looks plausible.
func (re ResolvedExecution) ValidateFor(g TestGate, goos string) error {
	authorized := make(map[string]bool)
	for _, n := range AuthorizedEnvNames(g, goos) {
		authorized[n] = true
	}
	if len(re.Env) > MaxResolvedEnvNames {
		return fmt.Errorf("resolved execution carries %d environment names, limit %d", len(re.Env), MaxResolvedEnvNames)
	}
	total := 0
	seen := make(map[string]bool, len(re.Env))
	var prev string
	for i, e := range re.Env {
		if err := validateEnvName(e.Name); err != nil {
			return fmt.Errorf("resolved execution env[%d]: %w", i, err)
		}
		if !authorized[e.Name] {
			return fmt.Errorf("resolved execution carries %s, which the policy does not authorize", e.Name)
		}
		if seen[e.Name] {
			return fmt.Errorf("resolved execution carries %s twice", e.Name)
		}
		seen[e.Name] = true
		// Sorted, so the shape is canonical rather than dependent on resolution order.
		if i > 0 && e.Name <= prev {
			return fmt.Errorf("resolved execution env is not sorted by name (%s after %s)", e.Name, prev)
		}
		prev = e.Name
		if strings.ContainsRune(e.Value, 0) {
			return fmt.Errorf("resolved execution value of %s contains NUL", e.Name)
		}
		if len(e.Value) > MaxResolvedEnvValue {
			return fmt.Errorf("resolved execution value of %s is %d bytes, limit %d", e.Name, len(e.Value), MaxResolvedEnvValue)
		}
		total += len(e.Name) + 1 + len(e.Value)
	}
	if total > MaxResolvedEnvTotalByte {
		return fmt.Errorf("resolved execution environment is %d bytes, limit %d", total, MaxResolvedEnvTotalByte)
	}
	// An explicitly SET name is stated by the policy, so it must be present whatever the host holds.
	// An inherited one may legitimately be absent.
	for _, e := range g.Env.Set {
		if !g.Disabled && !seen[e.Name] {
			return fmt.Errorf("resolved execution omits %s, which the policy sets explicitly", e.Name)
		}
	}
	for _, p := range []struct{ what, v string }{
		{"scratch_home", re.ScratchHome},
		{"scratch_cache", re.ScratchCache},
		{"scratch_temp", re.ScratchTemp},
	} {
		if strings.TrimSpace(p.v) == "" {
			return fmt.Errorf("resolved execution %s is required", p.what)
		}
	}
	return nil
}

// ResolveForRun resolves the frozen environment with the run's own scratch layout.
//
// The layout lives here rather than at the call site so the three directories cannot drift apart
// between bootstrap and anything that later has to reason about them.
func ResolveForRun(g TestGate, goos string, look EnvLookup, relDir string) (ResolvedExecution, error) {
	return ResolveExecution(g, goos, look, relDir+"/scratch/home", relDir+"/scratch/cache", relDir+"/scratch/tmp")
}

// NoAmbientEnv is an EnvLookup that finds nothing. Tests use it so a fixture's frozen environment does
// not depend on the machine the suite happens to run on.
func NoAmbientEnv(string) (string, bool) { return "", false }

// HostGOOS is the platform resolution happens on. Named so tests can state the platform explicitly
// rather than depending on where they run.
func HostGOOS() string { return runtime.GOOS }
