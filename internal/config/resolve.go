package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"

	"github.com/David-c0degeek/claudex/internal/redact"
)

// NameIdentity is how the platform decides whether two environment names are the SAME variable.
//
// It is persisted rather than recomputed, because it is a property of the host that RESOLVED the
// environment, not of the host that later reads it. Without it a Linux-origin value carrying only
// `PATH` could be re-validated on Windows and silently accepted under different rules — the frozen
// identity would then be interpreted by a rule that was never applied to produce it.
type NameIdentity string

const (
	// NameByteExact is the Unix rule: names differing in any byte are different variables.
	NameByteExact NameIdentity = "byte_exact"
	// NameASCIIFold is the Windows rule: names are the same variable when they are equal after ASCII
	// upper-casing, because the OS supplies ONE variable for `Path` and `PATH`. The name grammar is
	// restricted to ASCII precisely so this fold is a plain A-Z mapping that validation and
	// child-environment construction cannot implement differently.
	NameASCIIFold NameIdentity = "ascii_fold"
)

// ErrHostIdentityMismatch means a frozen environment was resolved under a different platform's name
// rules than the host now reading it. It is fail-closed: the run cannot be executed or recovered here.
var ErrHostIdentityMismatch = errors.New("config: resolved execution was frozen under different platform name rules")

// IdentityFor reports the name-identity rule a GOOS uses.
func IdentityFor(goos string) NameIdentity {
	if goos == "windows" {
		return NameASCIIFold
	}
	return NameByteExact
}

// FoldName produces the comparison key for a name under an identity rule.
func FoldName(name string, id NameIdentity) string {
	if id != NameASCIIFold {
		return name
	}
	b := []byte(name)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}

// ResolvedVar is one frozen environment entry, carried as RAW BYTES.
//
// It is a distinct type from EnvAssignment, and the duplication is deliberate. The source policy is a
// hand-authored document, so plain UTF-8 strings are right there. This is an EXECUTION VIEW: an
// inherited Unix value is an arbitrary byte string that need not be valid UTF-8, and Go's JSON encoder
// silently replaces invalid bytes — so persisting it as a JSON string would bind something the child
// never receives, which is precisely the defect the evidence packet had to be fixed for over git paths.
type ResolvedVar struct {
	Name  []byte
	Value []byte
}

type wireResolvedVar struct {
	NameB64  string `json:"name_b64"`
	ValueB64 string `json:"value_b64"`
}

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
	// Identity is the platform name rule this was resolved under, persisted so a later host cannot
	// reinterpret it.
	Identity NameIdentity
	// Env is the resolved environment, sorted by the identity's comparison key.
	Env []ResolvedVar
	// ScratchHome, ScratchCache and ScratchTemp are tool-owned per-run directories, derived exactly
	// from the run directory. They exist because HOME is not inherited: tools that insist on one get a
	// directory inside the run rather than the operator's, which is what keeps credential-bearing
	// config out of the command's reach.
	ScratchHome  string
	ScratchCache string
	ScratchTemp  string
}

type wireResolvedExecution struct {
	Identity     string            `json:"identity"`
	Env          []wireResolvedVar `json:"env"`
	ScratchHome  string            `json:"scratch_home"`
	ScratchCache string            `json:"scratch_cache"`
	ScratchTemp  string            `json:"scratch_temp"`
}

// MarshalJSON writes names and values base64-encoded, so arbitrary bytes survive the round trip.
func (re ResolvedExecution) MarshalJSON() ([]byte, error) {
	w := wireResolvedExecution{
		Identity:     string(re.Identity),
		Env:          make([]wireResolvedVar, 0, len(re.Env)),
		ScratchHome:  re.ScratchHome,
		ScratchCache: re.ScratchCache,
		ScratchTemp:  re.ScratchTemp,
	}
	for _, e := range re.Env {
		w.Env = append(w.Env, wireResolvedVar{
			NameB64:  base64.StdEncoding.EncodeToString(e.Name),
			ValueB64: base64.StdEncoding.EncodeToString(e.Value),
		})
	}
	return json.Marshal(w)
}

