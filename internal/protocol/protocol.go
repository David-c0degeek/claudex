// Package protocol embeds the versioned message schemas and validates messages
// against the exact embedded bytes, so the contract given to a provider and the
// contract the coordinator enforces are one source of truth.
//
// Every message carries an integer major protocol_version and a message_type.
// A message is first canonicalized (internal/canonjson) — which enforces
// duplicate-key, integer-range, Unicode, size, depth, and trailing-content
// rules — then version-negotiated (an unsupported major is rejected with
// remediation before any field evaluation), then structurally validated against
// the compiled schema selected by the caller's expected type. Strict typed Go
// decoding and semantic/cross-field checks remain a separate downstream layer.
package protocol

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
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

var (
	compiledOnce sync.Once
	compiled     map[string]*schemaNode
	compileErr   error
)

func schemas() (map[string]*schemaNode, error) {
	compiledOnce.Do(func() { compiled, compileErr = loadAll() })
	return compiled, compileErr
}

func loadAll() (map[string]*schemaNode, error) {
	entries, err := fs.ReadDir(schemaFS, "schemas")
	if err != nil {
		return nil, err
	}
	out := make(map[string]*schemaNode)
	for _, e := range entries {
		typ, major, ok := parseSchemaName(e.Name())
		if !ok {
			return nil, fmt.Errorf("protocol: schema filename %q is not <type>.v<major>.json", e.Name())
		}
		if major != SupportedVersion {
			continue
		}
		raw, err := schemaFS.ReadFile("schemas/" + e.Name())
		if err != nil {
			return nil, err
		}
		node, err := compile(raw)
		if err != nil {
			return nil, fmt.Errorf("protocol: compile %s: %w", e.Name(), err)
		}
		if _, dup := out[typ]; dup {
			return nil, fmt.Errorf("protocol: duplicate schema for type %q at version %d", typ, major)
		}
		out[typ] = node
	}
	return out, nil
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

// Validate canonicalizes raw, rejects an unsupported protocol major, confirms
// the message_type matches expectedType, and structurally validates against the
// embedded schema for expectedType. On success it returns the canonical bytes
// (the caller redacts before taking any durable digest). Diagnostics report
// structural paths and never echo submitted values.
func Validate(expectedType string, raw []byte) ([]byte, error) {
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
	sch, ok := set[expectedType]
	if !ok {
		return nil, fmt.Errorf("protocol: no schema registered for message type %q", expectedType)
	}
	var v interface{}
	dec := json.NewDecoder(bytes.NewReader(canon))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("protocol: parse: %w", err)
	}
	if err := sch.validate(v, "$"); err != nil {
		return nil, fmt.Errorf("protocol: %w", err)
	}
	return canon, nil
}

// negotiate checks the version major and the message_type header before any
// schema field evaluation. A version mismatch is a ProtocolVersionError; a
// type mismatch names only the expected type, never the submitted one.
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
