package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/David-c0degeek/claudex/internal/canonjson"
)

// schemaNode is a compiled schema. Only the keyword subset below is supported;
// compilation rejects any other keyword so a new constraint can never be
// silently ignored at validation time.
type schemaNode struct {
	types           []string // allowed JSON types; empty = any
	properties      map[string]*schemaNode
	required        []string
	additionalProps bool // meaningful only when properties is set
	hasProperties   bool
	hasAdditional   bool
	items           *schemaNode
	enum            []interface{}
	hasEnum         bool
	constVal        interface{}
	hasConst        bool
	minLength       *int
	maxLength       *int
	minItems        *int
	maxItems        *int
	minimum         *int64
	maximum         *int64
}

// supportedKeywords are the schema keywords this compiler understands.
var supportedKeywords = map[string]bool{
	"type": true, "properties": true, "required": true,
	"additionalProperties": true, "items": true, "enum": true, "const": true,
	"minLength": true, "maxLength": true, "minItems": true, "maxItems": true,
	"minimum": true, "maximum": true,
}

// ignoredAnnotations are non-constraint keywords accepted and ignored (so a
// schema can carry a title/description without being flagged as unsupported).
var ignoredAnnotations = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true, "$comment": true,
}

var validTypes = map[string]bool{
	"object": true, "array": true, "string": true,
	"integer": true, "boolean": true, "null": true,
}

// compile parses schema bytes into a validator, rejecting any unsupported
// keyword or malformed constraint. The bytes are canonicalized first, so the
// embedded contract inherits canonjson's duplicate-key, integer, Unicode, size,
// depth, and trailing-content rules — a schema with a duplicate keyword or a
// trailing document never compiles.
func compile(raw []byte) (*schemaNode, error) {
	canon, err := canonjson.Canonicalize(raw)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(canon, &m); err != nil {
		return nil, fmt.Errorf("schema: parse: %w", err)
	}
	return compileNode(m)
}

func compileNode(m map[string]json.RawMessage) (*schemaNode, error) {
	n := &schemaNode{additionalProps: true}
	// Deterministic keyword iteration for stable error messages.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if ignoredAnnotations[k] {
			// Recognized annotations have no evaluation effect but are still
			// type-checked: they must be strings.
			var s string
			if err := json.Unmarshal(m[k], &s); err != nil {
				return nil, fmt.Errorf("schema: annotation %q must be a string", k)
			}
			continue
		}
		if !supportedKeywords[k] {
			return nil, fmt.Errorf("schema: unsupported keyword %q", k)
		}
		if err := n.applyKeyword(k, m[k]); err != nil {
			return nil, err
		}
	}
	if err := n.enforceProfile(); err != nil {
		return nil, err
	}
	return n, nil
}

// enforceProfile applies the authored-schema contract (stricter than generic
// JSON Schema, matching the harvested provider-strict shape):
//   - every object type must declare properties, additionalProperties:false, and
//     a required list naming every property exactly once (optional values are
//     modeled as required-but-nullable via a "null" type union);
//   - every array type must declare items;
//   - a constraint keyword is rejected unless its applicable type is present
//     (a typeless const/enum is allowed);
//   - bounds must be coherent (min <= max), enum must be non-empty, and the type
//     list must not repeat an alternative.
func (n *schemaNode) enforceProfile() error {
	seen := make(map[string]bool, len(n.types))
	for _, t := range n.types {
		if seen[t] {
			return fmt.Errorf("schema: duplicate type alternative %q", t)
		}
		seen[t] = true
	}
	isObject := containsString(n.types, "object")
	isArray := containsString(n.types, "array")
	isString := containsString(n.types, "string")
	isInteger := containsString(n.types, "integer")

	// Constraint keywords require their applicable type.
	if (n.hasProperties || len(n.required) > 0 || n.hasAdditional) && !isObject {
		return fmt.Errorf("schema: object keywords require type object")
	}
	if (n.items != nil || n.minItems != nil || n.maxItems != nil) && !isArray {
		return fmt.Errorf("schema: array keywords require type array")
	}
	if (n.minLength != nil || n.maxLength != nil) && !isString {
		return fmt.Errorf("schema: string keywords require type string")
	}
	if (n.minimum != nil || n.maximum != nil) && !isInteger {
		return fmt.Errorf("schema: numeric keywords require type integer")
	}

	// Completeness of composite types.
	if isObject {
		if !n.hasProperties {
			return fmt.Errorf("schema: an object must declare properties")
		}
		if !n.hasAdditional || n.additionalProps {
			return fmt.Errorf("schema: an object must set additionalProperties:false")
		}
		if err := n.checkRequiredExact(); err != nil {
			return err
		}
	}
	if isArray && n.items == nil {
		return fmt.Errorf("schema: an array must declare items")
	}

	// Coherent bounds and non-empty enum.
	if n.minLength != nil && n.maxLength != nil && *n.minLength > *n.maxLength {
		return fmt.Errorf("schema: minLength exceeds maxLength")
	}
	if n.minItems != nil && n.maxItems != nil && *n.minItems > *n.maxItems {
		return fmt.Errorf("schema: minItems exceeds maxItems")
	}
	if n.minimum != nil && n.maximum != nil && *n.minimum > *n.maximum {
		return fmt.Errorf("schema: minimum exceeds maximum")
	}
	if n.hasEnum && len(n.enum) == 0 {
		return fmt.Errorf("schema: enum must not be empty")
	}
	return nil
}