// UnmarshalJSON is STRICT and CANONICAL, and it has to implement both itself.
//
// A custom unmarshaller opts out of the caller's decoder settings: the DisallowUnknownFields that
// bootstrap and state decoding rely on does not reach these nested fields, so without this the carrier
// would silently accept an unknown or case-aliased field, a duplicate key, a null collapsed to empty,
// and non-canonical base64 with embedded newlines. Each of those gives ONE execution identity several
// durable byte shapes — and a future or stale field would be discarded at recovery without anyone
// noticing.
func (re *ResolvedExecution) UnmarshalJSON(data []byte) error {
	// The package's own walker: duplicate keys, non-canonical key spellings, explicit nulls and
	// trailing content, at every depth.
	if err := checkStrictJSON(data); err != nil {
		return fmt.Errorf("resolved execution: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w wireResolvedExecution
	if err := dec.Decode(&w); err != nil {
		return fmt.Errorf("resolved execution: %w", err)
	}
	// Every field REQUIRED, present explicitly. An absent `env` decoding to empty would make "no
	// environment" and "the field was lost" the same document.
	present := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &present); err != nil {
		return fmt.Errorf("resolved execution: %w", err)
	}
	for _, k := range []string{"identity", "env", "scratch_home", "scratch_cache", "scratch_temp"} {
		if _, ok := present[k]; !ok {
			return fmt.Errorf("resolved execution: %s is required", k)
		}
	}
	out := ResolvedExecution{
		Identity:     NameIdentity(w.Identity),
		Env:          make([]ResolvedVar, 0, len(w.Env)),
		ScratchHome:  w.ScratchHome,
		ScratchCache: w.ScratchCache,
		ScratchTemp:  w.ScratchTemp,
	}
	for i, e := range w.Env {
		n, err := decodeCanonicalB64(e.NameB64)
		if err != nil {
			return fmt.Errorf("resolved execution env[%d] name: %w", i, err)
		}
		v, err := decodeCanonicalB64(e.ValueB64)
		if err != nil {
			return fmt.Errorf("resolved execution env[%d] value: %w", i, err)
		}
		out.Env = append(out.Env, ResolvedVar{Name: n, Value: v})
	}
	*re = out
	return nil
}

// decodeCanonicalB64 decodes base64 and requires the spelling to be the ONE canonical encoding of its
// own bytes.
//
// encoding/base64 accepts variants — embedded CR/LF among them — that decode to identical bytes. Left
// alone, one execution identity would have many valid durable spellings, so a digest over the carrier
// would bind the spelling rather than the content. Re-encoding and comparing is the whole check.
func decodeCanonicalB64(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if base64.StdEncoding.EncodeToString(b) != s {
		return nil, fmt.Errorf("base64 is not the canonical encoding of its own bytes")
	}
	return b, nil
}

// Resolution ceilings, validated at attach BEFORE anything is journaled.
//
// The policy source is bounded at 16 KiB, but an INHERITED value is not bounded by it at all — it comes
// from the host and can be arbitrarily large. The binding constraint is the prepared-transaction
// payload, which caps at 64 KiB and already carries two separately base64-expanded 16 KiB snapshots
// (2 * 16384 * 4/3 ≈ 43.7 KiB) plus the effective policy. So the ceiling that matters is the ENCODED
// size of this value, not its raw byte count: MaxResolvedEnvTotalByte once permitted 32 KiB of raw
// bytes, which cannot fit alongside the snapshots at all, and control bytes could expand further still.
// TestBootstrapIntentFitsItsPayloadCeiling proves the whole intent fits at every admitted maximum.
const (
	MaxResolvedEnvNames = 64
	MaxResolvedEnvValue = 4096
	// MaxResolvedExecutionWireBytes bounds the marshaled JSON of this value.
	MaxResolvedExecutionWireBytes = 6 * 1024
)

// EnvLookup reads an ambient variable. It is a parameter so resolution is testable without mutating the
// process environment, and so the one place that touches ambient state is explicit.
type EnvLookup func(name string) (string, bool)

// AuthorizedEnvNames is the closed set of names a run may carry: what the operator allowlisted, plus the
// enumerated platform-required set for this GOOS, deduplicated under the platform's identity rule.
//
// The union is the AUTHORITY, stated here rather than assembled during resolution. An earlier draft had
// resolution quietly add the Windows names, which contradicted the policy's own claim that nothing
// unnamed reaches the command — recording an implicitly added value does not make it authorized.
//
// It returns an error rather than a set when two names COLLIDE under the platform rule: on Windows
// `Path` and `PATH` are one variable to the OS but two entries to any exact-spelling list, so a
// resolution built from that list would freeze two entries while the child received one, and the
// durable identity would describe an environment that never existed.
func AuthorizedEnvNames(g TestGate, goos string) ([]string, error) {
	id := IdentityFor(goos)
	byKey := map[string]string{}
	var out []string
	add := func(n, where string) error {
		k := FoldName(n, id)
		if prev, dup := byKey[k]; dup {
			if prev == n {
				return nil
			}
			return fmt.Errorf("%s names %s, which is the same variable as %s under %s name rules", where, n, prev, goos)
		}
		byKey[k] = n
		out = append(out, n)
		return nil
	}
	for _, n := range g.Env.Inherit {
		if err := add(n, "test_gate.env.inherit"); err != nil {
			return nil, err
		}
	}
	for _, e := range g.Env.Set {
		if err := add(e.Name, "test_gate.env.set"); err != nil {
			return nil, err
		}
	}
	for _, n := range PlatformRequired(goos) {
		// A platform name the operator already allowlisted in the same spelling is redundant, not an
		// error; a DIFFERENT spelling of it is the collision above.
		if err := add(n, "the platform-required set"); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return FoldName(out[i], id) < FoldName(out[j], id) })
	return out, nil
}

