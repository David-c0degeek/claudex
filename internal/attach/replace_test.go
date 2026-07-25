package attach

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// replaceOp derives a canonical operation id from a seed byte, so distinct-seed
// replacements in a test are distinct operations (not idempotent retries of each other).
func replaceOp(seed byte) string { return "op-" + strings.Repeat(fmt.Sprintf("%02x", seed), 16) }

// replaceReq builds a same-role replacement request with a deterministic RNG and an
// operation id derived from the seed.
func replaceReq(repo, runID string, role state.SlotRole, agent state.Agent, gen uint64, seed byte) ReplaceRequest {
	return ReplaceRequest{
		RepoDir: repo, RunID: runID, Role: role, Agent: agent, ExpectedGeneration: gen,
		OperationID: replaceOp(seed),
		RNG:         bytes.NewReader(bytes.Repeat([]byte{seed, 0x11, 0x22, 0x33}, 32)),
		Evidence:    &stubIssuer{},
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

// The lead slot can be replaced BEFORE pairing (a crashed initiator): the pair
// journal is legitimately Absent and the run is a pristine joinable INIT. JoinAttach
// still succeeds afterward against the replacement lead.
func TestReplaceAttachLeadBeforePairing(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo) // lead only; no JoinAttach yet
	res, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotLead, state.AgentClaude, 1, 0x20))
	if err != nil {
		t.Fatalf("replace lead before pairing: %v", err)
	}
	reg := loadReg(t, repo, a.RunID)
	if r := reg.Resolve(res.SessionID); r.Status != state.RegCurrent || r.Role != state.SlotLead {
		t.Fatalf("replacement lead not current: %+v", r)
	}
	if r := reg.Resolve(a.SessionID); r.Status != state.RegReplaced {
		t.Fatalf("original lead not replaced: %+v", r)
	}
	if reg.Pair != nil {
		t.Fatalf("pair slot should still be empty: %+v", reg.Pair)
	}
	// Pairing still completes and advances to PLAN_DRAFT.
	if _, err := JoinAttach(joinRequest(repo, a.RunID, opID("b"), 0x10)); err != nil {
		t.Fatalf("join after lead replacement: %v", err)
	}
	if rs := loadRunState(t, repo, a.RunID); rs.Phase != state.PhasePlanDraft {
		t.Fatalf("run did not pair after lead replacement: %s", rs.Phase)
	}
}

// Replacing the empty pair slot before pairing is ErrReplaceSlotEmpty.
func TestReplaceAttachEmptyPairSlot(t *testing.T) {
	repo := t.TempDir()
	a := bootstrapRun(t, repo)
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)); !errors.Is(err, ErrReplaceSlotEmpty) {
		t.Fatalf("err = %v, want ErrReplaceSlotEmpty", err)
	}
}

// A pending (mid-transaction) pair journal is recovery-required before a replacement.
func TestReplaceAttachPendingJournal(t *testing.T) {
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
		t.Fatal("expected a crash after pair-fill")
	}
	stepFailpoint = nil

	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotLead, state.AgentClaude, 1, 0x20)); !errors.Is(err, ErrReplaceRecoveryRequired) {
		t.Fatalf("err = %v, want ErrReplaceRecoveryRequired", err)
	}
}

// A terminal pair journal whose durable effects are missing (a deleted RunState or
// Registry) is recovery-required, not a blind trust of the record class.
func TestReplaceAttachTerminalMissingEffects(t *testing.T) {
	lay := func(repo, runID string) RunLocation { return runLocationFor(layoutFor(repo), runID) }

	t.Run("missing run state", func(t *testing.T) {
		repo := t.TempDir()
		a, _ := pairedRun(t, repo)
		if err := os.RemoveAll(lay(repo, a.RunID).StateDir); err != nil {
			t.Fatalf("remove state: %v", err)
		}
		if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)); !errors.Is(err, ErrReplaceRecoveryRequired) {
			t.Fatalf("err = %v, want ErrReplaceRecoveryRequired", err)
		}
	})

	t.Run("missing registry", func(t *testing.T) {
		repo := t.TempDir()
		a, _ := pairedRun(t, repo)
		if err := os.RemoveAll(lay(repo, a.RunID).RegistryDir); err != nil {
			t.Fatalf("remove registry: %v", err)
		}
		if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)); !errors.Is(err, ErrReplaceRecoveryRequired) {
			t.Fatalf("err = %v, want ErrReplaceRecoveryRequired", err)
		}
	})
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
		{"empty operation id", func(r *ReplaceRequest) { r.OperationID = "" }},
		{"non-canonical operation id", func(r *ReplaceRequest) { r.OperationID = "op-not-hex" }},
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

