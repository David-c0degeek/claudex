package state

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

// sid builds a valid minted session id ("sess-" + 32 hex of the given digit).
func sid(c string) string { return sessionIDPrefix + strings.Repeat(c, 32) }

func newRegistryDir(t *testing.T) (*RegistryStore, string) {
	t.Helper()
	dir := t.TempDir()
	return OpenRegistry(filepath.Join(dir, "registry"), filepath.Join(dir, "run.lock")), dir
}

func newRegistry(t *testing.T) *RegistryStore {
	t.Helper()
	s, _ := newRegistryDir(t)
	return s
}

func leadSlot(agent Agent, id string, gen, rev uint64) *RoleSlot {
	return &RoleSlot{Agent: agent, CurrentSessionID: id, Sessions: []SessionRecord{{SessionID: id, Generation: gen, IssuedRegistryRevision: rev}}}
}

// bootstrapLead fills the lead slot (claude) with a generation-1 session.
func bootstrapLead(t *testing.T, s *RegistryStore) Registry {
	t.Helper()
	reg, err := s.Mutate(0, func(gen uint64, next *Registry) error {
		next.RunID = "run-a"
		next.Lead = leadSlot(AgentClaude, sid("1"), 1, gen)
		return nil
	})
	if err != nil {
		t.Fatalf("bootstrap lead: %v", err)
	}
	return reg
}

func fillPair(t *testing.T, s *RegistryStore, reg Registry) Registry {
	t.Helper()
	out, err := s.Mutate(reg.Revision, func(gen uint64, next *Registry) error {
		next.Pair = leadSlot(AgentCodex, sid("3"), 1, gen)
		return nil
	})
	if err != nil {
		t.Fatalf("fill pair: %v", err)
	}
	return out
}

func TestRegistryResolve(t *testing.T) {
	s := newRegistry(t)
	reg := fillPair(t, s, bootstrapLead(t, s))

	if got := reg.Resolve(sid("1")); got.Status != RegCurrent || got.Role != SlotLead || got.Agent != AgentClaude {
		t.Fatalf("lead resolve = %+v", got)
	}
	if got := reg.Resolve(sid("3")); got.Status != RegCurrent || got.Role != SlotPair || got.Agent != AgentCodex {
		t.Fatalf("pair resolve = %+v", got)
	}
	if got := reg.Resolve(sid("9")); got.Status != RegUnknown {
		t.Fatalf("unknown resolve = %+v", got)
	}
}

// A replacement appends exactly one next generation with a new id; the superseded
// id resolves as replaced (not unknown), carrying the slot's current generation.
func TestRegistryReplacement(t *testing.T) {
	s := newRegistry(t)
	reg := bootstrapLead(t, s)
	reg, err := s.Mutate(reg.Revision, func(gen uint64, next *Registry) error {
		next.Lead.Sessions = append(next.Lead.Sessions, SessionRecord{SessionID: sid("2"), Generation: 2, IssuedRegistryRevision: gen})
		next.Lead.CurrentSessionID = sid("2")
		return nil
	})
	if err != nil {
		t.Fatalf("replace lead: %v", err)
	}
	if got := reg.Resolve(sid("2")); got.Status != RegCurrent || got.CurrentGeneration != 2 {
		t.Fatalf("new session resolve = %+v", got)
	}
	old := reg.Resolve(sid("1"))
	if old.Status != RegReplaced || old.Role != SlotLead || old.CurrentGeneration != 2 || old.CurrentSessionID != sid("2") {
		t.Fatalf("superseded session resolve = %+v, want replaced with current gen 2", old)
	}
}

