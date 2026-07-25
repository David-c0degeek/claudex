package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/canonjson"
	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// bootstrapRun bootstraps a lead-claude run and returns its id.
func bootstrapRun(t *testing.T, repo string) FirstAttachResult {
	t.Helper()
	a, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
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
		Evidence:    &stubIssuer{},
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
		// PLAN_CRITIQUE carries the candidate plan the accepted draft produced; the
		// first candidate initializes the canonical empty implementation-check set.
		n.CandidatePlan = &state.PlanRef{
			Source:    state.EventRef{Digest: dg, TurnID: pin.FirstTurnID},
			Digest:    strings.Repeat("b", 64),
			StepCount: 1,
		}
		emptyDigest, _ := canonjson.Digest([]byte("[]"))
		n.CandidateChecks = &state.CheckSetRef{Keys: []string{}, Digest: emptyDigest}
		// PLAN_CRITIQUE is read-only, so the reissued assignment carries its own evidence binding;
		// the invariant refuses a state where one moved without the other.
		n.Assignment = &state.Ref{ID: "turn-critique", IssuedRevision: gen}
		n.Evidence = &state.AssignmentEvidence{
			TurnID:          "turn-critique",
			IssuedRevision:  gen,
			ManifestRelPath: evidence.PacketManifestRel("turn-critique"),
			RootDigest:      stubDigest("turn-critique"),
		}
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

// A forged BaseStateDigest is rejected before any effect; Registry stays lead-only.
func TestPairPlanForRejectsForgedBaseDigest(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	reg, _, _ := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load()
	rs, _, _ := state.Open(filepath.Join(runDir, "state"), runLock(runDir)).Load()
	leadDigest, _ := canonDigest(reg.Lead)

	in := PairAttachIntent{
		RunID: a.RunID, TxnID: "pair-forged", OperationID: opID("b"),
		PairSessionID: "sess-" + strings.Repeat("f", 32), PairAgent: state.AgentCodex, FirstTurnID: "turn-forged",
		ExpectedRegistryRevision: reg.Revision, ExpectedStateRevision: rs.Revision,
		StartedUnix: 2000, DeadlineUnix: 2000 + rs.EffectivePolicy.Limits.MaxWallSeconds,
		LeadDigest: leadDigest, BaseStateDigest: strings.Repeat("0", 64), // wrong base digest
	}
	payload, _ := in.marshal()
	g, ok, err := genstore.Acquire(runLock(runDir))
	if err != nil || !ok {
		t.Fatalf("acquire: %v", err)
	}
	if _, perr := pairPlanFor(lay, g, a.RunID, txn.Intent{Version: txn.IntentVersion, Kind: pairIntentKind, TxnID: in.TxnID, ExpectedStateRevision: rs.Revision, Payload: payload}); perr == nil {
		g.Release()
		t.Fatalf("pairPlanFor accepted a forged base digest")
	}
	g.Release()
	reg2, _, _ := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load()
	if reg2.Pair != nil {
		t.Fatalf("registry was mutated by a rejected plan")
	}
}

// A clock preceding the run's creation is refused before Registry fills or a
// journal is written.
func TestJoinAttachRejectsPreCreatedClock(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	jr := joinRequest(repo, a.RunID, opID("b"), 0x10)
	jr.Now = 500 // < CreatedUnix (1000)
	if _, err := JoinAttach(jr); err == nil {
		t.Fatalf("pre-created clock should be rejected")
	}
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	reg, _, _ := state.OpenRegistry(filepath.Join(runDir, "registry"), runLock(runDir)).Load()
	if reg.Pair != nil {
		t.Fatalf("registry pair filled despite the clock rejection")
	}
	if _, ok, _ := txn.Open(lay.attachJournalDir(runDir), runLock(runDir)).Latest(); ok {
		t.Fatalf("an attach journal was written despite the clock rejection")
	}
}

// The durable issuance proof survives a downstream CANCEL: recovery completes
// rather than bricking, even though the assignment was cleared.
func TestJoinAttachRecoversAfterCancel(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))

	fired := false
	stepFailpoint = func(s string) error {
		if s == "state-plan-draft" && !fired {
			fired = true
			return errors.New("crash after plan-draft")
		}
		return nil
	}
	defer func() { stepFailpoint = nil }()
	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err == nil {
		t.Fatalf("expected a crash after plan-draft")
	}
	stepFailpoint = nil

	// A downstream cancel clears the assignment (no accepted turn).
	st := state.Open(filepath.Join(runDir, "state"), runLock(runDir))
	rs, _, _ := st.Load()
	if _, err := st.Mutate(rs.Revision, func(_ uint64, n *state.RunState) error {
		n.Lifecycle = state.LifecycleCancelled
		// A cancel consumes the turn, and the evidence binding has the assignment's lifetime.
		n.Assignment = nil
		n.Evidence = nil
		return nil
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Recovery still completes via the write-once FirstTurn proof.
	res, err := JoinAttach(minimalJoinRequest(repo, a.RunID, opID("b")))
	if err != nil {
		t.Fatalf("recovery after cancel: %v", err)
	}
	if rec, _, _ := txn.Open(lay.attachJournalDir(runDir), runLock(runDir)).Latest(); !rec.Complete {
		t.Fatalf("attach journal not completed after cancel+recovery")
	}
	// The mutable assignment is gone, but the write-once FirstTurn still records it.
	rs2, _, _ := st.Load()
	if rs2.Assignment != nil {
		t.Fatalf("assignment should be cleared by the cancel")
	}
	if rs2.FirstTurn == nil || rs2.FirstTurn.ID != res.FirstTurnID {
		t.Fatalf("first_turn lost after cancel: %+v want %s", rs2.FirstTurn, res.FirstTurnID)
	}
}

// A pair intent with correct timestamps/baseline but a DIFFERENT FirstTurnID than
// the one actually issued is not Applied — the write-once FirstTurn proves which
// turn was issued, so a forged id can't be claimed.
func TestPlanDraftAppliedRejectsForgedFirstTurn(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	res, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(a.RunID))
	rec, _, _ := txn.Open(lay.attachJournalDir(runDir), runLock(runDir)).Latest()
	var pin PairAttachIntent
	if err := json.Unmarshal(rec.Intent.Payload, &pin); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rs, _, _ := state.Open(filepath.Join(runDir, "state"), runLock(runDir)).Load()

	if ok, _ := planDraftApplied(rs, pin); !ok {
		t.Fatalf("the real intent should be applied")
	}
	forged := pin
	forged.FirstTurnID = "turn-forged"
	if ok, _ := planDraftApplied(rs, forged); ok {
		t.Fatalf("a forged FirstTurnID (%s vs issued %s) was accepted as applied", forged.FirstTurnID, res.FirstTurnID)
	}
}

// A pending pair transaction is recovered by a same-op minimal retry, and a
// different minimal operation recovers it forward but is told the pair exists.
func TestJoinAttachPendingRecoveryOps(t *testing.T) {
	t.Run("same op minimal", func(t *testing.T) {
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
		if _, err := JoinAttach(minimalJoinRequest(repo, a.RunID, opID("b"))); err != nil {
			t.Fatalf("same-op minimal recovery: %v", err)
		}
	})
	t.Run("different op minimal", func(t *testing.T) {
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
		// A different op recovers the pending transaction forward but is refused.
		if _, err := JoinAttach(minimalJoinRequest(repo, a.RunID, opID("c"))); !errors.Is(err, ErrPairFilled) {
			t.Fatalf("different-op recovery err = %v, want ErrPairFilled", err)
		}
	})
}