// The optimistic mint retries past a collision with an existing session id, and
// exhausts when every draw collides. Each subtest uses its own run so the mint (which
// now runs under the guard, after the idempotency/generation checks) is actually
// reached.
func TestReplaceAttachCollision(t *testing.T) {
	t.Run("retry", func(t *testing.T) {
		repo := t.TempDir()
		a, _ := pairedRun(t, repo)
		collide, err := hex.DecodeString(a.SessionID[len("sess-"):]) // the lead session's bytes
		if err != nil {
			t.Fatalf("decode session: %v", err)
		}
		fresh := bytes.Repeat([]byte{0xa5}, 16)
		txnBytes := bytes.Repeat([]byte{0xb6}, 16) // the txn id mint that follows the session mint
		req := replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)
		// collide, then fresh (session mint), then the txn-id mint bytes.
		req.RNG = bytes.NewReader(bytes.Join([][]byte{collide, fresh, txnBytes}, nil))
		res, err := ReplaceAttach(req)
		if err != nil {
			t.Fatalf("replace: %v", err)
		}
		if res.SessionID == a.SessionID || res.SessionID != "sess-"+hex.EncodeToString(fresh) {
			t.Fatalf("collision was not retried to the fresh id: %s", res.SessionID)
		}
	})

	t.Run("exhaustion", func(t *testing.T) {
		repo := t.TempDir()
		a, _ := pairedRun(t, repo)
		collide, err := hex.DecodeString(a.SessionID[len("sess-"):])
		if err != nil {
			t.Fatalf("decode session: %v", err)
		}
		req := replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)
		req.RNG = bytes.NewReader(bytes.Repeat(collide, 8)) // every draw collides
		if _, err := ReplaceAttach(req); !errors.Is(err, state.ErrMintExhausted) {
			t.Fatalf("err = %v, want ErrMintExhausted", err)
		}
	})
}

// Concurrent replacements at the same expected generation produce exactly one winner;
// the loser is busy or stale, a retry at the same generation is stale, exactly one
// session is appended, and the untouched slot is byte-identical.
func TestReplaceAttachConcurrent(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	leadBefore := loadReg(t, repo, a.RunID).Lead

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
			continue
		}
		if !errors.Is(errs[i], genstore.ErrBusy) && !errors.Is(errs[i], ErrReplaceStaleGeneration) {
			t.Fatalf("loser err = %v, want ErrBusy or ErrReplaceStaleGeneration", errs[i])
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly one winner, got %d (errs: %v)", wins, errs)
	}
	// A retry at the now-superseded generation is stale.
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x40)); !errors.Is(err, ErrReplaceStaleGeneration) {
		t.Fatalf("retry at generation 1 err = %v, want ErrReplaceStaleGeneration", err)
	}
	reg := loadReg(t, repo, a.RunID)
	if len(reg.Pair.Sessions) != 2 {
		t.Fatalf("want exactly one appended session, pair has %d", len(reg.Pair.Sessions))
	}
	if !reflect.DeepEqual(reg.Lead, leadBefore) {
		t.Fatalf("the untouched lead slot changed: %+v -> %+v", leadBefore, reg.Lead)
	}
}

func ambiguousMutate() registryMutate {
	return func(*state.RegistryStore, *genstore.Guard, uint64, func(uint64, *state.Registry) error) (state.Registry, error) {
		return state.Registry{}, fmt.Errorf("%w: injected", genstore.ErrAmbiguous)
	}
}

// sampleReplaceIntent is a valid frozen replacement intent for a paired run's pair slot.
func sampleReplaceIntent(runID string, regRev uint64) ReplaceIntent {
	return ReplaceIntent{
		RunID: runID, TxnID: "run-" + strings.Repeat("c", 32), OperationID: opID("d"),
		Role: state.SlotPair, Agent: state.AgentCodex,
		SupersededGeneration: 1, NewSessionID: "sess-" + strings.Repeat("7", 32), NewGeneration: 2,
		ExpectedRegistryRevision: regRev,
	}
}