// ResolveExecution freezes the environment for a run.
//
// An inherited name that is ABSENT from the host is simply absent from the result rather than resolved
// to an empty string. The two are distinguishable to a program — `os.LookupEnv` reports which — and
// inventing an empty value would hand the command a variable the host never had.
func ResolveExecution(g TestGate, goos string, look EnvLookup, scratchHome, scratchCache, scratchTemp string) (ResolvedExecution, error) {
	// Checked BEFORE the disabled branch, because a stated secret is persisted whether or not it will
	// ever execute: the value sits in PolicyCanonical and EffectivePolicy, and txn.Run durably appends
	// the whole intent before any participant runs. Skipping the check for a disabled gate meant a
	// credential reached the journal precisely because nothing was going to use it.
	for _, e := range g.Env.Set {
		if err := refuseCredential(e.Name, e.Value); err != nil {
			return ResolvedExecution{}, err
		}
	}
	re := ResolvedExecution{Identity: IdentityFor(goos), Env: []ResolvedVar{}}
	if !g.Disabled {
		names, err := AuthorizedEnvNames(g, goos)
		if err != nil {
			return ResolvedExecution{}, err
		}
		set := make(map[string]string, len(g.Env.Set))
		for _, e := range g.Env.Set {
			set[FoldName(e.Name, re.Identity)] = e.Value
		}
		for _, n := range names {
			if v, ok := set[FoldName(n, re.Identity)]; ok {
				re.Env = append(re.Env, ResolvedVar{Name: []byte(n), Value: []byte(v)})
				continue
			}
			if v, ok := look(n); ok {
				re.Env = append(re.Env, ResolvedVar{Name: []byte(n), Value: []byte(v)})
			}
		}
	}
	re.ScratchHome, re.ScratchCache, re.ScratchTemp = scratchHome, scratchCache, scratchTemp
	if err := re.ValidateFor(g, goos, ""); err != nil {
		return ResolvedExecution{}, err
	}
	return re, nil
}

