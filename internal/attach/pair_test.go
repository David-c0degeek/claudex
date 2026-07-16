package attach

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/David-c0degeek/claudex/internal/state"
)

// bootstrapRun bootstraps a lead-claude run and returns its id.
func bootstrapRun(t *testing.T, repo string) FirstAttachResult {
	t.Helper()
	a, err := FirstAttach(newRequest(t, repo, &fakeWorktree{}))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return a
}

func joinRequest(repo, runID, op string, seed byte) JoinAttachRequest {
	return JoinAttachRequest{
		RepoDir:     repo,
		RunID:       runID,
		OperationID: op,
		Agent:       state.AgentCodex,
		Role:        state.SlotPair,
		Now:         2000,
		RNG:         bytes.NewReader(bytes.Repeat([]byte{seed, 0x5b, 0xc6, 0xd7}, 32)),
	}
}

func TestJoinAttachHappyPath(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)

	res, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if res.RunID != a.RunID || !state.IsSessionID(res.SessionID) || res.Role != state.SlotPair || res.Agent != state.AgentCodex || !state.IsRunID(res.FirstTurnID) {
		t.Fatalf("join result = %+v", res)
	}

	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))

	// The pair resolves current; the lead is unchanged.
	reg, _, _ := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load()
	if r := reg.Resolve(res.SessionID); r.Status != state.RegCurrent || r.Role != state.SlotPair || r.Agent != state.AgentCodex {
		t.Fatalf("pair resolve = %+v", r)
	}
	if r := reg.Resolve(a.SessionID); r.Status != state.RegCurrent || r.Role != state.SlotLead {
		t.Fatalf("lead no longer current after pair fill: %+v", r)
	}

	// The run advanced to PLAN_DRAFT with the lead's first turn issued.
	rs, _, _ := state.Open(filepath.Join(runDir, "state"), runLock(runDir)).Load()
	if rs.Phase != state.PhasePlanDraft || rs.Assignment == nil || rs.Assignment.ID != res.FirstTurnID {
		t.Fatalf("run state = %+v", rs)
	}
	if rs.Assignment.IssuedRevision != rs.Revision || rs.StartedUnix != 2000 {
		t.Fatalf("first turn / start not bound: %+v", rs.Assignment)
	}
	if rs.DeadlineUnix != 2000+rs.EffectivePolicy.Limits.MaxWallSeconds {
		t.Fatalf("deadline = %d", rs.DeadlineUnix)
	}
}

// The same pair operation retried returns the same pair session; a different
// operation is refused (reattach must present its session id).
func TestJoinAttachIdempotentAndConflict(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	first, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	// Same op retry -> same pair session.
	again, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	if err != nil {
		t.Fatalf("idempotent join: %v", err)
	}
	if again.SessionID != first.SessionID {
		t.Fatalf("idempotent join returned a different session")
	}
	// Different op on a filled pair -> conflict.
	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("c"), 0x20)); !errors.Is(err, ErrPairFilled) {
		t.Fatalf("different-op join err = %v, want ErrPairFilled", err)
	}
}

// A non-complementary agent (claude joining a claude-lead run) is refused.
func TestJoinAttachRejectsSameAgent(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	jr := joinRequest(repo, a.RunID, opID("b"), 0x10)
	jr.Agent = state.AgentClaude // same as the lead
	if _, err := JoinAttach(jr); !errors.Is(err, ErrPairFilled) {
		t.Fatalf("same-agent join err = %v, want ErrPairFilled", err)
	}
}

// Joining a run that is not the active/bootstrapped run is unauthorized.
func TestJoinAttachUnauthorized(t *testing.T) {
	repo := t.TempDir()
	bootstrapRun(t, repo)
	other := "run-" + string(bytes.Repeat([]byte("a"), 32))
	if _, err := JoinAttach(joinRequest(repo, other, opID("b"), 0x10)); !errors.Is(err, ErrJoinUnauthorized) {
		t.Fatalf("unauthorized join err = %v, want ErrJoinUnauthorized", err)
	}
}

// A crash after the registry pair-fill (effect applied, progress not recorded) is
// recovered forward: the pair is filled and the run reaches PLAN_DRAFT.
func TestJoinAttachRecoversMidTransaction(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)

	fired := false
	stepFailpoint = func(s string) error {
		if s == "registry-pair-fill" && !fired {
			fired = true
			return errors.New("injected crash after pair-fill")
		}
		return nil
	}
	defer func() { stepFailpoint = nil }()

	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err == nil {
		t.Fatalf("expected a crash after pair-fill")
	}
	stepFailpoint = nil

	res, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	if err != nil {
		t.Fatalf("recovery join: %v", err)
	}
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	rs, _, _ := state.Open(filepath.Join(runDir, "state"), runLock(runDir)).Load()
	if rs.Phase != state.PhasePlanDraft || rs.Assignment == nil || rs.Assignment.ID != res.FirstTurnID {
		t.Fatalf("run not at PLAN_DRAFT after recovery: %+v", rs)
	}
}