// The frozen intent validator rejects a generation overflow a forged journal could
// otherwise use to accept new_generation == 0.
func TestReplaceIntentValidation(t *testing.T) {
	base := sampleReplaceIntent("run-"+strings.Repeat("a", 32), 3)
	if err := base.validate(); err != nil {
		t.Fatalf("valid intent rejected: %v", err)
	}
	for name, mut := range map[string]func(*ReplaceIntent){
		"max superseded generation":  func(in *ReplaceIntent) { in.SupersededGeneration = ^uint64(0); in.NewGeneration = 0 },
		"new not superseded+1":       func(in *ReplaceIntent) { in.NewGeneration = in.SupersededGeneration + 2 },
		"zero superseded":            func(in *ReplaceIntent) { in.SupersededGeneration = 0; in.NewGeneration = 1 },
		"zero expected registry rev": func(in *ReplaceIntent) { in.ExpectedRegistryRevision = 0 },
		"bad operation id":           func(in *ReplaceIntent) { in.OperationID = "op-nope" },
	} {
		t.Run(name, func(t *testing.T) {
			in := base
			mut(&in)
			if err := in.validate(); err == nil {
				t.Fatalf("%s should be rejected", name)
			}
		})
	}
}

// A present replacement head is bound to the exact envelope/payload/run/steps before it
// is trusted or stepped over.
func TestBindReplaceHead(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 32)
	in := sampleReplaceIntent(runID, 3)
	valid := txn.Record{
		SchemaVersion: txn.RecordVersion, Revision: 1, Intent: in.txnIntent(),
		StepIDs: []string{"registry-replace"}, StepsDone: 1, Complete: true,
	}
	if got, err := bindReplaceHead(valid, runID); err != nil || got.OperationID != in.OperationID {
		t.Fatalf("valid head: got=%+v err=%v", got, err)
	}
	if _, err := bindReplaceHead(valid, "run-"+strings.Repeat("b", 32)); err == nil {
		t.Fatalf("wrong run should be rejected")
	}
	for name, mut := range map[string]func(*txn.Record){
		"wrong kind":         func(r *txn.Record) { r.Intent.Kind = "pair-attach" },
		"wrong version":      func(r *txn.Record) { r.Intent.Version = txn.IntentVersion + 1 },
		"wrong txn id":       func(r *txn.Record) { r.Intent.TxnID = "run-" + strings.Repeat("f", 32) },
		"non-zero state rev": func(r *txn.Record) { r.Intent.ExpectedStateRevision = 5 },
		"forged extra step":  func(r *txn.Record) { r.StepIDs = []string{"registry-replace", "extra"} },
		"forged wrong step":  func(r *txn.Record) { r.StepIDs = []string{"registry-forge"} },
	} {
		t.Run(name, func(t *testing.T) {
			rec := valid
			mut(&rec)
			if _, err := bindReplaceHead(rec, runID); err == nil {
				t.Fatalf("%s should be rejected", name)
			}
		})
	}
}

// The replacement step's participant statuses are the authority recovery depends on:
// NotApplied at the exact pre-state, Applied after the append, Indeterminate on drift.
func TestReplaceStepStatus(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	loc := runLocationFor(layoutFor(repo), a.RunID)
	registry := state.OpenRegistry(loc.RegistryDir, loc.RunLock)
	reg := loadReg(t, repo, a.RunID) // pair at generation 1
	in := sampleReplaceIntent(a.RunID, reg.Revision)

	if st, err := replaceStepStatus(registry, in); err != nil || st != txn.StatusNotApplied {
		t.Fatalf("pre-state status = %q err = %v, want not-applied", st, err)
	}
	// A binding that does not match the actual pre-state generation is Indeterminate.
	drift := in
	drift.SupersededGeneration, drift.NewGeneration = 2, 3
	if st, _ := replaceStepStatus(registry, drift); st != txn.StatusIndeterminate {
		t.Fatalf("drifted binding status = %q, want indeterminate", st)
	}

	// Apply the exact supersession, then it observes Applied.
	g, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	_, merr := registry.MutateLocked(g, reg.Revision, func(nextRev uint64, next *state.Registry) error {
		next.Pair.Sessions = append(next.Pair.Sessions, state.SessionRecord{SessionID: in.NewSessionID, Generation: 2, IssuedRegistryRevision: nextRev})
		next.Pair.CurrentSessionID = in.NewSessionID
		return nil
	})
	_ = g.Release()
	if merr != nil {
		t.Fatalf("apply supersession: %v", merr)
	}
	if st, err := replaceStepStatus(registry, in); err != nil || st != txn.StatusApplied {
		t.Fatalf("post-apply status = %q err = %v, want applied", st, err)
	}
}