func (n *schemaNode) checkRequiredExact() error {
	seen := make(map[string]bool, len(n.required))
	for _, r := range n.required {
		if seen[r] {
			return fmt.Errorf("schema: required lists %q more than once", r)
		}
		seen[r] = true
		if _, ok := n.properties[r]; !ok {
			return fmt.Errorf("schema: required names unknown property %q", r)
		}
	}
	for name := range n.properties {
		if !seen[name] {
			return fmt.Errorf("schema: property %q must be listed in required (optional values must be required-but-nullable)", name)
		}
	}
	return nil
}

func (n *schemaNode) applyKeyword(k string, raw json.RawMessage) error {
	switch k {
	case "type":
		return n.compileType(raw)
	case "properties":
		return n.compileProperties(raw)
	case "required":
		return decodeInto(raw, &n.required, "required")
	case "additionalProperties":
		var b bool
		if err := decodeInto(raw, &b, "additionalProperties"); err != nil {
			return err
		}
		n.additionalProps = b
		n.hasAdditional = true
	case "items":
		sub, err := compile(raw)
		if err != nil {
			return err
		}
		n.items = sub
	case "enum":
		vals, err := decodeValues(raw)
		if err != nil {
			return fmt.Errorf("schema: enum: %w", err)
		}
		n.enum, n.hasEnum = vals, true
	case "const":
		v, err := decodeValue(raw)
		if err != nil {
			return fmt.Errorf("schema: const: %w", err)
		}
		n.constVal, n.hasConst = v, true
	case "minLength":
		return decodeIntPtr(raw, &n.minLength)
	case "maxLength":
		return decodeIntPtr(raw, &n.maxLength)
	case "minItems":
		return decodeIntPtr(raw, &n.minItems)
	case "maxItems":
		return decodeIntPtr(raw, &n.maxItems)
	case "minimum":
		return decodeInt64Ptr(raw, &n.minimum)
	case "maximum":
		return decodeInt64Ptr(raw, &n.maximum)
	}
	return nil
}

func (n *schemaNode) compileType(raw json.RawMessage) error {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		n.types = []string{one}
	} else {
		if err := json.Unmarshal(raw, &n.types); err != nil {
			return fmt.Errorf("schema: type must be a string or array of strings")
		}
	}
	if len(n.types) == 0 {
		return fmt.Errorf("schema: type must not be empty")
	}
	for _, t := range n.types {
		if !validTypes[t] {
			return fmt.Errorf("schema: unsupported type %q (number is excluded; use integer)", t)
		}
	}
	return nil
}

func (n *schemaNode) compileProperties(raw json.RawMessage) error {
	var props map[string]json.RawMessage
	if err := json.Unmarshal(raw, &props); err != nil {
		return fmt.Errorf("schema: properties must be an object")
	}
	n.properties = make(map[string]*schemaNode, len(props))
	for name, sub := range props {
		node, err := compile(sub)
		if err != nil {
			return fmt.Errorf("schema: property %q: %w", name, err)
		}
		n.properties[name] = node
	}
	n.hasProperties = true
	return nil
}

