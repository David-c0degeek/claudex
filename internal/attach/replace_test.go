package attach

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

// replaceReq builds a same-role replacement request with a deterministic RNG.
func replaceReq(repo, runID string, role state.SlotRole, agent state.Agent, gen uint64, seed byte) ReplaceRequest {
	return ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: role, Agent: agent, ExpectedGeneration: gen,
		RNG: bytes.NewReader(bytes.Repeat([]byte{seed, 0x11, 0x22, 0x33}, 32)),
	}
}

// pairedRun bootstraps a lead-claude run and fills the codex pair.
func pairedRun(t *testing.T, repo string) (a FirstAttachResult, pairSession string) {
	t.Helper()
	a = bootstrapRun(t, repo)
	ja, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	return a, ja.SessionID
}

func loadReg(t *testing.T, repo, runID string) state.Registry {
	t.Helper()
	runDir := layoutFor(repo).runDir(state.RunDirRelFor(runID))
	reg, ok, err := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load()
	if err != nil || !ok {
		t.Fatalf("load registry: ok=%v err=%v", ok, err)
	}
	return reg
}

func loadRunState(t *testing.T, repo, runID string) state.RunState {
	t.Helper()
	runDir := layoutFor(repo).runDir(state.RunDirRelFor(runID))
	rs, ok, err := state.Open(filepath.Join(runDir, "state"), runLock(runDir)).Load()
	if err != nil || !ok {
		t.Fatalf("load run state: ok=%v err=%v", ok, err)
	}
	return rs
}

// A successful pair replacement: the new session is current at generation 2, the old
// session is replaced, the lead is untouched, and RunState does not advance.
func TestReplaceAttachSupersedesPair(t *testing.T) {
	repo := t.TempDir()
	a, oldPair := pairedRun(t, repo)
	rsBefore := loadRunState(t, repo, a.RunID)

	res, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20))
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if res.Generation != 2 || !state.IsSessionID(res.SessionID) || res.CommitWarning != nil || res.SessionID == oldPair {
		t.Fatalf("result = %+v", res)
	}

	reg := loadReg(t, repo, a.RunID)
	if r := reg.Resolve(res.SessionID); r.Status != state.RegCurrent || r.Role != state.SlotPair || r.Agent != state.AgentCodex || r.CurrentGeneration != 2 {
		t.Fatalf("new session not current: %+v", r)
	}
	if r := reg.Resolve(oldPair); r.Status != state.RegReplaced {
		t.Fatalf("old pair session not replaced (submit authority): %+v", r)
	}
	if r := reg.Resolve(a.SessionID); r.Status != state.RegCurrent || r.Role != state.SlotLead {
		t.Fatalf("lead disturbed: %+v", r)
	}
	// The append bound to the actual appended revision; the other slot is unchanged.
	if reg.Pair.Sessions[1].IssuedRegistryRevision != reg.Revision || len(reg.Lead.Sessions) != 1 {
		t.Fatalf("append not bound / lead history changed: %+v", reg)
	}
	if rs := loadRunState(t, repo, a.RunID); rs.Revision != rsBefore.Revision {
		t.Fatalf("RunState advanced: %d -> %d", rsBefore.Revision, rs.Revision)
	}
}

// The lead role can also be replaced; the pair is untouched.
func TestReplaceAttachSupersedesLead(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	res, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotLead, state.AgentClaude, 1, 0x20))
	if err != nil {
		t.Fatalf("replace lead: %v", err)
	}
	reg := loadReg(t, repo, a.RunID)
	if r := reg.Resolve(res.SessionID); r.Status != state.RegCurrent || r.Role != state.SlotLead {
		t.Fatalf("new lead not current: %+v", r)
	}
	if r := reg.Resolve(a.SessionID); r.Status != state.RegReplaced {
		t.Fatalf("old lead not replaced: %+v", r)
	}
	if len(reg.Pair.Sessions) != 1 {
		t.Fatalf("pair history changed: %+v", reg.Pair)
	}
}

func TestReplaceAttachWrongAgent(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	// The pair slot holds codex; a claude request is a mid-run switch, never replacement.
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentClaude, 1, 0x20)); !errors.Is(err, ErrReplaceAgentMismatch) {
		t.Fatalf("err = %v, want ErrReplaceAgentMismatch", err)
	}
}

