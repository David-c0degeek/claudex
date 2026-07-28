package testgate

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/David-c0degeek/claudex/internal/canonjson"
)

// Bytes and ByteList are the wire grammar for everything in the execution view.
//
// Go's encoder already writes a []byte as base64, which is the grammar the design pins - but it writes
// a NIL one as `null`, and `null` is not base64. That gave every empty value two admissible spellings
// with the same meaning and different digests, which is the one thing a digest-identified record cannot
// have. These types emit the empty form and REFUSE null, so each fact has exactly one wire shape.
type Bytes []byte

func (b Bytes) MarshalJSON() ([]byte, error) {
	if b == nil {
		return json.Marshal([]byte{})
	}
	return json.Marshal([]byte(b))
}

func (b *Bytes) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%w: a byte field is null; the empty value is \"\"", ErrLifecycle)
	}
	var v []byte
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v == nil {
		v = []byte{}
	}
	*b = v
	return nil
}

// ByteList is an ordered list of byte strings - argv elements, environment names.
type ByteList []Bytes

func (l ByteList) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]Bytes(l))
}

func (l *ByteList) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%w: a byte list is null; the empty value is []", ErrLifecycle)
	}
	var v []Bytes
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v == nil {
		v = []Bytes{}
	}
	*l = v
	return nil
}

// Strings renders the list for human-facing messages only. It is lossy by construction and must never
// be used to compare or bind anything.
func (l ByteList) Strings() []string {
	out := make([]string, len(l))
	for i, b := range l {
		out[i] = string(b)
	}
	return out
}

// ExecutionView is the ONE description of what an attempt runs.
//
// It exists because the intent and the result each held their own partial copy of this, and proving they
// agreed meant comparing six fields by hand at every boundary - a comparison that silently omitted
// whichever field was added last. With one type carrying one wire grammar, "the result describes the
// execution that was armed" is a single digest comparison, and a field added here is bound everywhere
// at once rather than in the places somebody remembered.
type ExecutionView struct {
	// Executable is the absolute path resolved at attempt start, not re-derived later.
	Executable Bytes `json:"executable_b64"`
	// Argv is the command, losslessly. Empty elements are preserved: a canonical array, never a
	// shell-ish string.
	Argv ByteList `json:"argv_b64"`
	Cwd  Bytes    `json:"cwd_b64"`
	// EnvNames is the sorted frozen variable names; EnvDigest is over the canonical ACTUAL name/value
	// list. Redacted pairs collapse distinct secrets onto one marker, so a redaction-based digest could
	// not prove two environments equal.
	EnvNames  ByteList `json:"env_names_b64"`
	EnvDigest string   `json:"env_digest"`
}

// Digest is the identity of the whole view.
//
// Comparing this is what replaces the field-by-field agreement checks: two views with the same digest
// describe the same execution, and there is no field a future change can add without changing it.
func (v ExecutionView) Digest() (string, error) {
	return canonjson.DigestValue(v)
}

// validate refuses a view that cannot describe a runnable command.
func (v ExecutionView) validate() error {
	if len(v.Executable) == 0 {
		return fmt.Errorf("%w: the execution view names no executable", ErrLifecycle)
	}
	if len(v.Argv) == 0 {
		return fmt.Errorf("%w: the execution view has no argv, so a verifier cannot see which command ran", ErrLifecycle)
	}
	if len(v.Cwd) == 0 {
		return fmt.Errorf("%w: the execution view names no working directory", ErrLifecycle)
	}
	if !isSHA256(v.EnvDigest) {
		return fmt.Errorf("%w: the environment digest %q is not a sha256", ErrLifecycle, v.EnvDigest)
	}
	return nil
}

func cloneView(v ExecutionView) ExecutionView {
	return ExecutionView{
		Executable: append(Bytes(nil), v.Executable...),
		Argv:       cloneByteList(v.Argv),
		Cwd:        append(Bytes(nil), v.Cwd...),
		EnvNames:   cloneByteList(v.EnvNames),
		EnvDigest:  v.EnvDigest,
	}
}

func cloneByteList(src ByteList) ByteList {
	if src == nil {
		return nil
	}
	out := make(ByteList, len(src))
	for i, b := range src {
		out[i] = append(Bytes(nil), b...)
	}
	return out
}

// MarshalJSONForTest and UnmarshalJSONForTest expose the wire boundary so the null-refusal rule can be
// exercised directly; nothing in production calls them.
func (v ExecutionView) MarshalJSONForTest() ([]byte, error) { return json.Marshal(v) }

func (v *ExecutionView) UnmarshalJSONForTest(raw []byte) error { return json.Unmarshal(raw, v) }
