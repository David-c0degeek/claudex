package config

import (
	"encoding/json"
	"reflect"
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

const testRelDir = ".claudex/runs/run-a"

func resolve(t *testing.T, g TestGate, goos string, look EnvLookup) ResolvedExecution {
	t.Helper()
	re, err := ResolveForRun(g, goos, look, testRelDir)
	if err != nil {
		t.Fatalf("ResolveForRun: %v", err)
	}
	return re
}

func envNames(re ResolvedExecution) []string {
	out := make([]string, 0, len(re.Env))
	for _, e := range re.Env {
		out = append(out, string(e.Name))
	}
	return out
}

func envValue(re ResolvedExecution, name string) (string, bool) {
	for _, e := range re.Env {
		if string(e.Name) == name {
			return string(e.Value), true
		}
	}
	return "", false
}

// TestResolutionFreezesTheValueNotJustTheName is the point of the whole shape. Allowlisting a name
// makes inheritance INTENTIONAL; it does not stop the value changing underneath the run.
func TestResolutionFreezesTheValueNotJustTheName(t *testing.T) {
	re := resolve(t, gateInherit("PATH"), "linux", host(map[string]string{"PATH": "/usr/bin"}))
	if v, ok := envValue(re, "PATH"); !ok || v != "/usr/bin" {
		t.Fatalf("env = %v, want the host's PATH frozen", envNames(re))
	}
}

// TestArbitraryBytesSurviveTheWire.
//
// A Unix environment value is an arbitrary byte string that need not be valid UTF-8, and Go's JSON
// encoder silently REPLACES invalid bytes. Persisting the execution view as JSON strings would
// therefore bind something the child never receives — the same defect the evidence packet had to be
// fixed for over git paths. This is why the execution view has its own base64 shape while the source
// policy keeps plain UTF-8.
func TestArbitraryBytesSurviveTheWire(t *testing.T) {
	raw := []byte{0xff, 0xfe, 'a', 0x80}
	re := ResolvedExecution{
		Identity: NameByteExact,
		Env:      []ResolvedVar{{Name: []byte("WEIRD"), Value: raw}},
	}
	p := scratchPaths(testRelDir)
	re.ScratchHome, re.ScratchCache, re.ScratchTemp = p[0], p[1], p[2]

	b, err := json.Marshal(re)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ResolvedExecution
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.Env[0].Value, raw) {
		t.Fatalf("value round-tripped as %v, want %v — invalid UTF-8 was not preserved", got.Env[0].Value, raw)
	}
	// And the plain-string shape really would have lost it, so this test is about a live hazard.
	if string([]rune(string(raw))) == string(raw) {
		t.Fatal("the fixture is valid UTF-8; it proves nothing about byte preservation")
	}
}

// TestWindowsNameIdentityCollisions is the TURN-240 pin.
//
// Windows supplies ONE variable for `Path` and `PATH`. An exact-spelling allowlist would freeze two
// entries while the child received one, so the durable identity would describe an environment that
// never existed. The same document is perfectly valid on Linux, where they ARE two variables — which is
// why the check belongs to resolution, where the platform is known, and not to the portable document.
func TestWindowsNameIdentityCollisions(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate TestGate
	}{
		{"two inherited spellings", gateInherit("Path", "PATH")},
		{"inherited and set", TestGate{Argv: []string{"go", "test"},
			Env: TestGateEnv{Inherit: []string{"Path"}, Set: []EnvAssignment{{Name: "PATH", Value: "x"}}}}},
		{"two set spellings", TestGate{Argv: []string{"go", "test"},
			Env: TestGateEnv{Set: []EnvAssignment{{Name: "Ci", Value: "1"}, {Name: "CI", Value: "2"}}}}},
		{"a differently-spelled platform name", gateInherit("systemroot")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveForRun(tc.gate, "windows", host(map[string]string{"PATH": "p", "Path": "p", "systemroot": "s"}), testRelDir); err == nil {
				t.Fatal("windows resolution accepted two spellings of one variable")
			}
			// The very same policy is fine on Linux, where they are genuinely distinct.
			if _, err := ResolveForRun(tc.gate, "linux", host(map[string]string{"PATH": "p", "Path": "p", "systemroot": "s", "CI": "1", "Ci": "2"}), testRelDir); err != nil {
				t.Fatalf("linux resolution refused a policy that is valid there: %v", err)
			}
		})
	}
}