// The only legal first generation is lead-filled (one generation-1 session) with
// an empty pair; every other init shape is rejected and nothing is written.
func TestRegistryInitRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(gen uint64, next *Registry)
	}{
		{"pair present at init", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = leadSlot(AgentClaude, sid("1"), 1, gen)
			next.Pair = leadSlot(AgentCodex, sid("3"), 1, gen)
		}},
		{"lead empty", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Pair = leadSlot(AgentCodex, sid("3"), 1, gen)
		}},
		{"lead multi-session at init", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = &RoleSlot{Agent: AgentClaude, CurrentSessionID: sid("2"), Sessions: []SessionRecord{
				{sid("1"), 1, gen}, {sid("2"), 2, gen},
			}}
		}},
		{"non-generation-1 lead", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = leadSlot(AgentClaude, sid("2"), 2, gen)
		}},
		{"current not last", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = &RoleSlot{Agent: AgentClaude, CurrentSessionID: sid("9"), Sessions: []SessionRecord{{sid("1"), 1, gen}}}
		}},
		{"non-minted id", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = leadSlot(AgentClaude, "sess-XYZ", 1, gen) // uppercase / short
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newRegistry(t)
			if _, err := s.Mutate(0, func(gen uint64, next *Registry) error { c.mut(gen, next); return nil }); err == nil {
				t.Fatalf("%s should be rejected", c.name)
			}
			if _, ok, _ := s.Load(); ok {
				t.Fatalf("a generation was written despite the rejection")
			}
		})
	}
}

// Every registry revision records exactly one attach/replacement; a bulk append,
// a two-slot change, a no-op, a stale binding, and history/agent tampering are
// all rejected.
func TestRegistryTransitionRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(gen uint64, next *Registry)
	}{
		{"bulk append", func(gen uint64, next *Registry) {
			next.Lead.Sessions = append(next.Lead.Sessions,
				SessionRecord{sid("2"), 2, gen}, SessionRecord{sid("4"), 3, gen})
			next.Lead.CurrentSessionID = sid("4")
		}},
		{"two-slot change", func(gen uint64, next *Registry) {
			next.Lead.Sessions = append(next.Lead.Sessions, SessionRecord{sid("2"), 2, gen})
			next.Lead.CurrentSessionID = sid("2")
			next.Pair = leadSlot(AgentCodex, sid("3"), 1, gen)
		}},
		{"no-op generation", func(gen uint64, next *Registry) {}},
		{"agent switch", func(gen uint64, next *Registry) { next.Lead.Agent = AgentCodex }},
		{"history tamper", func(gen uint64, next *Registry) {
			next.Lead.Sessions[0].SessionID = sid("7")
			next.Lead.CurrentSessionID = sid("7")
		}},
		{"slot cleared", func(gen uint64, next *Registry) { next.Lead = nil }},
		{"stale new session", func(gen uint64, next *Registry) {
			next.Lead.Sessions = append(next.Lead.Sessions, SessionRecord{sid("2"), 2, 1}) // issued at stale rev 1
			next.Lead.CurrentSessionID = sid("2")
		}},
		{"current drift without append", func(gen uint64, next *Registry) {
			next.Lead.CurrentSessionID = sid("9") // no record added
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newRegistry(t)
			reg := bootstrapLead(t, s)
			if _, err := s.Mutate(reg.Revision, func(gen uint64, next *Registry) error { c.mut(gen, next); return nil }); err == nil {
				t.Fatalf("%s should be rejected", c.name)
			}
		})
	}
}

// Session ids are globally unique across both slot histories.
func TestRegistryCrossSlotUniqueness(t *testing.T) {
	s := newRegistry(t)
	reg := bootstrapLead(t, s) // lead current is sid("1")
	if _, err := s.Mutate(reg.Revision, func(gen uint64, next *Registry) error {
		next.Pair = leadSlot(AgentCodex, sid("1"), 1, gen) // same id as the lead
		return nil
	}); err == nil {
		t.Fatalf("a session id reused across slots should be rejected")
	}
}

// A callback that changes the generation identity is rejected before any write.
func TestRegistryRejectsRevisionPoisoning(t *testing.T) {
	s := newRegistry(t)
	if _, err := s.Mutate(0, func(gen uint64, next *Registry) error {
		next.RunID = "run-a"
		next.Lead = leadSlot(AgentClaude, sid("1"), 1, gen)
		next.Revision = 999 // poison the generation identity
		return nil
	}); err == nil {
		t.Fatalf("a callback that changes the revision should be rejected")
	}
	if _, ok, _ := s.Load(); ok {
		t.Fatalf("a generation was written despite revision poisoning")
	}
}

