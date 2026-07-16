// Package canonjson produces deterministic, whitespace- and key-order-
// independent canonical bytes for a JSON value, plus a sha256 digest over those
// bytes, so two submissions that differ only in formatting collapse to one
// identity.
//
// It follows RFC 8785 (JSON Canonicalization Scheme) for object-key ordering
// and string escaping, but deliberately restricts the numeric domain to safe
// integers: a non-integer JSON number, or an integer outside ±(2^53-1), is
// rejected. Full RFC 8785 number serialization is ECMAScript shortest-round-
// trip double formatting, which is a hazard to reproduce exactly without a
// dependency, and any integer beyond 2^53 loses identity when a JavaScript
// provider parses it. Protocol artifacts therefore carry fractional quantities
// as integer-scaled minor units (e.g. cents, milliseconds) and full-width
// identities/counters as strings, never as floats or large numbers.
//
// The parser is strict by construction: it rejects duplicate object keys, lone
// UTF-16 surrogates, invalid UTF-8, leading zeros, trailing content, and input
// past bounded size/nesting limits — so canonicalization is injective on the
// inputs it accepts and never silently normalizes an ambiguous document.
package canonjson

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxSafeInt is the largest integer with an exact double representation
// (2^53-1); the canonical numeric domain is [-MaxSafeInt, MaxSafeInt].
const MaxSafeInt = 1<<53 - 1

const (
	maxDepth = 64
	maxSize  = 1 << 20 // 1 MiB
)

// intLit is a validated, canonical base-10 integer literal.
type intLit string

// Canonicalize returns the canonical form of exactly one JSON value in raw.
func Canonicalize(raw []byte) ([]byte, error) {
	if len(raw) > maxSize {
		return nil, fmt.Errorf("canonjson: input exceeds %d bytes", maxSize)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("canonjson: input is not valid UTF-8")
	}
	p := &parser{s: raw}
	p.skipWS()
	v, err := p.parseValue(0)
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("canonjson: unexpected trailing content at byte %d", p.i)
	}
	var b strings.Builder
	writeValue(&b, v)
	return []byte(b.String()), nil
}

// CanonicalizeValue marshals a Go value and canonicalizes the result. An integer
// Go type keeps the value inside the numeric domain; a float field, or an
// integer beyond ±(2^53-1), is rejected by Canonicalize.
func CanonicalizeValue(v interface{}) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonjson: marshal: %w", err)
	}
	return Canonicalize(raw)
}

// Digest returns the lower-hex sha256 over the canonical bytes of raw.
func Digest(raw []byte) (string, error) {
	c, err := Canonicalize(raw)
	if err != nil {
		return "", err
	}
	return hexDigest(c), nil
}

// DigestValue returns the lower-hex sha256 over the canonical bytes of v.
func DigestValue(v interface{}) (string, error) {
	c, err := CanonicalizeValue(v)
	if err != nil {
		return "", err
	}
	return hexDigest(c), nil
}

func hexDigest(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// --- strict recursive-descent parser over raw bytes ---

type parser struct {
	s []byte
	i int
}

type member struct {
	key string
	val interface{}
}

func (p *parser) skipWS() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) parseValue(depth int) (interface{}, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("canonjson: nesting exceeds %d", maxDepth)
	}
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("canonjson: unexpected end of input")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.parseObject(depth)
	case c == '[':
		return p.parseArray(depth)
	case c == '"':
		return p.parseString()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	case c == 't':
		return p.parseLiteral("true", true)
	case c == 'f':
		return p.parseLiteral("false", false)
	case c == 'n':
		return p.parseLiteral("null", nil)
	default:
		return nil, fmt.Errorf("canonjson: unexpected byte %q at %d", c, p.i)
	}
}

func (p *parser) parseLiteral(word string, v interface{}) (interface{}, error) {
	if p.i+len(word) > len(p.s) || string(p.s[p.i:p.i+len(word)]) != word {
		return nil, fmt.Errorf("canonjson: invalid literal at %d", p.i)
	}
	p.i += len(word)
	return v, nil
}

func (p *parser) parseObject(depth int) (interface{}, error) {
	p.i++ // '{'
	members := []member{}
	seen := map[string]bool{}
	p.skipWS()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return members, nil
	}
	for {
		p.skipWS()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, fmt.Errorf("canonjson: expected object key at %d", p.i)
		}
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		ks := key.(string)
		if seen[ks] {
			return nil, fmt.Errorf("canonjson: duplicate object key %q", ks)
		}
		seen[ks] = true
		p.skipWS()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, fmt.Errorf("canonjson: expected ':' at %d", p.i)
		}
		p.i++
		p.skipWS()
		val, err := p.parseValue(depth + 1)
		if err != nil {
			return nil, err
		}
		members = append(members, member{key: ks, val: val})
		p.skipWS()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("canonjson: unterminated object")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return members, nil
		default:
			return nil, fmt.Errorf("canonjson: expected ',' or '}' at %d", p.i)
		}
	}
}

func (p *parser) parseArray(depth int) (interface{}, error) {
	p.i++ // '['
	arr := []interface{}{}
	p.skipWS()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return arr, nil
	}
	for {
		p.skipWS()
		v, err := p.parseValue(depth + 1)
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		p.skipWS()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("canonjson: unterminated array")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return arr, nil
		default:
			return nil, fmt.Errorf("canonjson: expected ',' or ']' at %d", p.i)
		}
	}
}