// TestFrozenIdentityIsNotReinterpretedByAnotherHost.
//
// A value frozen under one platform's name rules says nothing about what the other would have produced,
// so a Linux-origin environment containing only PATH must not be re-validated as valid Windows state.
func TestFrozenIdentityIsNotReinterpretedByAnotherHost(t *testing.T) {
	g := gateInherit("PATH")
	lin := resolve(t, g, "linux", host(map[string]string{"PATH": "/usr/bin"}))
	if lin.Identity != NameByteExact {
		t.Fatalf("identity = %q, want byte_exact", lin.Identity)
	}
	err := lin.ValidateFor(g, "windows", testRelDir)
	if err == nil || !strings.Contains(err.Error(), "different platform name rules") {
		t.Fatalf("err = %v, want a host-identity refusal", err)
	}
}

// TestPlatformRequiredNamesAreAuthorizedNotInjected. The Windows names must be part of the AUTHORITY —
// reachable through AuthorizedEnvNames and therefore accepted by validation — rather than added during
// resolution to a policy that never mentioned them.
func TestPlatformRequiredNamesAreAuthorizedNotInjected(t *testing.T) {
	g := gateInherit("PATH")
	h := host(map[string]string{"PATH": `C:\bin`, "SystemRoot": `C:\Windows`, "ComSpec": `C:\Windows\cmd.exe`, "PATHEXT": ".EXE"})

	win := resolve(t, g, "windows", h)
	for _, want := range []string{"ComSpec", "PATH", "PATHEXT", "SystemRoot"} {
		if _, ok := envValue(win, want); !ok {
			t.Fatalf("windows resolution %v omits the platform-required %q", envNames(win), want)
		}
	}
	lin := resolve(t, g, "linux", h)
	if len(lin.Env) != 1 || string(lin.Env[0].Name) != "PATH" {
		t.Fatalf("linux resolution = %v, want only PATH", envNames(lin))
	}
	rogue := win
	rogue.Env = append(append([]ResolvedVar{}, win.Env...), ResolvedVar{Name: []byte("SECRET"), Value: []byte("x")})
	if err := rogue.ValidateFor(g, "windows", testRelDir); err == nil || !strings.Contains(err.Error(), "does not authorize") {
		t.Fatalf("err = %v, want an authorization refusal", err)
	}
}

// TestThePlatformSetCannotBeEnlargedByACaller. A closed set that any importer could append to would be
// a claim the type system contradicts.
func TestThePlatformSetCannotBeEnlargedByACaller(t *testing.T) {
	got := PlatformRequired("windows")
	got = append(got, "SECRET")
	_ = got
	for _, n := range PlatformRequired("windows") {
		if n == "SECRET" {
			t.Fatal("PlatformRequired handed out its own backing array; the authority is mutable")
		}
	}
}

// TestExplicitlySetValuesAreBoundExactly.
//
// Requiring only that a `set` NAME be present let a forged or recovered intent substitute any value it
// liked while passing validation — the policy said what the value is, so validation must say so too.
func TestExplicitlySetValuesAreBoundExactly(t *testing.T) {
	g := TestGate{Argv: []string{"go", "test"}, Env: TestGateEnv{Set: []EnvAssignment{{Name: "CI", Value: "1"}}}}
	re := resolve(t, g, "linux", host(map[string]string{"CI": "ambient"}))
	if v, _ := envValue(re, "CI"); v != "1" {
		t.Fatalf("an ambient value overrode an explicitly set one: %q", v)
	}
	forged := re
	forged.Env = []ResolvedVar{{Name: []byte("CI"), Value: []byte("tampered")}}
	if err := forged.ValidateFor(g, "linux", testRelDir); err == nil || !strings.Contains(err.Error(), "not the value the policy sets") {
		t.Fatalf("err = %v, want a substituted-value refusal", err)
	}
	missing := ResolvedExecution{Identity: NameByteExact, Env: []ResolvedVar{}}
	p := scratchPaths(testRelDir)
	missing.ScratchHome, missing.ScratchCache, missing.ScratchTemp = p[0], p[1], p[2]
	if err := missing.ValidateFor(g, "linux", testRelDir); err == nil || !strings.Contains(err.Error(), "sets explicitly") {
		t.Fatalf("err = %v, want a refusal for an omitted set entry", err)
	}
}

