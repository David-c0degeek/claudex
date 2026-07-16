package attach

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
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

func minimalJoinRequest(repo, runID, op string) JoinAttachRequest {
	return JoinAttachRequest{RepoDir: repo, RunID: runID, OperationID: op}
}

// A completed pair retry needs only {repo, run, operation}; agent/role/now/rng
// are validated only for a new fill.
func TestJoinAttachMinimalRetry(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	first, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	got, err := JoinAttach(minimalJoinRequest(repo, a.RunID, opID("b")))
	if err != nil {
		t.Fatalf("minimal retry: %v", err)
	}
	if got.SessionID != first.SessionID {
		t.Fatalf("minimal retry returned a different session")
	}
}

// A run that went terminal while still INIT is not joinable (no split-brain).
func TestJoinAttachRefusesCancelledInit(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	st := state.Open(filepath.Join(runDir, "state"), runLock(runDir))
	rs, _, _ := st.Load()
	if _, err := st.Mutate(rs.Revision, func(_ uint64, n *state.RunState) error { n.Lifecycle = state.LifecycleCancelled; return nil }); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); !errors.Is(err, ErrJoinUnauthorized) {
		t.Fatalf("join a cancelled-INIT run err = %v, want ErrJoinUnauthorized", err)
	}
}

// A crash after the plan-draft effect, followed by a downstream Submit accepting
// the first turn, still recovers the pending pair journal (monotonic observation)
// rather than bricking it.
func TestJoinAttachRecoversAfterDownstreamAccept(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))

	fired := false
	stepFailpoint = func(s string) error {
		if s == "state-plan-draft" && !fired {
			fired = true
			return errors.New("crash after plan-draft effect")
		}
		return nil
	}
	defer func() { stepFailpoint = nil }()
	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err == nil {
		t.Fatalf("expected a crash after plan-draft")
	}
	stepFailpoint = nil

	journal := txn.Open(lay.attachJournalDir(runDir), runLock(runDir))
	rec, _, _ := journal.Latest()
	var pin PairAttachIntent
	if err := json.Unmarshal(rec.Intent.Payload, &pin); err != nil {
		t.Fatalf("decode pending pair intent: %v", err)
	}

	// A downstream Submit accepts the first (PLAN_DRAFT) turn, advancing the run.
	st := state.Open(filepath.Join(runDir, "state"), runLock(runDir))
	rs, _, _ := st.Load()
	dg := strings.Repeat("a", 64)
	if _, err := st.Mutate(rs.Revision, func(gen uint64, n *state.RunState) error {
		n.Phase = state.PhasePlanCritique
		n.AcceptedTurns[pin.FirstTurnID] = state.AcceptedTurn{
			ArtifactDigest: dg,
			Receipt:        state.Receipt{TurnID: pin.FirstTurnID, Revision: gen, ArtifactDigest: dg},
			Phase:          state.PhasePlanDraft,
		}
		n.Assignment = &state.Ref{ID: "turn-critique", IssuedRevision: gen}
		return nil
	}); err != nil {
		t.Fatalf("downstream accept: %v", err)
	}

	// Recovery must complete (monotonic Applied via the accepted-turn proof).
	res, err := JoinAttach(minimalJoinRequest(repo, a.RunID, opID("b")))
	if err != nil {
		t.Fatalf("recovery after downstream accept: %v", err)
	}
	if res.FirstTurnID != pin.FirstTurnID {
		t.Fatalf("recovery returned a different first turn")
	}
	if rec2, _, _ := journal.Latest(); !rec2.Complete {
		t.Fatalf("attach journal not completed after recovery")
	}
}

// A forged pair intent whose frozen lead digest does not match the run is refused
// before any effect.
func TestPairPlanForRejectsForgedLeadDigest(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	reg, _, _ := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load()
	rs, _, _ := state.Open(filepath.Join(runDir, "state"), runLock(runDir)).Load()
	baseDigest, _ := canonDigest(rs)

	in := PairAttachIntent{
		RunID: a.RunID, TxnID: "pair-forged", OperationID: opID("b"),
		PairSessionID: "sess-" + strings.Repeat("f", 32), PairAgent: state.AgentCodex, FirstTurnID: "turn-forged",
		ExpectedRegistryRevision: reg.Revision, ExpectedStateRevision: rs.Revision,
		StartedUnix: 2000, DeadlineUnix: 2000 + rs.EffectivePolicy.Limits.MaxWallSeconds,
		LeadDigest: strings.Repeat("0", 64), BaseStateDigest: baseDigest, // wrong lead digest
	}
	payload, _ := in.marshal()

	g, ok, err := genstore.Acquire(runLock(runDir))
	if err != nil || !ok {
		t.Fatalf("acquire: %v", err)
	}
	defer g.Release()
	_, perr := pairPlanFor(lay, g, a.RunID, txn.Intent{
		Version: txn.IntentVersion, Kind: pairIntentKind, TxnID: in.TxnID, ExpectedStateRevision: rs.Revision, Payload: payload,
	})
	if perr == nil {
		t.Fatalf("pairPlanFor accepted a forged lead digest")
	}
}