// A crash that leaves the Registry append durable but the journal at zero progress is
// recovered by OBSERVING the applied effect: a same-op retry advances/completes without
// a second append or any RNG use.
func TestReplaceAttachRecoversAppliedButUnrecorded(t *testing.T) {
	repo := t.TempDir()
	a, oldPair := pairedRun(t, repo)

	real := defaultReplaceSeams()
	seams := real
	seams.mutate = func(rs *state.RegistryStore, g *genstore.Guard, rev uint64, fn func(uint64, *state.Registry) error) (state.Registry, error) {
		reg, err := real.mutate(rs, g, rev, fn) // the append commits durably
		if err != nil {
			return reg, err
		}
		return reg, fmt.Errorf("%w: injected after a durable apply", genstore.ErrAmbiguous)
	}
	req := replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)
	res1, err1 := replaceAttach(req, seams)
	if !errors.Is(err1, ErrReplaceOutcomeUnknown) {
		t.Fatalf("attempt 1 err = %v, want ErrReplaceOutcomeUnknown", err1)
	}
	// The effect DID land, but the journal is still pending.
	if reg := loadReg(t, repo, a.RunID); reg.Pair.CurrentSessionID != res1.SessionID {
		t.Fatalf("the append was not durable: %+v", reg.Pair)
	}

	// Same-op retry, RNG that errors if minted: recovery observes the applied effect.
	retry := req
	retry.RNG = errReader{}
	res2, err2 := ReplaceAttach(retry)
	if err2 != nil {
		t.Fatalf("retry: %v", err2)
	}
	if res2.SessionID != res1.SessionID || res2.Generation != 2 {
		t.Fatalf("retry did not reconcile: res1=%+v res2=%+v", res1, res2)
	}
	reg := loadReg(t, repo, a.RunID)
	if len(reg.Pair.Sessions) != 2 {
		t.Fatalf("recovery appended a second session: %+v", reg.Pair)
	}
	if r := reg.Resolve(oldPair); r.Status != state.RegReplaced {
		t.Fatalf("old pair not replaced: %+v", r)
	}
}

// A completed replacement head whose durable Registry effect is missing cannot be
// silently stepped over by a different operation.
func TestReplaceAttachTerminalReplaceEffectMissing(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)); err != nil {
		t.Fatalf("op-A: %v", err)
	}
	// The completed replacement's Registry effect vanishes.
	loc := runLocationFor(layoutFor(repo), a.RunID)
	if err := os.RemoveAll(loc.RegistryDir); err != nil {
		t.Fatalf("remove registry: %v", err)
	}
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 2, 0x21)); err == nil {
		t.Fatal("a different op stepped over a completed head with a missing effect")
	}
}

// A committed idempotent retry whose guard release fails returns the authoritative
// result with a post-commit warning, not a plain operation error.
func TestReplaceAttachIdempotentReleaseWarning(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	req := replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)
	first, err := ReplaceAttach(req)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	// Retry the same op with a failing run-guard release.
	seams := defaultReplaceSeams()
	seams.releaseRun = failRelease(errors.New("run-drop-x"))
	retry := req
	retry.RNG = errReader{}
	res, err := replaceAttach(retry, seams)
	if err != nil {
		t.Fatalf("an idempotent committed retry must not be an operation error: %v", err)
	}
	if res.SessionID != first.SessionID {
		t.Fatalf("retry differs: %s vs %s", res.SessionID, first.SessionID)
	}
	var pce *genstore.PostCommitError
	if !errors.As(res.CommitWarning, &pce) {
		t.Fatalf("CommitWarning = %v, want *genstore.PostCommitError", res.CommitWarning)
	}
}