// TestScratchPathsMustBeTheDerivedLayout.
//
// Checking only that the three paths are non-blank let a forged journal point HOME, cache and temp
// anywhere on the machine — defeating the credential isolation those directories exist to provide,
// while still satisfying "the paths are set".
func TestScratchPathsMustBeTheDerivedLayout(t *testing.T) {
	g := gateInherit("PATH")
	re := resolve(t, g, "linux", host(map[string]string{"PATH": "/usr/bin"}))
	for _, tc := range []struct {
		name  string
		tweak func(*ResolvedExecution)
	}{
		{"home redirected outside the run", func(r *ResolvedExecution) { r.ScratchHome = "/home/operator" }},
		{"cache redirected outside the run", func(r *ResolvedExecution) { r.ScratchCache = "/tmp/elsewhere" }},
		{"temp redirected outside the run", func(r *ResolvedExecution) { r.ScratchTemp = "/tmp/elsewhere" }},
		{"another run's layout", func(r *ResolvedExecution) { *r, _ = ResolveExecutionForTest(g, ".claudex/runs/run-b") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := re
			tc.tweak(&forged)
			if err := forged.ValidateFor(g, "linux", testRelDir); err == nil || !strings.Contains(err.Error(), "derived layout") {
				t.Fatalf("err = %v, want a scratch-layout refusal", err)
			}
		})
	}
}

// ResolveExecutionForTest builds a resolution for another run directory, used to prove one run's layout
// is refused for a different run.
func ResolveExecutionForTest(g TestGate, relDir string) (ResolvedExecution, error) {
	return ResolveForRun(g, "linux", host(map[string]string{"PATH": "/usr/bin"}), relDir)
}

// TestResolvedSecretsAreRefusedBeforeAnythingIsPersisted.
//
// The transaction durably appends the whole intent before any participant runs, so a credential-shaped
// value must be refused HERE — at resolution — or it reaches the journal before any later guard can
// object. Refused rather than redacted: redaction collapses two distinct secrets to one marker, so a
// digest over redacted values would bind something the command never received.
func TestResolvedSecretsAreRefusedBeforeAnythingIsPersisted(t *testing.T) {
	const secret = "sk-ant-abcdefghijklmnopqrstuvwx"
	t.Run("inherited from the host", func(t *testing.T) {
		_, err := ResolveForRun(gateInherit("TOKEN"), "linux", host(map[string]string{"TOKEN": secret}), testRelDir)
		if err == nil || !strings.Contains(err.Error(), "looks like a credential") {
			t.Fatalf("err = %v, want a credential refusal", err)
		}
	})
	t.Run("stated in the policy", func(t *testing.T) {
		g := TestGate{Argv: []string{"go", "test"}, Env: TestGateEnv{Set: []EnvAssignment{{Name: "TOKEN", Value: secret}}}}
		_, err := ResolveForRun(g, "linux", host(nil), testRelDir)
		if err == nil || !strings.Contains(err.Error(), "looks like a credential") {
			t.Fatalf("err = %v, want a credential refusal", err)
		}
	})
	// A CREDENTIAL-SHAPED NAME is refused whatever its value, and that is a deliberate false positive
	// rather than an oversight. internal/redact is explicitly heuristic, this project REFUSES rather
	// than redacts, and the two together mean the conservative direction is the only sound one: a
	// variable called TOKEN whose value happens to be innocuous today is one policy edit away from
	// carrying a real credential into a durable journal. An operator who needs such a name gets a clear
	// refusal instead of a run whose bootstrap record may hold their secret.
	t.Run("a credential-shaped name is refused whatever the value", func(t *testing.T) {
		_, err := ResolveForRun(gateInherit("TOKEN"), "linux", host(map[string]string{"TOKEN": "ordinary"}), testRelDir)
		if err == nil || !strings.Contains(err.Error(), "looks like a credential") {
			t.Fatalf("err = %v, want the conservative refusal", err)
		}
	})
	t.Run("an ordinary variable is not refused", func(t *testing.T) {
		if _, err := ResolveForRun(gateInherit("CI"), "linux", host(map[string]string{"CI": "1"}), testRelDir); err != nil {
			t.Fatalf("an ordinary variable was refused: %v", err)
		}
	})
}

