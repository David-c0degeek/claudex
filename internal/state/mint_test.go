package state

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// constReader fills every read with a fixed byte, so each 16-byte draw yields the
// same id — used to force minting exhaustion deterministically.
type constReader byte

func (c constReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c)
	}
	return len(p), nil
}

func block(b byte) []byte {
	out := make([]byte, 16)
	for i := range out {
		out[i] = b
	}
	return out
}

func hexOf(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0xf]})
}

func TestMintTurnAndGateIDShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mint   func([]byte, map[string]bool) (string, error)
		prefix string
	}{
		{"turn", func(seed []byte, taken map[string]bool) (string, error) {
			return MintTurnID(bytes.NewReader(seed), taken)
		}, "turn-"},
		{"gate", func(seed []byte, taken map[string]bool) (string, error) {
			return MintGateID(bytes.NewReader(seed), taken)
		}, "gate-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := tc.mint(block(0xab), nil)
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if !strings.HasPrefix(id, tc.prefix) {
				t.Fatalf("id %q lacks prefix %q", id, tc.prefix)
			}
			// The minted id is a valid persisted run-id (the single authority); there is
			// no separate turn/gate predicate.
			if !IsRunID(id) {
				t.Fatalf("minted id %q is not a valid run id", id)
			}
			if id != tc.prefix+strings.Repeat("ab", 16) {
				t.Fatalf("id %q not the expected hex of the seed", id)
			}
		})
	}
}

// A taken id is skipped and the next draw is returned.
func TestMintTurnIDSkipsTaken(t *testing.T) {
	seed := append(block(0x01), block(0x02)...)
	takenID := "turn-" + strings.Repeat(hexOf(0x01), 16)
	nextID := "turn-" + strings.Repeat(hexOf(0x02), 16)
	got, err := MintTurnID(bytes.NewReader(seed), map[string]bool{takenID: true})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if got == takenID {
		t.Fatalf("minter returned the taken id %q", got)
	}
	if got != nextID {
		t.Fatalf("mint = %q, want the second draw %q", got, nextID)
	}
}

func TestMintNilRNG(t *testing.T) {
	if _, err := MintTurnID(nil, nil); err == nil {
		t.Fatal("nil RNG should error")
	}
	if _, err := MintGateID(nil, nil); err == nil {
		t.Fatal("nil RNG should error")
	}
}

// When every draw collides with a taken id, minting exhausts its bounded attempts.
func TestMintExhausted(t *testing.T) {
	id0, err := MintGateID(constReader(0xcd), nil)
	if err != nil {
		t.Fatalf("prime: %v", err)
	}
	if _, err := MintGateID(constReader(0xcd), map[string]bool{id0: true}); !errors.Is(err, ErrMintExhausted) {
		t.Fatalf("err = %v, want ErrMintExhausted", err)
	}
}
