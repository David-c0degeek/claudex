package config

import (
	"slices"
	"strings"
	"testing"
)

func gateInherit(names ...string) TestGate {
	return TestGate{Argv: []string{"go", "test"}, Env: TestGateEnv{Inherit: names}}
}

func host(vals map[string]string) EnvLookup {
	return func(n string) (string, bool) {
		v, ok := vals[n]
		return v, ok
	}
}

func resolve(t *testing.T, g TestGate, goos string, look EnvLookup) ResolvedExecution {
	t.Helper()
	re, err := ResolveForRun(g, goos, look, ".claudex/runs/run-a")
	if err != nil {
		t.Fatalf("ResolveForRun: %v", err)
	}
	return re
}

// TestResolutionFreezesTheValueNotJustTheName is the point of the whole shape.
//
// Allowlisting a name makes inheritance INTENTIONAL; it does not stop the value changing underneath the
// run. Resolution reads the host once, and everything afterwards reads the frozen copy.
func TestResolutionFreezesTheValueNotJustTheName(t *testing.T) {
	re := resolve(t, gateInherit("PATH"), "linux", host(map[string]string{"PATH": "/usr/bin"}))
	want := []EnvAssignment{{Name: "PATH", Value: "/usr/bin"}}
	if !slices.Equal(re.Env, want) {
		t.Fatalf("env = %+v, want %+v", re.Env, want)
	}
}

// TestAnAbsentInheritedNameStaysAbsent. "Set to empty" and "not set" are distinguishable to a program,
// so inventing an empty value would hand the command a variable the host never had.
func TestAnAbsentInheritedNameStaysAbsent(t *testing.T) {
	re := resolve(t, gateInherit("PATH", "CI"), "linux", host(map[string]string{"PATH": "/usr/bin"}))
	for _, e := range re.Env {
		if e.Name == "CI" {
			t.Fatalf("an absent inherited name was materialized as %q", e.Value)
		}
	}
}

// TestPlatformRequiredNamesAreAuthorizedNotInjected is CX's pin 5 made executable.
//
// The Windows names must be part of the AUTHORITY — reachable through AuthorizedEnvNames and therefore
// accepted by validation — rather than added during resolution to a policy that never mentioned them.
// The difference is observable: an implicitly added value would be refused by ValidateFor, which is
// exactly what makes "nothing unnamed reaches the command" true rather than aspirational.
func TestPlatformRequiredNamesAreAuthorizedNotInjected(t *testing.T) {
	g := gateInherit("PATH")
	h := host(map[string]string{"PATH": `C:\bin`, "SystemRoot": `C:\Windows`, "ComSpec": `C:\Windows\cmd.exe`, "PATHEXT": ".EXE"})

	win := resolve(t, g, "windows", h)
	var names []string
	for _, e := range win.Env {
		names = append(names, e.Name)
	}
	for _, want := range []string{"ComSpec", "PATH", "PATHEXT", "SystemRoot"} {
		if !slices.Contains(names, want) {
			t.Fatalf("windows resolution %v omits the platform-required %q", names, want)
		}
	}
	// The same policy on Linux carries only what it named: the platform set is per-GOOS, not a global
	// enlargement of every policy.
	lin := resolve(t, g, "linux", h)
	if len(lin.Env) != 1 || lin.Env[0].Name != "PATH" {
		t.Fatalf("linux resolution = %+v, want only PATH", lin.Env)
	}
	// And a value the policy never authorized is refused rather than recorded.
	rogue := win
	rogue.Env = append(slices.Clone(win.Env), EnvAssignment{Name: "SECRET", Value: "x"})
	slices.SortFunc(rogue.Env, func(a, b EnvAssignment) int { return strings.Compare(a.Name, b.Name) })
	if err := rogue.ValidateFor(g, "windows"); err == nil || !strings.Contains(err.Error(), "does not authorize") {
		t.Fatalf("err = %v, want an authorization refusal", err)
	}
}

// TestExplicitlySetNamesMustSurviveResolution. A `set` entry is stated by the policy, so unlike an
// inherited name it cannot legitimately be missing afterwards.
func TestExplicitlySetNamesMustSurviveResolution(t *testing.T) {
	g := TestGate{Argv: []string{"go", "test"}, Env: TestGateEnv{Set: []EnvAssignment{{Name: "CI", Value: "1"}}}}
	re := resolve(t, g, "linux", host(nil))
	if !slices.Equal(re.Env, []EnvAssignment{{Name: "CI", Value: "1"}}) {
		t.Fatalf("env = %+v, want the explicitly set entry", re.Env)
	}
	// A set value does NOT come from the host, so an ambient value of the same name cannot win.
	re = resolve(t, g, "linux", host(map[string]string{"CI": "ambient"}))
	if re.Env[0].Value != "1" {
		t.Fatalf("an ambient value overrode an explicitly set one: %q", re.Env[0].Value)
	}
	missing := ResolvedExecution{Env: []EnvAssignment{}, ScratchHome: "h", ScratchCache: "c", ScratchTemp: "t"}
	if err := missing.ValidateFor(g, "linux"); err == nil || !strings.Contains(err.Error(), "sets explicitly") {
		t.Fatalf("err = %v, want a refusal for an omitted set entry", err)
	}
}