// validate checks instance v against the compiled schema.
func (n *schemaNode) validate(v interface{}, path string) error {
	if len(n.types) > 0 {
		jt := jsonType(v)
		if !containsString(n.types, jt) {
			return fmt.Errorf("%s: expected type %s, got %s", path, strings.Join(n.types, "|"), jt)
		}
	}
	if n.hasConst && !valueEquals(v, n.constVal) {
		return fmt.Errorf("%s: value is not the required constant", path)
	}
	if n.hasEnum {
		ok := false
		for _, e := range n.enum {
			if valueEquals(v, e) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s: value is not in the allowed enum", path)
		}
	}
	switch val := v.(type) {
	case map[string]interface{}:
		return n.validateObject(val, path)
	case []interface{}:
		return n.validateArray(val, path)
	case string:
		return n.validateString(val, path)
	case json.Number:
		return n.validateNumber(val, path)
	}
	return nil
}

func (n *schemaNode) validateObject(obj map[string]interface{}, path string) error {
	for _, req := range n.required {
		if _, ok := obj[req]; !ok {
			return fmt.Errorf("%s: missing required property %q", path, req)
		}
	}
	if !n.additionalProps {
		// Report a count only: an instance key is a submitted value and could
		// carry a secret, so it is never echoed in a diagnostic. With no declared
		// properties every key is unexpected.
		extra := 0
		for k := range obj {
			if _, ok := n.properties[k]; !ok {
				extra++
			}
		}
		if extra > 0 {
			return fmt.Errorf("%s: %d property/properties not allowed by the schema", path, extra)
		}
	}
	// Validate present properties in sorted order for stable errors.
	names := make([]string, 0, len(n.properties))
	for name := range n.properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if pv, ok := obj[name]; ok {
			if err := n.properties[name].validate(pv, path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *schemaNode) validateArray(arr []interface{}, path string) error {
	if n.minItems != nil && len(arr) < *n.minItems {
		return fmt.Errorf("%s: fewer than %d items", path, *n.minItems)
	}
	if n.maxItems != nil && len(arr) > *n.maxItems {
		return fmt.Errorf("%s: more than %d items", path, *n.maxItems)
	}
	if n.items != nil {
		for i, e := range arr {
			if err := n.items.validate(e, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *schemaNode) validateString(s, path string) error {
	if n.minLength != nil && utf8.RuneCountInString(s) < *n.minLength {
		return fmt.Errorf("%s: shorter than %d characters", path, *n.minLength)
	}
	if n.maxLength != nil && utf8.RuneCountInString(s) > *n.maxLength {
		return fmt.Errorf("%s: longer than %d characters", path, *n.maxLength)
	}
	return nil
}

func (n *schemaNode) validateNumber(num json.Number, path string) error {
	if n.minimum == nil && n.maximum == nil {
		return nil
	}
	iv, err := num.Int64()
	if err != nil {
		return fmt.Errorf("%s: number is not a valid integer", path)
	}
	if n.minimum != nil && iv < *n.minimum {
		return fmt.Errorf("%s: less than %d", path, *n.minimum)
	}
	if n.maximum != nil && iv > *n.maximum {
		return fmt.Errorf("%s: greater than %d", path, *n.maximum)
	}
	return nil
}

// --- helpers ---

func jsonType(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return "number"
		}
		return "integer"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return "unknown"
	}
}

// valueEquals compares two decoded values by their restricted-canonical form, so
// enum/const comparison is exact and number-format independent.
func valueEquals(a, b interface{}) bool {
	ca, ea := canonjson.CanonicalizeValue(a)
	cb, eb := canonjson.CanonicalizeValue(b)
	return ea == nil && eb == nil && bytes.Equal(ca, cb)
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func decodeInto(raw json.RawMessage, dst interface{}, what string) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("schema: %s: %w", what, err)
	}
	return nil
}

func decodeIntPtr(raw json.RawMessage, dst **int) error {
	var i int
	if err := json.Unmarshal(raw, &i); err != nil {
		return fmt.Errorf("schema: expected an integer constraint: %w", err)
	}
	if i < 0 {
		return fmt.Errorf("schema: length/size constraint must be non-negative")
	}
	*dst = &i
	return nil
}

func decodeInt64Ptr(raw json.RawMessage, dst **int64) error {
	var i int64
	if err := json.Unmarshal(raw, &i); err != nil {
		return fmt.Errorf("schema: expected an integer bound: %w", err)
	}
	*dst = &i
	return nil
}

// decodeValue decodes a schema literal (for const) with number-preserving
// semantics.
func decodeValue(raw json.RawMessage) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func decodeValues(raw json.RawMessage) ([]interface{}, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]interface{}, 0, len(items))
	for _, it := range items {
		v, err := decodeValue(it)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