// A same-operation retry after an ambiguous (pending) first attempt recovers the frozen
// transaction and completes to the SAME session, without re-minting.
func TestReplaceAttachIdempotentRecoversPending(t *testing.T) {
	repo := t.TempDir()
	a, oldPair := pairedRun(t, repo)

	amb := defaultReplaceSeams()
	amb.mutate = ambiguousMutate()
	req := replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)
	res1, err1 := replaceAttach(req, amb)
	if !errors.Is(err1, ErrReplaceOutcomeUnknown) {
		t.Fatalf("attempt 1 err = %v, want ErrReplaceOutcomeUnknown", err1)
	}
	// The ambiguous mutate applied nothing: the Registry effect did not land.
	if reg := loadReg(t, repo, a.RunID); len(reg.Pair.Sessions) != 1 {
		t.Fatalf("ambiguous attempt appended a session: %+v", reg.Pair)
	}

	// Retry with the SAME operation id and an RNG that ERRORS if minted — proving the
	// retry recovers the frozen intent and never re-mints.
	retry := req
	retry.RNG = errReader{}
	res2, err2 := ReplaceAttach(retry)
	if err2 != nil {
		t.Fatalf("retry: %v", err2)
	}
	if res2.SessionID != res1.SessionID || res2.Generation != 2 {
		t.Fatalf("retry did not reconcile to the frozen candidate: res1=%+v res2=%+v", res1, res2)
	}
	reg := loadReg(t, repo, a.RunID)
	if r := reg.Resolve(res2.SessionID); r.Status != state.RegCurrent || r.CurrentGeneration != 2 {
		t.Fatalf("recovered session not current: %+v", r)
	}
	if r := reg.Resolve(oldPair); r.Status != state.RegReplaced {
		t.Fatalf("old pair not replaced after recovery: %+v", r)
	}
	if len(reg.Pair.Sessions) != 2 {
		t.Fatalf("recovery double-appended: %+v", reg.Pair)
	}
}

// A same-operation retry after a clean completion returns the same result and does not
// replace a second time.
func TestReplaceAttachIdempotentAfterCompletion(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	req := replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)
	res1, err := ReplaceAttach(req)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	retry := req
	retry.RNG = errReader{} // a retry never re-mints
	res2, err := ReplaceAttach(retry)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res2.SessionID != res1.SessionID || res2.Generation != res1.Generation {
		t.Fatalf("idempotent retry differs: %+v vs %+v", res1, res2)
	}
	if reg := loadReg(t, repo, a.RunID); len(reg.Pair.Sessions) != 2 {
		t.Fatalf("retry double-appended: %+v", reg.Pair)
	}
}

// Reusing an operation id for a different replacement (a different role/agent binding)
// is a conflict, not an idempotent retry.
func TestReplaceAttachOperationConflict(t *testing.T) {
	repo := t.TempDir()
	a, _ := pairedRun(t, repo)
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20)); err != nil {
		t.Fatalf("replace: %v", err)
	}
	// Same seed => same operation id, but now targeting the lead role.
	if _, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotLead, state.AgentClaude, 1, 0x20)); !errors.Is(err, ErrReplaceConflict) {
		t.Fatalf("err = %v, want ErrReplaceConflict", err)
	}
}

// A different operation cannot step around a pending one: it recovers the pending
// replacement first, then applies its own.
func TestReplaceAttachDifferentOpRecoversPending(t *testing.T) {
	repo := t.TempDir()
	a, oldPair := pairedRun(t, repo)

	amb := defaultReplaceSeams()
	amb.mutate = ambiguousMutate()
	resA, err := replaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20), amb)
	if !errors.Is(err, ErrReplaceOutcomeUnknown) {
		t.Fatalf("op-A err = %v, want ErrReplaceOutcomeUnknown", err)
	}

	// op-B (a different operation) at the generation op-A will recover to: it recovers
	// op-A first (its session becomes current at generation 2), then supersedes it.
	resB, err := ReplaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 2, 0x21))
	if err != nil {
		t.Fatalf("op-B: %v", err)
	}
	reg := loadReg(t, repo, a.RunID)
	if r := reg.Resolve(resA.SessionID); r.Status != state.RegReplaced || r.CurrentGeneration != 3 {
		t.Fatalf("op-A's pending session was not recovered then superseded: %+v", r)
	}
	if r := reg.Resolve(resB.SessionID); r.Status != state.RegCurrent || r.CurrentGeneration != 3 {
		t.Fatalf("op-B not current at gen 3: %+v", r)
	}
	if r := reg.Resolve(oldPair); r.Status != state.RegReplaced {
		t.Fatalf("original pair: %+v", r)
	}
	if len(reg.Pair.Sessions) != 3 {
		t.Fatalf("want 3 pair sessions (original + op-A + op-B), got %d", len(reg.Pair.Sessions))
	}
}

// failRelease still releases the real OS lock (so the file is freed and the run can be
// cleaned up) but reports the injected release error.
func failRelease(err error) func(*genstore.Guard) error {
	return func(g *genstore.Guard) error { _ = g.Release(); return err }
}