// TestResolvedEnvironmentIsBoundedByItsENCODEDSize.
//
// The binding constraint is the transaction payload, which already carries two base64-expanded 16 KiB
// snapshots. A raw-byte ceiling said nothing about what the payload would actually hold, and the one
// that was there (32 KiB raw) could not have fitted alongside the snapshots at all.
func TestResolvedEnvironmentIsBoundedByItsENCODEDSize(t *testing.T) {
	t.Run("one oversized value", func(t *testing.T) {
		_, err := ResolveForRun(gateInherit("BIG"), "linux",
			host(map[string]string{"BIG": strings.Repeat("x", MaxResolvedEnvValue+1)}), testRelDir)
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("err = %v, want a per-value ceiling refusal", err)
		}
	})
	t.Run("aggregate wire size", func(t *testing.T) {
		var names []string
		vals := map[string]string{}
		for i := 0; i < 8; i++ {
			n := "VAR" + string(rune('A'+i))
			names = append(names, n)
			vals[n] = strings.Repeat("y", MaxResolvedEnvValue)
		}
		_, err := ResolveForRun(gateInherit(names...), "linux", host(vals), testRelDir)
		if err == nil || !strings.Contains(err.Error(), "encodes to") {
			t.Fatalf("err = %v, want an encoded-size refusal", err)
		}
	})
	t.Run("too many names", func(t *testing.T) {
		var names []string
		vals := map[string]string{}
		for i := 0; i <= MaxResolvedEnvNames; i++ {
			n := "V" + string(rune('A'+i/26)) + string(rune('a'+i%26))
			names = append(names, n)
			vals[n] = "v"
		}
		_, err := ResolveForRun(gateInherit(names...), "linux", host(vals), testRelDir)
		if err == nil || !strings.Contains(err.Error(), "environment names, limit") {
			t.Fatalf("err = %v, want a name-count refusal", err)
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
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("resolution is not deterministic:\n%+v\n%+v", first, second)
	}
	for i := 1; i < len(first.Env); i++ {
		if string(first.Env[i-1].Name) >= string(first.Env[i].Name) {
			t.Fatalf("resolution is not sorted: %v", envNames(first))
		}
	}
	unsorted := first
	unsorted.Env = []ResolvedVar{{Name: []byte("PATH"), Value: []byte("/usr/bin")}, {Name: []byte("CI"), Value: []byte("1")}}
	if err := unsorted.ValidateFor(g, "linux", testRelDir); err == nil || !strings.Contains(err.Error(), "not sorted") {
		t.Fatalf("err = %v, want a canonical-order refusal", err)
	}
}

// TestDisabledGateResolvesNoEnvironment. Nothing will run, so nothing is authorized — but the scratch
// layout is still recorded, so enabling the gate later is a policy change rather than a shape change.
func TestDisabledGateResolvesNoEnvironment(t *testing.T) {
	re := resolve(t, TestGate{Disabled: true}, "windows", host(map[string]string{"SystemRoot": `C:\Windows`}))
	if len(re.Env) != 0 {
		t.Fatalf("a disabled gate resolved %v", envNames(re))
	}
	if re.ScratchHome == "" || re.ScratchCache == "" || re.ScratchTemp == "" {
		t.Fatalf("scratch layout missing: %+v", re)
	}
}

// TestTheExecutionViewDecoderIsStrictAndCanonical.
//
// A custom unmarshaller opts OUT of the caller's decoder settings, so the DisallowUnknownFields that
// bootstrap and state decoding rely on never reaches these nested fields. Left plain, one execution
// identity would have many valid durable byte shapes, and a future or stale field would be discarded at
// recovery without anyone noticing.
func TestTheExecutionViewDecoderIsStrictAndCanonical(t *testing.T) {
	p := scratchPaths(testRelDir)
	base := ResolvedExecution{
		Identity:    NameByteExact,
		Env:         []ResolvedVar{{Name: []byte("PATH"), Value: []byte("/usr/bin")}},
		ScratchHome: p[0], ScratchCache: p[1], ScratchTemp: p[2],
	}
	valid, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round ResolvedExecution
	if err := json.Unmarshal(valid, &round); err != nil {
		t.Fatalf("the canonical encoding was refused by its own decoder: %v", err)
	}
	if !reflect.DeepEqual(round, base) {
		t.Fatalf("round trip changed the value: %+v -> %+v", base, round)
	}

	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{
			"an unknown field",
			strings.Replace(string(valid), `{"identity"`, `{"unknown":"accepted","identity"`, 1),
			"unknown field",
		},
		{
			"a null collapsed to empty",
			strings.Replace(string(valid), `"env":[`, `"env":null,"ignored":[`, 1),
			"null",
		},
		{
			"a duplicate key",
			strings.Replace(string(valid), `{"identity"`, `{"identity":"byte_exact","identity"`, 1),
			"duplicate key",
		},
		{
			"a case-aliased key",
			strings.Replace(string(valid), `"scratch_home"`, `"Scratch_Home"`, 1),
			"non-canonical key spelling",
		},
		{
			"trailing content",
			string(valid) + " x",
			"after top-level value",
		},
		{
			// encoding/base64 accepts embedded newlines, which decode to identical bytes — so one
			// identity would have several valid spellings and a digest over the carrier would bind the
			// spelling rather than the content.
			"non-canonical base64",
			// The newline is JSON-ESCAPED, so the decoded field really contains one — which
			// encoding/base64 happily accepts, decoding to the same bytes as the canonical spelling.
			strings.Replace(string(valid), `"UEFUSA=="`, `"UEFU\nSA=="`, 1),
			"canonical encoding",
		},
		{
			"a missing required field",
			strings.Replace(string(valid), `,"scratch_temp":"`+p[2]+`"`, ``, 1),
			"required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ResolvedExecution
			err := json.Unmarshal([]byte(tc.doc), &got)
			if err == nil {
				t.Fatalf("accepted %s: %s", tc.name, tc.doc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestADisabledGateStillRefusesAStatedSecret.
//
// Resolution used to skip the whole environment path when the gate was disabled, so no credential check
// ran — while the value sat in PolicyCanonical and EffectivePolicy either way, and txn.Run durably
// appends the intent before any participant executes. A credential reached the journal precisely
// BECAUSE nothing was going to use it.
func TestADisabledGateStillRefusesAStatedSecret(t *testing.T) {
	g := TestGate{Disabled: true, Env: TestGateEnv{Set: []EnvAssignment{{Name: "TOKEN", Value: "ordinary"}}}}
	if _, err := ResolveForRun(g, "linux", host(nil), testRelDir); err == nil {
		t.Fatal("a disabled gate carrying a credential-shaped entry was accepted")
	} else if !strings.Contains(err.Error(), "looks like a credential") {
		t.Fatalf("err = %v, want a credential refusal", err)
	}
	// The validator agrees, so a value that reached state by some other path is refused too.
	ok := ResolvedExecution{Identity: NameByteExact, Env: []ResolvedVar{}}
	pp := scratchPaths(testRelDir)
	ok.ScratchHome, ok.ScratchCache, ok.ScratchTemp = pp[0], pp[1], pp[2]
	if err := ok.ValidateFor(g, "linux", testRelDir); err == nil || !strings.Contains(err.Error(), "looks like a credential") {
		t.Fatalf("err = %v, want the validator to refuse it as well", err)
	}
}