func (p *parser) parseString() (interface{}, error) {
	p.i++ // opening quote
	var sb strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return sb.String(), nil
		case c == '\\':
			r, err := p.parseEscape()
			if err != nil {
				return nil, err
			}
			sb.WriteRune(r)
		case c < 0x20:
			return nil, fmt.Errorf("canonjson: unescaped control character 0x%02x in string", c)
		default:
			r, size := utf8.DecodeRune(p.s[p.i:])
			if r == utf8.RuneError && size == 1 {
				return nil, fmt.Errorf("canonjson: invalid UTF-8 in string")
			}
			sb.WriteRune(r)
			p.i += size
		}
	}
	return nil, fmt.Errorf("canonjson: unterminated string")
}

// parseEscape consumes a backslash escape (p.s[p.i] == '\\') and returns the
// decoded rune, validating \u surrogate pairing and rejecting lone surrogates.
func (p *parser) parseEscape() (rune, error) {
	p.i++ // backslash
	if p.i >= len(p.s) {
		return 0, fmt.Errorf("canonjson: dangling escape")
	}
	c := p.s[p.i]
	p.i++
	switch c {
	case '"':
		return '"', nil
	case '\\':
		return '\\', nil
	case '/':
		return '/', nil
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case 'u':
		return p.parseUnicodeEscape()
	default:
		return 0, fmt.Errorf("canonjson: invalid escape \\%c", c)
	}
}

func (p *parser) parseUnicodeEscape() (rune, error) {
	h1, err := p.readHex4()
	if err != nil {
		return 0, err
	}
	switch {
	case h1 >= 0xD800 && h1 <= 0xDBFF: // high surrogate: needs a low surrogate
		if p.i+1 >= len(p.s) || p.s[p.i] != '\\' || p.s[p.i+1] != 'u' {
			return 0, fmt.Errorf("canonjson: lone high surrogate \\u%04x", h1)
		}
		p.i += 2
		h2, err := p.readHex4()
		if err != nil {
			return 0, err
		}
		if h2 < 0xDC00 || h2 > 0xDFFF {
			return 0, fmt.Errorf("canonjson: invalid low surrogate \\u%04x", h2)
		}
		return utf16.DecodeRune(rune(h1), rune(h2)), nil
	case h1 >= 0xDC00 && h1 <= 0xDFFF:
		return 0, fmt.Errorf("canonjson: lone low surrogate \\u%04x", h1)
	default:
		return rune(h1), nil
	}
}

func (p *parser) readHex4() (int, error) {
	if p.i+4 > len(p.s) {
		return 0, fmt.Errorf("canonjson: truncated \\u escape")
	}
	n, err := strconv.ParseUint(string(p.s[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, fmt.Errorf("canonjson: invalid \\u escape %q", string(p.s[p.i:p.i+4]))
	}
	p.i += 4
	return int(n), nil
}

func (p *parser) parseNumber() (interface{}, error) {
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			p.i++
		} else {
			break
		}
	}
	tok := string(p.s[start:p.i])
	for _, c := range tok {
		if c == '.' || c == 'e' || c == 'E' {
			return nil, fmt.Errorf("canonjson: non-integer number %q not allowed (use integer-scaled units or a string)", tok)
		}
	}
	digits := tok
	if strings.HasPrefix(digits, "-") {
		digits = digits[1:]
	}
	if len(digits) == 0 {
		return nil, fmt.Errorf("canonjson: invalid number %q", tok)
	}
	if len(digits) > 1 && digits[0] == '0' {
		return nil, fmt.Errorf("canonjson: leading zero in number %q", tok)
	}
	v, err := strconv.ParseInt(tok, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("canonjson: invalid integer %q: %w", tok, err)
	}
	if v > MaxSafeInt || v < -MaxSafeInt {
		return nil, fmt.Errorf("canonjson: integer %d outside the safe range ±(2^53-1); use a string", v)
	}
	return intLit(strconv.FormatInt(v, 10)), nil
}

// --- canonical serializer ---

func writeValue(b *strings.Builder, v interface{}) {
	switch val := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if val {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case intLit:
		b.WriteString(string(val))
	case string:
		writeString(b, val)
	case []interface{}:
		b.WriteByte('[')
		for i, e := range val {
			if i > 0 {
				b.WriteByte(',')
			}
			writeValue(b, e)
		}
		b.WriteByte(']')
	case []member:
		sort.Slice(val, func(i, j int) bool { return lessUTF16(val[i].key, val[j].key) })
		b.WriteByte('{')
		for i, m := range val {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, m.key)
			b.WriteByte(':')
			writeValue(b, m.val)
		}
		b.WriteByte('}')
	default:
		// Unreachable: the parser only produces the types above.
		panic(fmt.Sprintf("canonjson: unexpected node %T", v))
	}
}

// writeString emits a JSON string with RFC 8785 minimal escaping: escape only
// the mandatory characters, using the short forms where they exist and lower-
// hex \u00xx for the remaining control characters; all other runes (including
// non-ASCII) are emitted literally as UTF-8.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// lessUTF16 orders two strings by their UTF-16 code units, as RFC 8785 requires
// for object-key sorting (this differs from code-point order only for
// characters outside the Basic Multilingual Plane).
func lessUTF16(a, bb string) bool {
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(bb))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}