// The exact-release-once + joined-errors contract holds on rejection and post-commit
// paths, proven with injected per-call release/mutation seams.
func TestReplaceAttachSeams(t *testing.T) {
	t.Run("early reject joins both release errors", func(t *testing.T) {
		repo := t.TempDir()
		a, _ := pairedRun(t, repo)
		repoErr := errors.New("repo-release-x")
		runErr := errors.New("run-release-x")
		seams := defaultReplaceSeams()
		seams.releaseRepo = failRelease(repoErr)
		seams.releaseRun = failRelease(runErr)
		// A stale generation rejects while both guards are held.
		_, err := replaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 99, 0x20), seams)
		if !errors.Is(err, ErrReplaceStaleGeneration) || !errors.Is(err, repoErr) || !errors.Is(err, runErr) {
			t.Fatalf("err did not join the stale error and both release errors: %v", err)
		}
	})

	t.Run("repo release failure aborts before mutation", func(t *testing.T) {
		repo := t.TempDir()
		a, oldPair := pairedRun(t, repo)
		repoErr := errors.New("repo-drop-x")
		seams := defaultReplaceSeams()
		seams.releaseRepo = failRelease(repoErr)
		_, err := replaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20), seams)
		if !errors.Is(err, repoErr) {
			t.Fatalf("err = %v, want the repo release error", err)
		}
		if reg := loadReg(t, repo, a.RunID); len(reg.Pair.Sessions) != 1 || reg.Pair.CurrentSessionID != oldPair {
			t.Fatalf("a mutation happened despite the repo-release abort: %+v", reg.Pair)
		}
	})

	t.Run("post-commit run release failure returns result plus PostCommitError", func(t *testing.T) {
		repo := t.TempDir()
		a, _ := pairedRun(t, repo)
		runErr := errors.New("run-drop-x")
		seams := defaultReplaceSeams()
		seams.releaseRun = failRelease(runErr)
		res, err := replaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20), seams)
		if err != nil {
			t.Fatalf("a committed append must not be an error: %v", err)
		}
		var pce *genstore.PostCommitError
		if !errors.As(res.CommitWarning, &pce) {
			t.Fatalf("CommitWarning = %v, want *genstore.PostCommitError", res.CommitWarning)
		}
		if reg := loadReg(t, repo, a.RunID); reg.Pair.CurrentSessionID != res.SessionID {
			t.Fatalf("the append is not durable: %+v", reg.Pair)
		}
	})

	t.Run("ambiguous mutation retains the candidate as outcome-unknown", func(t *testing.T) {
		repo := t.TempDir()
		a, _ := pairedRun(t, repo)
		seams := defaultReplaceSeams()
		seams.mutate = func(*state.RegistryStore, *genstore.Guard, uint64, func(uint64, *state.Registry) error) (state.Registry, error) {
			return state.Registry{}, fmt.Errorf("%w: injected", genstore.ErrAmbiguous)
		}
		res, err := replaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20), seams)
		if !errors.Is(err, ErrReplaceOutcomeUnknown) || !errors.Is(err, genstore.ErrAmbiguous) {
			t.Fatalf("err = %v, want ErrReplaceOutcomeUnknown wrapping ErrAmbiguous", err)
		}
		if !state.IsSessionID(res.SessionID) || res.Generation != 2 {
			t.Fatalf("the candidate identity was not retained for recovery: %+v", res)
		}
		if res.CommitWarning != nil {
			t.Fatalf("CommitWarning must be reserved for a proven commit: %v", res.CommitWarning)
		}
	})

	t.Run("proven-uncommitted mutation returns a zero result", func(t *testing.T) {
		repo := t.TempDir()
		a, _ := pairedRun(t, repo)
		seams := defaultReplaceSeams()
		seams.mutate = func(*state.RegistryStore, *genstore.Guard, uint64, func(uint64, *state.Registry) error) (state.Registry, error) {
			return state.Registry{}, errors.New("plain proven failure")
		}
		res, err := replaceAttach(replaceReq(repo, a.RunID, state.SlotPair, state.AgentCodex, 1, 0x20), seams)
		if err == nil {
			t.Fatal("a proven-uncommitted mutation must be an error")
		}
		if res.SessionID != "" || res.Generation != 0 {
			t.Fatalf("want a zero result for a proven-uncommitted failure: %+v", res)
		}
	})
}
