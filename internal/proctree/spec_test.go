package proctree

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/canonjson"
)

// invalidUTF8 is the case canonical JSON cannot carry: encoding/json replaces each invalid byte with
// U+FFFD, so "x\x80" and "x\x81" would collapse to the same string and the digest would bind
// something the child never receives.
var invalidUTF8 = []byte{'x', 0x80, 0xFE, 'y'}

func baseSpec() ExecSpec {
	return ExecSpec{
		Executable: []byte("/usr/bin/env"),
		Argv:       [][]byte{[]byte("env"), []byte(""), []byte("arg")},
		Cwd:        []byte("/work"),
		Env: []EnvVar{
			{Name: []byte("PATH"), Value: []byte("/usr/bin")},
			{Name: []byte("ODD"), Value: invalidUTF8},
		},
		Identity: NameByteExact,
	}
}

// TestSpecRoundTripIsByteExact is the obligation the base64 grammar exists for. It also covers the
// intentionally empty argv element, which is meaningful and must survive.
func TestSpecRoundTripIsByteExact(t *testing.T) {
	in := baseSpec()
	raw, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := DecodeSpec(raw)
	if err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	if !bytes.Equal(out.Executable, in.Executable) || !bytes.Equal(out.Cwd, in.Cwd) {
		t.Fatalf("paths not preserved: %q %q", out.Executable, out.Cwd)
	}
	if len(out.Argv) != len(in.Argv) {
		t.Fatalf("argv length = %d, want %d", len(out.Argv), len(in.Argv))
	}
	for i := range in.Argv {
		if !bytes.Equal(out.Argv[i], in.Argv[i]) {
			t.Fatalf("argv[%d] = %q, want %q", i, out.Argv[i], in.Argv[i])
		}
	}
	var found bool
	for _, e := range out.Env {
		if string(e.Name) == "ODD" {
			found = true
			if !bytes.Equal(e.Value, invalidUTF8) {
				t.Fatalf("invalid-UTF-8 value = %x, want %x", e.Value, invalidUTF8)
			}
		}
	}
	if !found {
		t.Fatal("ODD not present after round trip")
	}

	// Environ is the form os/exec consumes, so byte-exactness has to survive that conversion too:
	// Go strings are byte sequences, which is what makes this possible at all.
	var got string
	for _, kv := range out.Environ() {
		if strings.HasPrefix(kv, "ODD=") {
			got = strings.TrimPrefix(kv, "ODD=")
		}
	}
	if got != string(invalidUTF8) {
		t.Fatalf("Environ value = %x, want %x", got, invalidUTF8)
	}

	argv := out.ArgvStrings()
	if len(argv) != 3 || argv[1] != "" {
		t.Fatalf("ArgvStrings dropped the empty element: %q", argv)
	}
}

func TestEnvNameGrammar(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
	}{
		{"empty", ""},
		{"equals", "A=B"},
		{"nul", "A\x00B"},
		{"leading digit", "1PATH"},
		{"non-ascii", "PÄTH"},
		{"dash", "MY-VAR"},
		{"space", "MY VAR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSpec()
			s.Env = []EnvVar{{Name: []byte(tc.env), Value: []byte("v")}}
			if err := s.Validate(); !errors.Is(err, ErrSpecInvalid) {
				t.Fatalf("err = %v, want ErrSpecInvalid", err)
			}
		})
	}
	// The grammar admits what it should.
	for _, ok := range []string{"PATH", "_X", "A1", "SystemRoot", "PATHEXT"} {
		s := baseSpec()
		s.Env = []EnvVar{{Name: []byte(ok), Value: []byte("v")}}
		if err := s.Validate(); err != nil {
			t.Fatalf("%q rejected: %v", ok, err)
		}
	}
}

func TestNULInValueRefused(t *testing.T) {
	s := baseSpec()
	s.Env = []EnvVar{{Name: []byte("PATH"), Value: []byte("a\x00b")}}
	if err := s.Validate(); !errors.Is(err, ErrSpecInvalid) {
		t.Fatalf("err = %v, want ErrSpecInvalid", err)
	}
}

// TestWindowsFoldCollision is the case where a digest would bind two entries while the OS supplies
// one. The same pair is legitimate under the Unix rule, which is why identity is a parameter.
func TestWindowsFoldCollision(t *testing.T) {
	pair := []EnvVar{
		{Name: []byte("Path"), Value: []byte("a")},
		{Name: []byte("PATH"), Value: []byte("b")},
	}

	win := baseSpec()
	win.Identity = NameASCIIFold
	win.Env = pair
	if err := win.Validate(); !errors.Is(err, ErrEnvNameCollision) {
		t.Fatalf("windows: err = %v, want ErrEnvNameCollision", err)
	}

	unix := baseSpec()
	unix.Identity = NameByteExact
	unix.Env = pair
	if err := unix.Validate(); err != nil {
		t.Fatalf("unix: names differing in case are distinct: %v", err)
	}
}

