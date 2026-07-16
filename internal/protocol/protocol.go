// Package protocol embeds the versioned message schemas and validates messages
// against the exact embedded bytes, so the contract given to a provider and the
// contract the coordinator enforces are one source of truth.
//
// Every message carries an integer major protocol_version and a message_type.
// Validate runs a bounded header preflight first, so a readable but unsupported
// major is rejected with upgrade remediation before this version's canonical
// rules apply. A supported message is then canonicalized (internal/canonjson) —
// inheriting duplicate-key, integer-range, Unicode, size, depth, and trailing-
// content rules — its header re-checked authoritatively, and its body
// structurally validated against the compiled schema selected by the caller's
// expected type. Strict typed Go decoding and semantic/cross-field checks remain
// a separate downstream layer.
package protocol

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"sync"

	"github.com/David-c0degeek/claudex/internal/canonjson"
)

// SupportedVersion is the only protocol major this coordinator speaks.
const SupportedVersion = 1

//go:embed schemas/*.json
var schemaFS embed.FS

// ProtocolVersionError is returned when a message's protocol_version is missing
// or is not the supported major. It carries remediation, not submitted payload.
type ProtocolVersionError struct {
	Got interface{} // the offending version value, or nil if absent
}

func (e *ProtocolVersionError) Error() string {
	got := "missing"
	if e.Got != nil {
		got = fmt.Sprintf("%v", e.Got)
	}
	return fmt.Sprintf("unsupported protocol_version (%s); this coordinator speaks protocol_version %d — upgrade the claudex client to a matching version", got, SupportedVersion)
}

// entry binds an embedded schema's exact bytes to its compiled form and identity.
type entry struct {
	raw      []byte
	compiled *schemaNode
	typ      string
	major    int
}

type schemaKey struct {
	typ   string
	major int
}

var (
	compiledOnce sync.Once
	registry     map[schemaKey]*entry
	compileErr   error
)

func schemas() (map[schemaKey]*entry, error) {
	compiledOnce.Do(func() { registry, compileErr = loadAll() })
	return registry, compileErr
}

func loadAll() (map[schemaKey]*entry, error) {
	entries, err := fs.ReadDir(schemaFS, "schemas")
	if err != nil {
		return nil, err
	}
	out := make(map[schemaKey]*entry)
	for _, e := range entries {
		typ, major, ok := parseSchemaName(e.Name())
		if !ok {
			return nil, fmt.Errorf("protocol: schema filename %q is not <type>.v<major>.json", e.Name())
		}
		raw, err := schemaFS.ReadFile("schemas/" + e.Name())
		if err != nil {
			return nil, err
		}
		node, err := compile(raw)
		if err != nil {
			return nil, fmt.Errorf("protocol: compile %s: %w", e.Name(), err)
		}
		if err := checkIdentity(node, typ, major); err != nil {
			return nil, fmt.Errorf("protocol: %s: %w", e.Name(), err)
		}
		key := schemaKey{typ, major}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("protocol: duplicate schema for type %q at version %d", typ, major)
		}
		out[key] = &entry{raw: raw, compiled: node, typ: typ, major: major}
	}
	return out, nil
}

// checkIdentity binds a schema's registry identity to its contents: the root
// must be a strict object whose message_type is a string const equal to the
// filename type and whose protocol_version is an integer const equal to the
// filename major. A mislabeled or inert schema fails startup, never registers
// silently.
func checkIdentity(n *schemaNode, typ string, major int) error {
	if !typesExactly(n, "object") {
		return fmt.Errorf("schema root must be exactly an object")
	}
	mt := n.properties["message_type"]
	if mt == nil || !mt.hasConst || !typesExactly(mt, "string") || !valueEquals(mt.constVal, typ) {
		return fmt.Errorf("message_type must be a string const equal to %q", typ)
	}
	pv := n.properties["protocol_version"]
	if pv == nil || !pv.hasConst || !typesExactly(pv, "integer") || !valueEquals(pv.constVal, major) {
		return fmt.Errorf("protocol_version must be an integer const equal to %d", major)
	}
	return nil
}

// typesExactly reports whether the node declares exactly the one type t (not a
// union that merely contains it).
func typesExactly(n *schemaNode, t string) bool {
	return len(n.types) == 1 && n.types[0] == t
}

