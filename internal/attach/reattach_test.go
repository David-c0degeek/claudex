package attach

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

// treeDigest is a byte-for-byte digest of a directory tree (relative paths + file
// contents + dir markers), used to prove a read-only operation mutated nothing.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			lines = append(lines, "d:"+filepath.ToSlash(rel))
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(b)
		lines = append(lines, "f:"+filepath.ToSlash(rel)+":"+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func reattachReq(repo, runID, session string, agent state.Agent, role state.SlotRole) ReattachRequest {
	return ReattachRequest{RepoDir: repo, RunID: runID, SessionID: session, Agent: agent, Role: role}
}

// Lead reattach can happen before pair attach ever created the run lock: it is
// current, mutates nothing (byte-for-byte), and never creates <run>/run.lock.
func TestReattachLeadBeforePairIsReadOnly(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))

	before := treeDigest(t, repo)
	res, err := Reattach(reattachReq(repo, a.RunID, a.SessionID, state.AgentClaude, state.SlotLead))
	if err != nil {
		t.Fatalf("reattach lead: %v", err)
	}
	if res.Status != ReattachCurrent || res.SessionID != a.SessionID || res.Role != state.SlotLead {
		t.Fatalf("lead reattach = %+v", res)
	}
	if before != treeDigest(t, repo) {
		t.Fatalf("reattach mutated the filesystem")
	}
	if _, err := os.Stat(filepath.Join(runDir, "run.lock")); !os.IsNotExist(err) {
		t.Fatalf("reattach created the run lock: %v", err)
	}
}

// After a pair joins, both sessions reattach as current.
func TestReattachCurrentBothRoles(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	pair, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if r, _ := Reattach(reattachReq(repo, a.RunID, a.SessionID, state.AgentClaude, state.SlotLead)); r.Status != ReattachCurrent {
		t.Fatalf("lead reattach = %+v", r)
	}
	if r, _ := Reattach(reattachReq(repo, a.RunID, pair.SessionID, state.AgentCodex, state.SlotPair)); r.Status != ReattachCurrent {
		t.Fatalf("pair reattach = %+v", r)
	}
}

// A superseded session reattaches as replaced (with the current generation), never
// returning the replacement credential; the read mutates nothing.
func TestReattachReplaced(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	pair, _ := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	regStore := state.OpenRegistry(filepath.Join(runDir, "registry"), filepath.Join(runDir, "run.lock"))
	reg, _, _ := regStore.Load()
	newSess := "sess-" + strings.Repeat("e", 32)
	if _, err := regStore.Mutate(reg.Revision, func(gen uint64, next *state.Registry) error {
		next.Pair.Sessions = append(next.Pair.Sessions, state.SessionRecord{SessionID: newSess, Generation: 2, IssuedRegistryRevision: gen})
		next.Pair.CurrentSessionID = newSess
		return nil
	}); err != nil {
		t.Fatalf("replace pair: %v", err)
	}

	before := treeDigest(t, repo)
	res, err := Reattach(reattachReq(repo, a.RunID, pair.SessionID, state.AgentCodex, state.SlotPair))
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if res.Status != ReattachReplaced || res.SessionID != "" || res.CurrentGeneration != 2 {
		t.Fatalf("replaced reattach = %+v", res)
	}
	if before != treeDigest(t, repo) {
		t.Fatalf("reattach mutated the filesystem")
	}
}

// Unknown session, agent/role mismatch, a non-active run, and a pending attach
// journal each report their typed outcome and mutate nothing.
func TestReattachTypedOutcomes(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)

	t.Run("unknown", func(t *testing.T) {
		before := treeDigest(t, repo)
		r, _ := Reattach(reattachReq(repo, a.RunID, "sess-"+strings.Repeat("9", 32), state.AgentClaude, state.SlotLead))
		if r.Status != ReattachUnknown {
			t.Fatalf("unknown = %+v", r)
		}
		if before != treeDigest(t, repo) {
			t.Fatalf("mutated fs")
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		// The lead session presented with the pair role.
		r, _ := Reattach(reattachReq(repo, a.RunID, a.SessionID, state.AgentClaude, state.SlotPair))
		if r.Status != ReattachMismatch {
			t.Fatalf("mismatch = %+v", r)
		}
	})
	t.Run("stale run", func(t *testing.T) {
		other := "run-" + strings.Repeat("a", 32)
		r, _ := Reattach(reattachReq(repo, other, "sess-"+strings.Repeat("1", 32), state.AgentClaude, state.SlotLead))
		if r.Status != ReattachStale {
			t.Fatalf("stale = %+v", r)
		}
	})
}

// A pending pair-attach journal blocks reattach with recovery-required.
func TestReattachRecoveryRequired(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	fired := false
	stepFailpoint = func(s string) error {
		if s == "registry-pair-fill" && !fired {
			fired = true
			return errors.New("cut")
		}
		return nil
	}
	defer func() { stepFailpoint = nil }()
	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err == nil {
		t.Fatalf("expected a crash")
	}
	stepFailpoint = nil

	r, err := Reattach(reattachReq(repo, a.RunID, a.SessionID, state.AgentClaude, state.SlotLead))
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if r.Status != ReattachRecoveryRequired {
		t.Fatalf("pending-journal reattach = %+v, want recovery-required", r)
	}
}