func TestMintSessionID(t *testing.T) {
	// Deterministic RNG: the first draw collides with an existing id, the second
	// is fresh.
	seq := append(bytes.Repeat([]byte{0x00}, 16), bytes.Repeat([]byte{0x11}, 16)...)
	taken := func(id string) bool { return id == sid("0") }
	id, err := MintSessionID(bytes.NewReader(seq), taken)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if id != sid("1") {
		t.Fatalf("mint retried wrong: %q", id)
	}
	if !isSessionID(id) {
		t.Fatalf("minted id is not a canonical session id: %q", id)
	}
	// A nil RNG and an RNG failure both fail closed.
	if _, err := MintSessionID(nil, nil); err == nil {
		t.Fatalf("nil RNG should fail closed")
	}
	if _, err := MintSessionID(bytes.NewReader(nil), nil); err == nil {
		t.Fatalf("RNG failure should fail closed")
	}
	// Every one of the eight draws colliding exhausts with a typed error.
	if _, err := MintSessionID(bytes.NewReader(bytes.Repeat([]byte{0x5a}, 16*8)), func(string) bool { return true }); !errors.Is(err, ErrSessionIDExhausted) {
		t.Fatalf("all-collision mint err = %v, want ErrSessionIDExhausted", err)
	}
}

// Filling the pair with the same agent as the lead is rejected and leaves the
// registry head unchanged.
func TestRegistryRejectsSameAgentPair(t *testing.T) {
	s := newRegistry(t)
	reg := bootstrapLead(t, s) // lead is claude
	if _, err := s.Mutate(reg.Revision, func(gen uint64, next *Registry) error {
		next.Pair = leadSlot(AgentClaude, sid("3"), 1, gen) // same agent as lead
		return nil
	}); err == nil {
		t.Fatalf("filling the pair with the lead's agent should be rejected")
	}
	got, _, _ := s.Load()
	if got.Revision != reg.Revision || got.Pair != nil {
		t.Fatalf("registry head changed despite the rejection: %+v", got)
	}
}

// The older schema fails to decode with clear version remediation.
func TestRegistryRejectsOlderSchemaVersion(t *testing.T) {
	old := []byte(`{"schema_version":0,"run_id":"run-a","revision":1,"lead":null,"pair":null}`)
	if _, err := decodeRegistry(genstore.Record{Generation: 1, Payload: old}); !errors.Is(err, ErrUnsupportedRegistrySchema) {
		t.Fatalf("older registry schema err = %v, want ErrUnsupportedRegistrySchema", err)
	}
}

// The registry and the run state compose under one shared lock (serialized, not
// crash-atomic — the attach journal provides cross-store atomicity).
func TestRegistryComposesWithRunStateUnderOneLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "run.lock")
	rs := Open(filepath.Join(dir, "state"), lockPath)
	reg := OpenRegistry(filepath.Join(dir, "registry"), lockPath)

	g, ok, err := genstore.Acquire(lockPath)
	if err != nil || !ok {
		t.Fatalf("acquire shared lock: %v", err)
	}
	if _, err := rs.MutateLocked(g, 0, func(_ uint64, next *RunState) error { initState(next); return nil }); err != nil {
		t.Fatalf("state init under shared lock: %v", err)
	}
	if _, err := reg.MutateLocked(g, 0, func(gen uint64, next *Registry) error {
		next.RunID = "run-a"
		next.Lead = leadSlot(AgentClaude, sid("1"), 1, gen)
		return nil
	}); err != nil {
		t.Fatalf("registry mutate under shared lock: %v", err)
	}
	if err := g.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	if got, ok, _ := rs.Load(); !ok || got.RunID != "run-a" {
		t.Fatalf("run state not persisted")
	}
	if got, ok, _ := reg.Load(); !ok || got.Resolve(sid("1")).Status != RegCurrent || got.RunID != "run-a" {
		t.Fatalf("registry not persisted/resolvable")
	}
}