// TestResolvedEnvironmentIsBoundedBeforeAnythingIsJournaled.
//
// An inherited value is not bounded by the 16 KiB policy source — it comes from the host — while a
// prepared-transaction payload caps at 64 KiB. Without these ceilings a small, valid policy could
// resolve to an intent that cannot be written, discovered only once the run was already being created.
func TestResolvedEnvironmentIsBoundedBeforeAnythingIsJournaled(t *testing.T) {
	t.Run("one oversized value", func(t *testing.T) {
		g := gateInherit("BIG")
		_, err := ResolveForRun(g, "linux", host(map[string]string{"BIG": strings.Repeat("x", MaxResolvedEnvValue+1)}), ".claudex/runs/run-a")
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("err = %v, want a per-value ceiling refusal", err)
		}
	})
	t.Run("too many names", func(t *testing.T) {
		var names []string
		vals := map[string]string{}
		for i := 0; i <= MaxResolvedEnvNames; i++ {
			n := "VAR_" + strings.Repeat("A", i%20) + string(rune('a'+i%26)) + string(rune('a'+i/26))
			names = append(names, n)
			vals[n] = "v"
		}
		_, err := ResolveForRun(gateInherit(names...), "linux", host(vals), ".claudex/runs/run-a")
		if err == nil || !strings.Contains(err.Error(), "environment names, limit") {
			t.Fatalf("err = %v, want a name-count refusal", err)
		}
	})
	t.Run("aggregate too large", func(t *testing.T) {
		var names []string
		vals := map[string]string{}
		for i := 0; i < 16; i++ {
			n := "VAR" + string(rune('A'+i))
			names = append(names, n)
			vals[n] = strings.Repeat("y", MaxResolvedEnvValue)
		}
		_, err := ResolveForRun(gateInherit(names...), "linux", host(vals), ".claudex/runs/run-a")
		if err == nil || !strings.Contains(err.Error(), "environment is") {
			t.Fatalf("err = %v, want an aggregate ceiling refusal", err)
		}
	})
}

// TestResolutionIsCanonical. Two resolutions of the same policy against the same host must be
// byte-identical, or the digest bound in an attempt would depend on iteration order.
func TestResolutionIsCanonical(t *testing.T) {
	g := gateInherit("PATH", "CI", "ALPHA")
	h := host(map[string]string{"PATH": "/usr/bin", "CI": "1", "ALPHA": "a"})
	first := resolve(t, g, "linux", h)
	second := resolve(t, g, "linux", h)
	if !slices.Equal(first.Env, second.Env) {
		t.Fatalf("resolution is not deterministic:\n%+v\n%+v", first.Env, second.Env)
	}
	for i := 1; i < len(first.Env); i++ {
		if first.Env[i-1].Name >= first.Env[i].Name {
			t.Fatalf("resolution is not sorted: %+v", first.Env)
		}
	}
	unsorted := ResolvedExecution{
		Env:         []EnvAssignment{{Name: "PATH", Value: "/usr/bin"}, {Name: "CI", Value: "1"}},
		ScratchHome: "h", ScratchCache: "c", ScratchTemp: "t",
	}
	if err := unsorted.ValidateFor(g, "linux"); err == nil || !strings.Contains(err.Error(), "not sorted") {
		t.Fatalf("err = %v, want a canonical-order refusal", err)
	}
}

// TestDisabledGateResolvesNoEnvironment. Nothing will run, so nothing is authorized — but the scratch
// layout is still recorded, so enabling the gate later is a policy change rather than a shape change.
func TestDisabledGateResolvesNoEnvironment(t *testing.T) {
	re := resolve(t, TestGate{Disabled: true}, "windows", host(map[string]string{"SystemRoot": `C:\Windows`}))
	if len(re.Env) != 0 {
		t.Fatalf("a disabled gate resolved %+v", re.Env)
	}
	if re.ScratchHome == "" || re.ScratchCache == "" || re.ScratchTemp == "" {
		t.Fatalf("scratch layout missing: %+v", re)
	}
}