// A stale expected generation is rejected and never leaks the current session id.
func TestReplaceAttachStaleGeneration(t *testing.T) {
	repo := t.TempDir()
	a, oldPair := pairedRun(t, repo)
	_, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 99, 0x20))
	if !errors.Is(err, ErrReplaceStaleGeneration) {
		t.Fatalf("err = %v, want ErrReplaceStaleGeneration", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte(oldPair)) {
		t.Fatalf("stale error leaked the current session id: %v", err)
	}
	// A second successful replacement then makes generation 1 stale.
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x21)); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x22)); !errors.Is(err, ErrReplaceStaleGeneration) {
		t.Fatalf("replay of generation 1 err = %v, want ErrReplaceStaleGeneration", err)
	}
}

// A run id that is not the active bootstrap allocation is unauthorized.
func TestReplaceAttachUnauthorized(t *testing.T) {
	repo := t.TempDir()
	pairedRun(t, repo)
	other := "run-" + "0123456789abcdef0123456789abcdef"
	if _, err := ReplaceAttach(replaceReq(repo, other, state.SlotPair, state.AgentCodex, 1, 0x20)); !errors.Is(err, ErrReplaceUnauthorized) {
		t.Fatalf("err = %v, want ErrReplaceUnauthorized", err)
	}
}

// A run that is not a completed pairing (no pair journal yet) cannot be replaced.
func TestReplaceAttachNotPaired(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo) // lead only; no JoinAttach
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotLead, state.AgentClaude, 1, 0x20)); !errors.Is(err, ErrReplaceRecoveryRequired) {
		t.Fatalf("err = %v, want ErrReplaceRecoveryRequired", err)
	}
}

// A vanished pair journal (after pairing) forces recovery before any Registry change.
func TestReplaceAttachJournalVanished(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	lay := layoutFor(repo)
	if err := os.RemoveAll(lay.attachJournalDir(lay.runDir(state.RunDirRelFor(a.RunID)))); err != nil {
		t.Fatalf("remove journal: %v", err)
	}
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)); !errors.Is(err, ErrReplaceRecoveryRequired) {
		t.Fatalf("err = %v, want ErrReplaceRecoveryRequired", err)
	}
	// No new session was appended.
	if reg := loadReg(t, repo, a.RunID); len(reg.Pair.Sessions) != 1 {
		t.Fatalf("a session was appended despite recovery-required: %+v", reg.Pair)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReplaceAttachRNGFailure(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	req := replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)
	req.RNG = errReader{}
	if _, err := ReplaceAttach(req); err == nil {
		t.Fatal("an RNG failure should abort the replacement")
	}
	if reg := loadReg(t, repo, a.RunID); len(reg.Pair.Sessions) != 1 {
		t.Fatalf("a session was appended despite an RNG failure: %+v", reg.Pair)
	}
}

func TestReplaceAttachValidation(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	for _, tc := range []struct {
		name string
		mut  func(*ReplaceRequest)
	}{
		{"nil rng", func(r *ReplaceRequest) { r.RNG = nil }},
		{"zero generation", func(r *ReplaceRequest) { r.ExpectedGeneration = 0 }},
		{"bad role", func(r *ReplaceRequest) { r.Role = "middle" }},
		{"non-canonical run", func(r *ReplaceRequest) { r.RunID = "not-a-run" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)
			tc.mut(&req)
			if _, err := ReplaceAttach(req); err == nil {
				t.Fatalf("%s should be rejected", tc.name)
			}
		})
	}
}

// Concurrent replacements at the same expected generation produce exactly one winner;
// the durable history gains exactly one new session.
func TestReplaceAttachConcurrent(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, errs[idx] = ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, byte(0x30+idx)))
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for i := 0; i < 2; i++ {
		if errs[i] == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly one winner, got %d (errs: %v)", wins, errs)
	}
	if reg := loadReg(t, repo, a.RunID); len(reg.Pair.Sessions) != 2 {
		t.Fatalf("want exactly one appended session, pair has %d", len(reg.Pair.Sessions))
	}
}
