package proctree

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/David-c0degeek/claudex/internal/canonjson"
)

// MaxSpecBytes bounds the canonical execution spec on the wire. It is a TRANSPORT ceiling and is
// deliberately separate from the policy ceilings in run-policy v2: those bound what an operator may
// authorize, this bounds what the supervisor will read before it has validated anything at all.
const MaxSpecBytes = 256 << 10

const specSchemaVersion = 1

// NameIdentity selects how environment variable names are compared.
//
// It is a parameter rather than a build tag on purpose. The rule differs by target OS, but a rule
// that only exists on the platform it applies to cannot be tested from anywhere else — and the
// Windows rule is exactly the one most likely to be got wrong.
type NameIdentity int

const (
	// NameByteExact is the Unix rule: names are distinct if their bytes differ.
	NameByteExact NameIdentity = iota + 1
	// NameASCIIFold is the Windows rule: names are compared after ASCII upper-casing, because Path and
	// PATH are one variable to the OS but would be two entries in a sorted list. Names are restricted
	// to ASCII (see validateEnvName) precisely so this fold is exact in both validation and
	// child-environment construction; an abstract "case-insensitive" comparison would be
	// locale- and API-dependent and the two could disagree.
	NameASCIIFold
)

func (n NameIdentity) String() string {
	switch n {
	case NameByteExact:
		return "byte_exact"
	case NameASCIIFold:
		return "ascii_fold"
	}
	return "unknown"
}

func parseNameIdentity(s string) (NameIdentity, error) {
	switch s {
	case "byte_exact":
		return NameByteExact, nil
	case "ascii_fold":
		return NameASCIIFold, nil
	}
	return 0, fmt.Errorf("proctree: unknown name identity %q", s)
}

// EnvVar is one environment entry. Name and Value are raw bytes: an inherited Unix value may contain
// invalid UTF-8, and canonical JSON would replace those bytes, so the digest would bind something the
// child never receives. This is the defect found for git paths in the evidence packet and fixed there
// the same way.
type EnvVar struct {
	Name  []byte
	Value []byte
}

// ExecSpec is the complete description of the process to run. It crosses the re-exec boundary on its
// own inherited pipe rather than through argv or the supervisor's own environment: those would expose
// it through /proc/<pid>/cmdline and /proc/<pid>/environ, and would make "the child's environment" and
// "the supervisor's environment" the same object, at which point exactness could not even be stated.
type ExecSpec struct {
	// Executable is the absolute, cleaned path selected by resolution at attempt start.
	Executable []byte
	// Argv is the full argument vector including argv[0]. Empty elements are meaningful and preserved.
	Argv [][]byte
	// Cwd is the run worktree.
	Cwd []byte
	// Env is the frozen environment. Order is canonicalised on encode.
	Env []EnvVar
	// Identity is how Env names are compared and ordered.
	Identity NameIdentity
}

type wireEnv struct {
	NameB64  string `json:"name_b64"`
	ValueB64 string `json:"value_b64"`
}

type wireSpec struct {
	SchemaVersion int       `json:"schema_version"`
	NameIdentity  string    `json:"name_identity"`
	ExecutableB64 string    `json:"executable_b64"`
	CwdB64        string    `json:"cwd_b64"`
	ArgvB64       []string  `json:"argv_b64"`
	Env           []wireEnv `json:"env"`
}

var (
	// ErrSpecInvalid is the class for a spec that cannot be persisted or executed.
	ErrSpecInvalid = errors.New("proctree: invalid execution spec")
	// ErrEnvNameCollision means two allowlisted names collapse to one under the platform's identity
	// rule, so the digest would bind two entries where the OS supplies one.
	ErrEnvNameCollision = errors.New("proctree: environment name collision under platform identity")
)

// foldName applies the platform's name identity, producing the comparison key.
func foldName(name []byte, id NameIdentity) []byte {
	if id != NameASCIIFold {
		return name
	}
	out := make([]byte, len(name))
	for i, b := range name {
		if b >= 'a' && b <= 'z' {
			b -= 'a' - 'A'
		}
		out[i] = b
	}
	return out
}

