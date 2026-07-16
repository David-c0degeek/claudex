package state

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
)

func newRegistryDir(t *testing.T) (*RegistryStore, string) {
	t.Helper()
	dir := t.TempDir()
	regDir := filepath.Join(dir, "registry")
	return OpenRegistry(regDir, filepath.Join(dir, "run.lock")), dir
}

func newRegistry(t *testing.T) *RegistryStore {
	t.Helper()
	s, _ := newRegistryDir(t)
	return s
}

// bootstrapLead fills the lead slot with a claude session and returns the registry.
func bootstrapLead(t *testing.T, s *RegistryStore, sid string) Registry {
	t.Helper()
	reg, err := s.Mutate(0, func(gen uint64, next *Registry) error {
		next.RunID = "run-a"
		next.Lead = &RoleSlot{
			Agent:            AgentClaude,
			CurrentSessionID: sid,
			Sessions:         []SessionRecord{{SessionID: sid, Generation: 1, IssuedRegistryRevision: gen}},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bootstrap lead: %v", err)
	}
	return reg
}

func TestRegistryResolve(t *testing.T) {
	s := newRegistry(t)
	reg := bootstrapLead(t, s, "sess-lead1")
	// Fill the pair with the complementary agent.
	reg, err := s.Mutate(reg.Revision, func(gen uint64, next *Registry) error {
		next.Pair = &RoleSlot{
			Agent:            AgentCodex,
			CurrentSessionID: "sess-pair1",
			Sessions:         []SessionRecord{{SessionID: "sess-pair1", Generation: 1, IssuedRegistryRevision: gen}},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fill pair: %v", err)
	}

	if got := reg.Resolve("sess-lead1"); got.Status != RegCurrent || got.Role != SlotLead || got.Agent != AgentClaude {
		t.Fatalf("lead resolve = %+v", got)
	}
	if got := reg.Resolve("sess-pair1"); got.Status != RegCurrent || got.Role != SlotPair || got.Agent != AgentCodex {
		t.Fatalf("pair resolve = %+v", got)
	}
	if got := reg.Resolve("sess-nope"); got.Status != RegUnknown {
		t.Fatalf("unknown resolve = %+v", got)
	}
}

// A replacement appends a new generation and a new session id; the superseded id
// resolves as replaced (not unknown), carrying the slot's current generation.
func TestRegistryReplacement(t *testing.T) {
	s := newRegistry(t)
	reg := bootstrapLead(t, s, "sess-lead1")
	reg, err := s.Mutate(reg.Revision, func(gen uint64, next *Registry) error {
		next.Lead.Sessions = append(next.Lead.Sessions, SessionRecord{SessionID: "sess-lead2", Generation: 2, IssuedRegistryRevision: gen})
		next.Lead.CurrentSessionID = "sess-lead2"
		return nil
	})
	if err != nil {
		t.Fatalf("replace lead: %v", err)
	}
	if got := reg.Resolve("sess-lead2"); got.Status != RegCurrent || got.CurrentGeneration != 2 {
		t.Fatalf("new session resolve = %+v", got)
	}
	old := reg.Resolve("sess-lead1")
	if old.Status != RegReplaced || old.Role != SlotLead || old.CurrentGeneration != 2 || old.CurrentSessionID != "sess-lead2" {
		t.Fatalf("superseded session resolve = %+v, want replaced with current gen 2", old)
	}
}

func TestRegistryRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(gen uint64, next *Registry)
	}{
		{"same agent both slots", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = &RoleSlot{Agent: AgentClaude, CurrentSessionID: "s1", Sessions: []SessionRecord{{"s1", 1, gen}}}
			next.Pair = &RoleSlot{Agent: AgentClaude, CurrentSessionID: "s2", Sessions: []SessionRecord{{"s2", 1, gen}}}
		}},
		{"non-consecutive generations", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = &RoleSlot{Agent: AgentClaude, CurrentSessionID: "s2", Sessions: []SessionRecord{{"s2", 2, gen}}}
		}},
		{"current not last", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = &RoleSlot{Agent: AgentClaude, CurrentSessionID: "sX", Sessions: []SessionRecord{{"s1", 1, gen}}}
		}},
		{"duplicate session id across slots", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = &RoleSlot{Agent: AgentClaude, CurrentSessionID: "dup", Sessions: []SessionRecord{{"dup", 1, gen}}}
			next.Pair = &RoleSlot{Agent: AgentCodex, CurrentSessionID: "dup", Sessions: []SessionRecord{{"dup", 1, gen}}}
		}},
		{"secret session id", func(gen uint64, next *Registry) {
			next.RunID = "run-a"
			next.Lead = &RoleSlot{Agent: AgentClaude, CurrentSessionID: "sk-ant-abcdefghijklmnopqrstuvwx", Sessions: []SessionRecord{{"sk-ant-abcdefghijklmnopqrstuvwx", 1, gen}}}
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

// Immutable-once-filled: agent cannot change, history is append-only, a filled
// slot cannot be cleared.
func TestRegistryTransitionImmutability(t *testing.T) {
	t.Run("agent immutable", func(t *testing.T) {
		s := newRegistry(t)
		reg := bootstrapLead(t, s, "sess-lead1")
		if _, err := s.Mutate(reg.Revision, func(_ uint64, next *Registry) error {
			next.Lead.Agent = AgentCodex
			return nil
		}); err == nil {
			t.Fatalf("changing a slot agent should be rejected")
		}
	})
	t.Run("history frozen", func(t *testing.T) {
		s := newRegistry(t)
		reg := bootstrapLead(t, s, "sess-lead1")
		if _, err := s.Mutate(reg.Revision, func(_ uint64, next *Registry) error {
			next.Lead.Sessions[0].SessionID = "sess-tamper"
			next.Lead.CurrentSessionID = "sess-tamper"
			return nil
		}); err == nil {
			t.Fatalf("rewriting an existing session record should be rejected")
		}
	})
	t.Run("slot not cleared", func(t *testing.T) {
		s := newRegistry(t)
		reg := bootstrapLead(t, s, "sess-lead1")
		if _, err := s.Mutate(reg.Revision, func(_ uint64, next *Registry) error {
			next.Lead = nil
			return nil
		}); err == nil {
			t.Fatalf("clearing a filled slot should be rejected")
		}
	})
	t.Run("new session must bind to this revision", func(t *testing.T) {
		s := newRegistry(t)
		reg := bootstrapLead(t, s, "sess-lead1")
		if _, err := s.Mutate(reg.Revision, func(_ uint64, next *Registry) error {
			next.Lead.Sessions = append(next.Lead.Sessions, SessionRecord{SessionID: "sess-lead2", Generation: 2, IssuedRegistryRevision: 1}) // stale
			next.Lead.CurrentSessionID = "sess-lead2"
			return nil
		}); err == nil {
			t.Fatalf("a new session bound to a stale revision should be rejected")
		}
	})
}

func TestMintSessionID(t *testing.T) {
	// Deterministic RNG: first draw collides with an existing id, second is fresh.
	seq := bytes.Repeat([]byte{0x00}, 16)
	seq = append(seq, bytes.Repeat([]byte{0x11}, 16)...)
	rng := bytes.NewReader(seq)
	taken := func(id string) bool { return id == "sess-"+"00000000000000000000000000000000" }
	id, err := MintSessionID(rng, taken)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if id != "sess-"+"11111111111111111111111111111111" {
		t.Fatalf("mint retried wrong: %q", id)
	}
	if !validID(id) {
		t.Fatalf("minted id is not canonical: %q", id)
	}

	// RNG failure fails closed.
	if _, err := MintSessionID(bytes.NewReader(nil), nil); err == nil {
		t.Fatalf("RNG failure should fail closed")
	}
}

// The older schema fails to decode with clear version remediation.
func TestRegistryRejectsOlderSchemaVersion(t *testing.T) {
	old := []byte(`{"schema_version":0,"run_id":"run-a","revision":1,"lead":null,"pair":null}`)
	if _, err := decodeRegistry(genstore.Record{Generation: 1, Payload: old}); !errors.Is(err, ErrUnsupportedRegistrySchema) {
		t.Fatalf("older registry schema err = %v, want ErrUnsupportedRegistrySchema", err)
	}
}

// The registry and the run state mutate atomically under one shared run lock.
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
		next.Lead = &RoleSlot{Agent: AgentClaude, CurrentSessionID: "sess-lead1", Sessions: []SessionRecord{{"sess-lead1", 1, gen}}}
		return nil
	}); err != nil {
		t.Fatalf("registry mutate under shared lock: %v", err)
	}
	if err := g.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	if _, ok, _ := rs.Load(); !ok {
		t.Fatalf("run state not persisted")
	}
	if got, ok, _ := reg.Load(); !ok || got.Resolve("sess-lead1").Status != RegCurrent {
		t.Fatalf("registry not persisted/resolvable")
	}
}