func parseSchemaName(name string) (typ string, major int, ok bool) {
	base, found := strings.CutSuffix(name, ".json")
	if !found {
		return "", 0, false
	}
	i := strings.LastIndex(base, ".v")
	if i <= 0 || i+2 >= len(base) {
		return "", 0, false
	}
	m, err := strconv.Atoi(base[i+2:])
	if err != nil || m < 1 {
		return "", 0, false
	}
	return base[:i], m, true
}

// Schema returns a copy of the exact embedded schema bytes for a message type
// and major, for handing to a provider as instruction. The copy means a caller
// can never mutate the registry's bytes.
func Schema(messageType string, major int) ([]byte, error) {
	set, err := schemas()
	if err != nil {
		return nil, err
	}
	e, ok := set[schemaKey{messageType, major}]
	if !ok {
		return nil, fmt.Errorf("protocol: no schema for message type %q version %d", messageType, major)
	}
	return append([]byte(nil), e.raw...), nil
}

// Validate rejects an unsupported major (via a bounded header preflight) with
// remediation, canonicalizes the message, re-checks the header authoritatively,
// confirms message_type matches expectedType, and structurally validates against
// the embedded schema for expectedType at SupportedVersion. On success it
// returns the canonical bytes (the caller redacts before any durable digest).
// Diagnostics report structural paths and never echo submitted values.
func Validate(expectedType string, raw []byte) ([]byte, error) {
	// Preflight: give a readable but unsupported major upgrade remediation before
	// this version's canonical rules could reject it for a different reason.
	if v, ok := preflightVersion(raw); ok && v != SupportedVersion {
		return nil, &ProtocolVersionError{Got: v}
	}

	canon, err := canonjson.Canonicalize(raw)
	if err != nil {
		return nil, fmt.Errorf("protocol: %w", err)
	}
	if err := negotiate(canon, expectedType); err != nil {
		return nil, err
	}
	set, err := schemas()
	if err != nil {
		return nil, err
	}
	e, ok := set[schemaKey{expectedType, SupportedVersion}]
	if !ok {
		return nil, fmt.Errorf("protocol: no schema registered for message type %q", expectedType)
	}
	var v interface{}
	dec := json.NewDecoder(bytes.NewReader(canon))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("protocol: parse: %w", err)
	}
	if err := e.compiled.validate(v, "$"); err != nil {
		return nil, fmt.Errorf("protocol: %w", err)
	}
	return canon, nil
}

// preflightVersion reads a single top-level integer protocol_version from raw
// without applying this version's canonical rules, so an old/future client gets
// upgrade remediation whenever its header is readable. It is bounded to the
// canonical size limit and reports ok=false if the top level is not an object,
// the key is absent, its value is not an integer, or it appears more than once
// (ambiguous — the canonical pass will reject the duplicate).
func preflightVersion(raw []byte) (version int, ok bool) {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(raw), maxPreflightBytes))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return 0, false
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '{' {
		return 0, false
	}
	found := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, false
		}
		key, _ := keyTok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return 0, false
		}
		if key != "protocol_version" {
			continue
		}
		var num json.Number
		if json.Unmarshal(val, &num) != nil || strings.ContainsAny(num.String(), ".eE") {
			return 0, false // present but not an integer: let the canonical pass speak
		}
		iv, err := num.Int64()
		if err != nil {
			return 0, false
		}
		if found {
			return 0, false // duplicate protocol_version: ambiguous
		}
		found = true
		version = int(iv)
	}
	return version, found
}

const maxPreflightBytes = 1 << 20

// negotiate re-checks the version major and message_type on the canonical bytes
// (the authoritative gate). A version mismatch is a ProtocolVersionError; a type
// mismatch names only the expected type, never the submitted one.
func negotiate(canon []byte, expectedType string) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(canon, &top); err != nil {
		return fmt.Errorf("protocol: message must be a JSON object")
	}
	pvRaw, ok := top["protocol_version"]
	if !ok {
		return &ProtocolVersionError{Got: nil}
	}
	var pv int
	if err := json.Unmarshal(pvRaw, &pv); err != nil {
		return &ProtocolVersionError{Got: "non-integer"} // fixed marker, not the submitted value
	}
	if pv != SupportedVersion {
		return &ProtocolVersionError{Got: pv}
	}
	mtRaw, ok := top["message_type"]
	if !ok {
		return fmt.Errorf("protocol: message is missing message_type")
	}
	var mt string
	if err := json.Unmarshal(mtRaw, &mt); err != nil {
		return fmt.Errorf("protocol: message_type must be a string")
	}
	if mt != expectedType {
		return fmt.Errorf("protocol: message_type does not match the expected %q", expectedType)
	}
	return nil
}