// validateEnvName enforces the portable ASCII grammar.
//
// Base64 preserves bytes; it does not make them a valid environment. A decoded pair still has to form
// an unambiguous name=value and still has to survive execve, so an empty name, a name containing '='
// or NUL, and a NUL in a value are all refused before anything is persisted or executed. The ASCII
// restriction goes further, and its purpose is the Windows fold: with names limited to ASCII, the
// fold is a plain A-Z mapping that validation and child-environment construction cannot implement
// differently.
func validateEnvName(name []byte) error {
	if len(name) == 0 {
		return fmt.Errorf("%w: empty environment variable name", ErrSpecInvalid)
	}
	for i, b := range name {
		switch {
		case b == 0:
			return fmt.Errorf("%w: NUL in environment variable name", ErrSpecInvalid)
		case b == '=':
			return fmt.Errorf("%w: '=' in environment variable name %q", ErrSpecInvalid, name)
		case b == '_':
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z':
		case b >= '0' && b <= '9':
			if i == 0 {
				return fmt.Errorf("%w: environment variable name %q starts with a digit", ErrSpecInvalid, name)
			}
		default:
			return fmt.Errorf("%w: environment variable name %q is not [A-Za-z_][A-Za-z0-9_]*", ErrSpecInvalid, name)
		}
	}
	return nil
}

func validateEnvValue(name, value []byte) error {
	if bytes.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: NUL in value of %q", ErrSpecInvalid, name)
	}
	return nil
}

// Validate checks everything that must hold before the spec is persisted, digested or executed.
func (s ExecSpec) Validate() error {
	switch s.Identity {
	case NameByteExact, NameASCIIFold:
	default:
		return fmt.Errorf("%w: name identity not set", ErrSpecInvalid)
	}
	if len(s.Executable) == 0 {
		return fmt.Errorf("%w: empty executable", ErrSpecInvalid)
	}
	if bytes.IndexByte(s.Executable, 0) >= 0 {
		return fmt.Errorf("%w: NUL in executable path", ErrSpecInvalid)
	}
	if len(s.Cwd) == 0 {
		return fmt.Errorf("%w: empty cwd", ErrSpecInvalid)
	}
	if bytes.IndexByte(s.Cwd, 0) >= 0 {
		return fmt.Errorf("%w: NUL in cwd", ErrSpecInvalid)
	}
	if len(s.Argv) == 0 {
		return fmt.Errorf("%w: empty argv", ErrSpecInvalid)
	}
	if len(s.Argv[0]) == 0 {
		return fmt.Errorf("%w: blank argv[0]", ErrSpecInvalid)
	}
	for i, a := range s.Argv {
		// Later arguments may be empty on purpose; none may contain NUL, which execve cannot carry.
		if bytes.IndexByte(a, 0) >= 0 {
			return fmt.Errorf("%w: NUL in argv[%d]", ErrSpecInvalid, i)
		}
	}
	seen := make(map[string]string, len(s.Env))
	for _, e := range s.Env {
		if err := validateEnvName(e.Name); err != nil {
			return err
		}
		if err := validateEnvValue(e.Name, e.Value); err != nil {
			return err
		}
		key := string(foldName(e.Name, s.Identity))
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("%w: %q and %q", ErrEnvNameCollision, prev, e.Name)
		}
		seen[key] = string(e.Name)
	}
	return nil
}

// canonicalEnv returns the environment sorted by its comparison key, with ties broken by the raw
// name. Ties can only occur when the keys differ but sort equal, which the collision check has
// already excluded; the secondary key exists so the ordering is total by construction rather than by
// argument.
func (s ExecSpec) canonicalEnv() []EnvVar {
	out := append([]EnvVar(nil), s.Env...)
	sort.SliceStable(out, func(i, j int) bool {
		ki := foldName(out[i].Name, s.Identity)
		kj := foldName(out[j].Name, s.Identity)
		if c := bytes.Compare(ki, kj); c != 0 {
			return c < 0
		}
		return bytes.Compare(out[i].Name, out[j].Name) < 0
	})
	return out
}

// Encode renders the canonical spec bytes.
func (s ExecSpec) Encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	w := wireSpec{
		SchemaVersion: specSchemaVersion,
		NameIdentity:  s.Identity.String(),
		ExecutableB64: base64.StdEncoding.EncodeToString(s.Executable),
		CwdB64:        base64.StdEncoding.EncodeToString(s.Cwd),
		ArgvB64:       make([]string, len(s.Argv)),
		Env:           make([]wireEnv, 0, len(s.Env)),
	}
	for i, a := range s.Argv {
		w.ArgvB64[i] = base64.StdEncoding.EncodeToString(a)
	}
	for _, e := range s.canonicalEnv() {
		w.Env = append(w.Env, wireEnv{
			NameB64:  base64.StdEncoding.EncodeToString(e.Name),
			ValueB64: base64.StdEncoding.EncodeToString(e.Value),
		})
	}
	b, err := canonjson.CanonicalizeValue(w)
	if err != nil {
		return nil, fmt.Errorf("proctree: encode spec: %w", err)
	}
	if len(b) > MaxSpecBytes {
		return nil, fmt.Errorf("%w: canonical spec %d bytes exceeds %d", ErrSpecInvalid, len(b), MaxSpecBytes)
	}
	return b, nil
}