// TestCanonicalOrderingIsInputIndependent proves the digest binds the environment, not the order the
// caller happened to build it in.
func TestCanonicalOrderingIsInputIndependent(t *testing.T) {
	a := baseSpec()
	a.Env = []EnvVar{
		{Name: []byte("ZED"), Value: []byte("1")},
		{Name: []byte("ALPHA"), Value: []byte("2")},
		{Name: []byte("MID"), Value: []byte("3")},
	}
	b := baseSpec()
	b.Env = []EnvVar{
		{Name: []byte("MID"), Value: []byte("3")},
		{Name: []byte("ZED"), Value: []byte("1")},
		{Name: []byte("ALPHA"), Value: []byte("2")},
	}
	da, err := a.Digest()
	if err != nil {
		t.Fatalf("Digest a: %v", err)
	}
	db, err := b.Digest()
	if err != nil {
		t.Fatalf("Digest b: %v", err)
	}
	if da != db {
		t.Fatalf("digest depends on input order: %s vs %s", da, db)
	}

	// A different VALUE must change the digest, or the binding proves nothing.
	c := baseSpec()
	c.Env = append([]EnvVar(nil), a.Env...)
	c.Env[0].Value = []byte("changed")
	dc, err := c.Digest()
	if err != nil {
		t.Fatalf("Digest c: %v", err)
	}
	if dc == da {
		t.Fatal("digest is insensitive to an environment value")
	}
}

func TestIdentityIsPartOfTheDigest(t *testing.T) {
	a := baseSpec()
	a.Identity = NameByteExact
	b := baseSpec()
	b.Identity = NameASCIIFold
	da, err := a.Digest()
	if err != nil {
		t.Fatalf("Digest a: %v", err)
	}
	db, err := b.Digest()
	if err != nil {
		t.Fatalf("Digest b: %v", err)
	}
	if da == db {
		t.Fatal("the comparison rule must be part of what the digest binds")
	}
}

// TestDecodeRevalidates is the guard that matters most: a spec is trustworthy because the reader
// proves it, not because the writer promised. Crafting the wire form directly bypasses Encode's
// validation exactly as a hostile or buggy peer would.
func TestDecodeRevalidates(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    wireSpec
	}{
		{"bad env name", wireSpec{
			SchemaVersion: specSchemaVersion,
			NameIdentity:  "byte_exact",
			ExecutableB64: base64.StdEncoding.EncodeToString([]byte("/bin/true")),
			CwdB64:        base64.StdEncoding.EncodeToString([]byte("/w")),
			ArgvB64:       []string{base64.StdEncoding.EncodeToString([]byte("true"))},
			Env:           []wireEnv{{NameB64: base64.StdEncoding.EncodeToString([]byte("BAD=NAME")), ValueB64: ""}},
		}},
		{"nul in value", wireSpec{
			SchemaVersion: specSchemaVersion,
			NameIdentity:  "byte_exact",
			ExecutableB64: base64.StdEncoding.EncodeToString([]byte("/bin/true")),
			CwdB64:        base64.StdEncoding.EncodeToString([]byte("/w")),
			ArgvB64:       []string{base64.StdEncoding.EncodeToString([]byte("true"))},
			Env:           []wireEnv{{NameB64: base64.StdEncoding.EncodeToString([]byte("OK")), ValueB64: base64.StdEncoding.EncodeToString([]byte("a\x00b"))}},
		}},
		{"empty argv", wireSpec{
			SchemaVersion: specSchemaVersion,
			NameIdentity:  "byte_exact",
			ExecutableB64: base64.StdEncoding.EncodeToString([]byte("/bin/true")),
			CwdB64:        base64.StdEncoding.EncodeToString([]byte("/w")),
			ArgvB64:       []string{},
		}},
		{"fold collision", wireSpec{
			SchemaVersion: specSchemaVersion,
			NameIdentity:  "ascii_fold",
			ExecutableB64: base64.StdEncoding.EncodeToString([]byte("/bin/true")),
			CwdB64:        base64.StdEncoding.EncodeToString([]byte("/w")),
			ArgvB64:       []string{base64.StdEncoding.EncodeToString([]byte("true"))},
			Env: []wireEnv{
				{NameB64: base64.StdEncoding.EncodeToString([]byte("Path")), ValueB64: ""},
				{NameB64: base64.StdEncoding.EncodeToString([]byte("PATH")), ValueB64: ""},
			},
		}},
		{"unknown identity", wireSpec{
			SchemaVersion: specSchemaVersion,
			NameIdentity:  "whatever",
			ExecutableB64: base64.StdEncoding.EncodeToString([]byte("/bin/true")),
			CwdB64:        base64.StdEncoding.EncodeToString([]byte("/w")),
			ArgvB64:       []string{base64.StdEncoding.EncodeToString([]byte("true"))},
		}},
		{"wrong schema version", wireSpec{
			SchemaVersion: specSchemaVersion + 1,
			NameIdentity:  "byte_exact",
			ExecutableB64: base64.StdEncoding.EncodeToString([]byte("/bin/true")),
			CwdB64:        base64.StdEncoding.EncodeToString([]byte("/w")),
			ArgvB64:       []string{base64.StdEncoding.EncodeToString([]byte("true"))},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := canonjson.CanonicalizeValue(tc.w)
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if _, err := DecodeSpec(raw); err == nil {
				t.Fatal("DecodeSpec accepted a spec its own encoder would refuse")
			}
		})
	}
}

func TestSpecStructuralRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutet func(*ExecSpec)
	}{
		{"empty executable", func(s *ExecSpec) { s.Executable = nil }},
		{"nul in executable", func(s *ExecSpec) { s.Executable = []byte("/bin/\x00true") }},
		{"empty cwd", func(s *ExecSpec) { s.Cwd = nil }},
		{"nul in cwd", func(s *ExecSpec) { s.Cwd = []byte("/w\x00") }},
		{"empty argv", func(s *ExecSpec) { s.Argv = nil }},
		{"blank argv0", func(s *ExecSpec) { s.Argv = [][]byte{[]byte(""), []byte("x")} }},
		{"nul in argv", func(s *ExecSpec) { s.Argv = [][]byte{[]byte("cmd"), []byte("a\x00b")} }},
		{"identity unset", func(s *ExecSpec) { s.Identity = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSpec()
			tc.mutet(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("Validate accepted a structurally invalid spec")
			}
			if _, err := s.Encode(); err == nil {
				t.Fatal("Encode accepted a structurally invalid spec")
			}
		})
	}
}

func TestSpecCeiling(t *testing.T) {
	s := baseSpec()
	s.Env = append(s.Env, EnvVar{Name: []byte("BIG"), Value: bytes.Repeat([]byte("v"), MaxSpecBytes)})
	if _, err := s.Encode(); !errors.Is(err, ErrSpecInvalid) {
		t.Fatalf("err = %v, want ErrSpecInvalid", err)
	}
	if _, err := DecodeSpec(make([]byte, MaxSpecBytes+1)); !errors.Is(err, ErrSpecInvalid) {
		t.Fatalf("decode err = %v, want ErrSpecInvalid", err)
	}
}

// TestSpecRefusesTrailingAndNonCanonicalBytes. The spec claims a CANONICAL grammar and its digest is
// bound in the attempt intent, so a reader that accepts non-canonical spellings makes that digest an
// authority over only one of the byte strings it admits.
func TestSpecRefusesTrailingAndNonCanonicalBytes(t *testing.T) {
	raw, err := baseSpec().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := DecodeSpec(raw); err != nil {
		t.Fatalf("the canonical encoding must decode: %v", err)
	}

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"trailing value", append(append([]byte(nil), raw...), []byte(" {}")...)},
		{"trailing garbage", append(append([]byte(nil), raw...), 'x')},
		{"leading whitespace", append([]byte(" "), raw...)},
		{"reordered members", reorderFirstTwoMembers(raw)},
		{"duplicate member", duplicateFirstMember(raw)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.in == nil {
				t.Skip("variant not constructible for this encoding")
			}
			if _, err := DecodeSpec(tc.in); err == nil {
				t.Fatal("DecodeSpec accepted non-canonical bytes")
			}
		})
	}
}

// reorderFirstTwoMembers swaps two top-level members, producing bytes that decode to the same value
// but are not the canonical encoding of it.
func reorderFirstTwoMembers(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) < 2 {
		return nil
	}
	keys[0], keys[1] = keys[1], keys[0]
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(m[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// duplicateFirstMember repeats a member. Go's decoder keeps the last occurrence, so the value still
// parses — which is exactly why the canonical re-encoding check has to exist.
func duplicateFirstMember(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil
	}
	first := keys[0]
	kb, err := json.Marshal(first)
	if err != nil {
		return nil
	}
	dup := append([]byte(nil), kb...)
	dup = append(dup, ':')
	dup = append(dup, m[first]...)
	dup = append(dup, ',')
	out := append([]byte("{"), dup...)
	return append(out, raw[1:]...)
}