// ValidateFor proves a resolved execution is exactly what the gate it claims to come from authorizes.
//
// relDir, when non-empty, is the run directory the scratch paths must be derived from. It is a
// parameter rather than something re-derived here because the caller is the party that knows which run
// this belongs to — and checking only that the paths are non-blank was the hole: a forged journal could
// redirect HOME, cache and temp outside the run entirely while still satisfying "the paths are set",
// defeating the credential isolation those directories exist to provide.
//
// It runs at attach before journaling AND again when the state-bound copy is read, because the value
// recovery acts on must be the one attach authorized rather than one that merely looks plausible.
func (re ResolvedExecution) ValidateFor(g TestGate, goos, relDir string) error {
	id := IdentityFor(goos)
	// Refused, not reinterpreted. A value frozen under one platform's name rules says nothing about
	// what the other platform would have produced.
	if re.Identity != id {
		return fmt.Errorf("%w: frozen as %q, host requires %q", ErrHostIdentityMismatch, re.Identity, id)
	}
	names, err := AuthorizedEnvNames(g, goos)
	if err != nil {
		return err
	}
	authorized := make(map[string]string, len(names))
	for _, n := range names {
		authorized[FoldName(n, id)] = n
	}
	setValues := make(map[string]string, len(g.Env.Set))
	for _, e := range g.Env.Set {
		setValues[FoldName(e.Name, id)] = e.Value
	}

	if len(re.Env) > MaxResolvedEnvNames {
		return fmt.Errorf("resolved execution carries %d environment names, limit %d", len(re.Env), MaxResolvedEnvNames)
	}
	seen := make(map[string]bool, len(re.Env))
	var prevKey string
	for i, e := range re.Env {
		name := string(e.Name)
		if err := validateEnvName(name); err != nil {
			return fmt.Errorf("resolved execution env[%d]: %w", i, err)
		}
		key := FoldName(name, id)
		if _, ok := authorized[key]; !ok {
			return fmt.Errorf("resolved execution carries %s, which the policy does not authorize", name)
		}
		if seen[key] {
			return fmt.Errorf("resolved execution carries %s twice under %s name rules", name, goos)
		}
		seen[key] = true
		// Sorted by the IDENTITY key, so the shape is canonical under the same rule that decides
		// sameness rather than under a second, disagreeing one.
		if i > 0 && key <= prevKey {
			return fmt.Errorf("resolved execution env is not sorted by name identity (%s after %s)", name, prevKey)
		}
		prevKey = key
		if idx := indexByte(e.Value, 0); idx >= 0 {
			return fmt.Errorf("resolved execution value of %s contains NUL", name)
		}
		if len(e.Value) > MaxResolvedEnvValue {
			return fmt.Errorf("resolved execution value of %s is %d bytes, limit %d", name, len(e.Value), MaxResolvedEnvValue)
		}
		// An explicitly SET value is stated BY THE POLICY, so it must be exactly that value. Requiring
		// only that the name be present let a forged or recovered intent substitute anything.
		if want, ok := setValues[key]; ok && string(e.Value) != want {
			return fmt.Errorf("resolved execution value of %s is not the value the policy sets", name)
		}
		if err := refuseCredential(name, string(e.Value)); err != nil {
			return err
		}
	}
	// The stated pairs are guarded whether or not the gate will execute, for the same reason resolution
	// guards them: they are persisted either way.
	for _, e := range g.Env.Set {
		if err := refuseCredential(e.Name, e.Value); err != nil {
			return err
		}
		if !g.Disabled && !seen[FoldName(e.Name, id)] {
			return fmt.Errorf("resolved execution omits %s, which the policy sets explicitly", e.Name)
		}
	}

	if relDir != "" {
		want := scratchPaths(relDir)
		if re.ScratchHome != want[0] || re.ScratchCache != want[1] || re.ScratchTemp != want[2] {
			return fmt.Errorf("resolved execution scratch paths are not the derived layout under %s", relDir)
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

	// The ENCODED size is what has to fit the transaction payload, so that is what is bounded.
	b, err := json.Marshal(re)
	if err != nil {
		return fmt.Errorf("resolved execution: %w", err)
	}
	if len(b) > MaxResolvedExecutionWireBytes {
		return fmt.Errorf("resolved execution encodes to %d bytes, limit %d", len(b), MaxResolvedExecutionWireBytes)
	}
	return nil
}

// refuseCredential rejects a credential-shaped assignment.
//
// Rejected, never redacted. Redaction collapses two distinct secrets to one marker, so a digest over
// redacted values would bind something the command never received — refusing is what keeps a digest
// over ACTUAL values sound. The pair is tested together because the detector's rules are
// schema-sensitive: an ordinary-looking value carries the signal only alongside its name.
func refuseCredential(name, value string) error {
	pair := name + "=" + value
	if redact.Text(pair) != pair {
		return fmt.Errorf("environment value of %s looks like a credential; name it in the policy without a secret value, or do not inherit it", name)
	}
	return nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// scratchPaths is the ONE definition of the per-run scratch layout, so bootstrap and validation cannot
// disagree about where those directories are.
func scratchPaths(relDir string) [3]string {
	return [3]string{relDir + "/scratch/home", relDir + "/scratch/cache", relDir + "/scratch/tmp"}
}

// ResolveForRun resolves the frozen environment with the run's own scratch layout.
func ResolveForRun(g TestGate, goos string, look EnvLookup, relDir string) (ResolvedExecution, error) {
	p := scratchPaths(relDir)
	re, err := ResolveExecution(g, goos, look, p[0], p[1], p[2])
	if err != nil {
		return ResolvedExecution{}, err
	}
	// Re-validated WITH the run directory, so the derived-layout rule is proven at the point the layout
	// is known rather than left to the caller.
	if err := re.ValidateFor(g, goos, relDir); err != nil {
		return ResolvedExecution{}, err
	}
	return re, nil
}

// NoAmbientEnv is an EnvLookup that finds nothing. Tests use it so a fixture's frozen environment does
// not depend on the machine the suite happens to run on.
func NoAmbientEnv(string) (string, bool) { return "", false }

// HostGOOS is the platform resolution happens on.
func HostGOOS() string { return runtime.GOOS }