// Digest is the value bound in the attempt intent, which the supervisor compares against before it
// will act on a spec.
func (s ExecSpec) Digest() (string, error) {
	b, err := s.Encode()
	if err != nil {
		return "", err
	}
	return canonjson.Digest(b)
}

// DecodeSpec parses canonical spec bytes and re-validates them. Validation runs on the way IN as well
// as out: a spec is only trustworthy if the reader proves it, not because the writer promised.
func DecodeSpec(raw []byte) (ExecSpec, error) {
	if len(raw) > MaxSpecBytes {
		return ExecSpec{}, fmt.Errorf("%w: spec %d bytes exceeds %d", ErrSpecInvalid, len(raw), MaxSpecBytes)
	}
	var w wireSpec
	if err := decodeExactlyOne(raw, &w); err != nil {
		return ExecSpec{}, fmt.Errorf("proctree: decode spec: %w", err)
	}
	if w.SchemaVersion != specSchemaVersion {
		return ExecSpec{}, fmt.Errorf("%w: schema version %d, want %d", ErrSpecInvalid, w.SchemaVersion, specSchemaVersion)
	}
	id, err := parseNameIdentity(w.NameIdentity)
	if err != nil {
		return ExecSpec{}, err
	}
	s := ExecSpec{Identity: id}
	if s.Executable, err = base64.StdEncoding.DecodeString(w.ExecutableB64); err != nil {
		return ExecSpec{}, fmt.Errorf("%w: executable: %v", ErrSpecInvalid, err)
	}
	if s.Cwd, err = base64.StdEncoding.DecodeString(w.CwdB64); err != nil {
		return ExecSpec{}, fmt.Errorf("%w: cwd: %v", ErrSpecInvalid, err)
	}
	s.Argv = make([][]byte, len(w.ArgvB64))
	for i, a := range w.ArgvB64 {
		if s.Argv[i], err = base64.StdEncoding.DecodeString(a); err != nil {
			return ExecSpec{}, fmt.Errorf("%w: argv[%d]: %v", ErrSpecInvalid, i, err)
		}
	}
	s.Env = make([]EnvVar, len(w.Env))
	for i, e := range w.Env {
		if s.Env[i].Name, err = base64.StdEncoding.DecodeString(e.NameB64); err != nil {
			return ExecSpec{}, fmt.Errorf("%w: env[%d] name: %v", ErrSpecInvalid, i, err)
		}
		if s.Env[i].Value, err = base64.StdEncoding.DecodeString(e.ValueB64); err != nil {
			return ExecSpec{}, fmt.Errorf("%w: env[%d] value: %v", ErrSpecInvalid, i, err)
		}
	}
	if err := s.Validate(); err != nil {
		return ExecSpec{}, err
	}
	// The grammar is CANONICAL, so canonicality is enforced on input rather than merely produced on
	// output. Decoding normalises one way: duplicate keys collapse, member order is discarded, and two
	// different byte strings can decode to the same value. Accepting those would make the digest bound
	// in the attempt intent an authority over only one of the byte strings this reader admits, which
	// is the opposite of what a content digest is for.
	canonical, err := s.Encode()
	if err != nil {
		return ExecSpec{}, err
	}
	if !bytes.Equal(canonical, raw) {
		return ExecSpec{}, fmt.Errorf("%w: bytes are not the canonical encoding of their own content", ErrSpecInvalid)
	}
	return s, nil
}

// decodeExactlyOne decodes one JSON value and PROVES nothing follows it.
//
// json.Decoder.Decode stops after a single value and leaves the rest of the stream unread, so a
// payload of "<valid> {}" decodes without complaint. For a frame payload that is a malformed frame
// accepted as a good one; for the spec it breaks the canonical-bytes claim outright.
func decodeExactlyOne(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing bytes after the value")
	}
	return nil
}

// Environ renders the child environment as os/exec expects it.
//
// Go strings are byte sequences and are not required to be valid UTF-8, so this conversion is
// byte-exact: an inherited value containing invalid UTF-8 reaches the child unchanged. That is the
// whole reason the spec carries bytes rather than JSON strings.
func (s ExecSpec) Environ() []string {
	env := s.canonicalEnv()
	out := make([]string, 0, len(env))
	for _, e := range env {
		out = append(out, string(e.Name)+"="+string(e.Value))
	}
	return out
}

// ArgvStrings renders argv as os/exec expects it, preserving empty elements.
func (s ExecSpec) ArgvStrings() []string {
	out := make([]string, len(s.Argv))
	for i, a := range s.Argv {
		out[i] = string(a)
	}
	return out
}
