package canonjson

import (
	"strconv"
	"strings"
	"testing"
)

func mustCanon(t *testing.T, raw string) string {
	t.Helper()
	c, err := Canonicalize([]byte(raw))
	if err != nil {
		t.Fatalf("canonicalize %q: %v", raw, err)
	}
	return string(c)
}

func mustReject(t *testing.T, raw string) {
	t.Helper()
	if c, err := Canonicalize([]byte(raw)); err == nil {
		t.Fatalf("expected %q to be rejected, got %q", raw, c)
	}
}

// Formatting, key order, and insignificant whitespace must not change the
// canonical bytes or the digest.
func TestStableAcrossFormattingAndKeyOrder(t *testing.T) {
	a := `{ "b": 1, "a": [ 1, 2, {"y":2,"x":1} ], "c": "hi" }`
	b := "{\n  \"c\":\"hi\",\n  \"a\":[1,2,{\"x\":1,\"y\":2}],\n  \"b\":1\n}"
	ca, cb := mustCanon(t, a), mustCanon(t, b)
	if ca != cb {
		t.Fatalf("canonical mismatch:\n a=%s\n b=%s", ca, cb)
	}
	want := `{"a":[1,2,{"x":1,"y":2}],"b":1,"c":"hi"}`
	if ca != want {
		t.Fatalf("canonical = %s, want %s", ca, want)
	}
	da, err := Digest([]byte(a))
	if err != nil {
		t.Fatalf("digest a: %v", err)
	}
	db, _ := Digest([]byte(b))
	if da != db {
		t.Fatalf("digest mismatch %s != %s", da, db)
	}
	if len(da) != 64 {
		t.Fatalf("digest length = %d, want 64", len(da))
	}
}

func TestIntegerDomain(t *testing.T) {
	if got := mustCanon(t, `-0`); got != "0" {
		t.Fatalf("-0 canonicalized to %q, want 0", got)
	}
	if got := mustCanon(t, `{"n":100}`); got != `{"n":100}` {
		t.Fatalf("integer canonical = %q", got)
	}
	// Safe-integer boundary is accepted; one past it is rejected.
	maxTok := strconv.FormatInt(MaxSafeInt, 10)
	if got := mustCanon(t, maxTok); got != maxTok {
		t.Fatalf("max safe int = %q, want %q", got, maxTok)
	}
	if got := mustCanon(t, "-"+maxTok); got != "-"+maxTok {
		t.Fatalf("min safe int = %q", got)
	}
	mustReject(t, "9007199254740992")  // 2^53
	mustReject(t, "-9007199254740992") // -(2^53)
	// Non-integer forms, even mathematically integral ones.
	for _, bad := range []string{`1.5`, `2e3`, `2E3`, `0.0`, `1.0`, `1e0`, `{"x":1.0}`, `[1, 2.5]`} {
		mustReject(t, bad)
	}
	// Leading zeros are not canonical JSON.
	mustReject(t, `01`)
	mustReject(t, `-01`)
}

func TestRejectsDuplicateKeys(t *testing.T) {
	mustReject(t, `{"a":1,"a":2}`)
	mustReject(t, `{"a":1,"b":2,"a":3}`)
}

func TestStringEscaping(t *testing.T) {
	// Quote, backslash, the short control escapes, a \u00xx control, and a
	// literal non-ASCII rune.
	raw := "\"a\\\"b\\\\c\\n\\t\\u0001\\u00e9\""
	got := mustCanon(t, raw)
	want := "\"a\\\"b\\\\c\\n\\t\\u0001é\""
	if got != want {
		t.Fatalf("escaping = %q, want %q", got, want)
	}
	// Solidus is NOT escaped and may appear escaped or literal on input.
	if got := mustCanon(t, `"a\/b"`); got != `"a/b"` {
		t.Fatalf("solidus should canonicalize to literal: %q", got)
	}
	// A valid surrogate pair decodes to its supplementary rune, emitted literally.
	if got := mustCanon(t, `"😀"`); got != "\"\U0001F600\"" {
		t.Fatalf("surrogate pair = %q", got)
	}
}

func TestRejectsBadUnicode(t *testing.T) {
	mustReject(t, `"\ud800"`)  // lone high surrogate
	mustReject(t, `"\udc00"`)  // lone low surrogate
	mustReject(t, `"\ud800A"`) // high surrogate followed by non-surrogate
	mustReject(t, "\"\x01\"")  // unescaped control character
	mustReject(t, `"\x41"`)    // invalid escape
}

// Object keys sort by UTF-16 code units, not code points: a supplementary-plane
// key (high surrogate 0xD83D...) sorts BEFORE U+FFFF, the opposite of code-point
// order.
func TestKeysSortByUTF16(t *testing.T) {
	got := mustCanon(t, `{"￿":1,"😀":2}`)
	want := "{\"\U0001F600\":2,\"￿\":1}"
	if got != want {
		t.Fatalf("utf-16 key order = %q, want %q", got, want)
	}
}

func TestRejectsTrailingContentAndGarbage(t *testing.T) {
	mustReject(t, `{"a":1} {"b":2}`)
	mustReject(t, `not json`)
	mustReject(t, `[1,2,]`)
	mustReject(t, `{"a":1,}`)
}

func TestCanonicalizeValueAndDigestValue(t *testing.T) {
	type inner struct {
		X int `json:"x"`
	}
	type msg struct {
		B int    `json:"b"`
		A string `json:"a"`
		C inner  `json:"c"`
	}
	m := msg{B: 2, A: "hi", C: inner{X: 3}}
	c, err := CanonicalizeValue(m)
	if err != nil {
		t.Fatalf("canonicalize value: %v", err)
	}
	if string(c) != `{"a":"hi","b":2,"c":{"x":3}}` {
		t.Fatalf("value canonical = %s", c)
	}
	d, err := DigestValue(m)
	if err != nil || len(d) != 64 {
		t.Fatalf("digest value = %q err=%v", d, err)
	}

	// A float field is rejected (stays inside the integer domain).
	if _, err := CanonicalizeValue(struct {
		F float64 `json:"f"`
	}{F: 1.5}); err == nil {
		t.Fatalf("a float field should be rejected")
	}
}

// HTML-escaping by encoding/json (e.g. of <, >, &) must not leak into the
// canonical output: decoding reverses it.
func TestHTMLEscapesDoNotLeak(t *testing.T) {
	c, err := CanonicalizeValue(map[string]string{"k": "a<b>&c"})
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	if !strings.Contains(string(c), "a<b>&c") {
		t.Fatalf("canonical should carry literal characters: %s", c)
	}
}

func TestRejectsOversizeAndDeepNesting(t *testing.T) {
	mustReject(t, string(make([]byte, maxSize+1)))
	deep := strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2)
	mustReject(t, deep)
}
