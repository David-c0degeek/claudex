package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/fsclass"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/legacy"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// opID builds a valid minted operation id ("op-" + 32 hex of the given digit).
func opID(c string) string { return "op-" + strings.Repeat(c, 32) }

// validIntent builds a coherent intent whose derived layout validates.
func validIntent() BootstrapIntent {
	runID := "run-" + strings.Repeat("a", 32)
	pol, _ := config.ParseRunPolicy(policyBytes())
	return BootstrapIntent{
		RunID: runID, TxnID: "boot-x1", OperationID: opID("c"),
		SessionID: "sess-" + strings.Repeat("b", 32), Agent: state.AgentClaude, CreatedUnix: 1000,
		PairJoinOperationID: opID("d"),
		RelDir:              ".claudex/runs/" + runID, TaskRelPath: "inputs/task.json",
		TaskDigest: config.Hash(taskBytes()), TaskCanonical: taskBytes(),
		PolicyRelPath: "inputs/policy.json", PolicyDigest: config.Hash(policyBytes()), PolicyCanonical: policyBytes(),
		EffectivePolicy: pol, Base: pol.BaseBranch, BaseCommit: strings.Repeat("a", 40),
		WorktreeRelPath: ".claudex/runs/" + runID + "/worktree", RunBranch: "claudex/" + runID,
		FSClass: "supported-local", FSReason: "local fixed drive", FSAck: false,
	}
}

func fsResultUnknown() fsclass.Result {
	return fsclass.Result{Class: fsclass.Unknown, Reason: "unclassified test filesystem"}
}

type fakeBase struct{ commit string }

func (f fakeBase) ResolveBase(context.Context, string, string) (string, error) { return f.commit, nil }

// fakePreflight is a no-op definite-new-run preflighter; err makes it refuse.
type fakePreflight struct{ err error }

func (f fakePreflight) Preflight(context.Context, string) error { return f.err }

// countingBase records how many times the base resolver was called.
type countingBase struct {
	commit string
	calls  int
}

func (b *countingBase) ResolveBase(context.Context, string, string) (string, error) {
	b.calls++
	return b.commit, nil
}

// fakeWorktree is an in-memory idempotent worktree participant. failFirst makes
// the first Apply fail so a mid-bootstrap crash can be simulated.
type fakeWorktree struct {
	applied    bool
	failFirst  bool
	applyCalls int
}

func (f *fakeWorktree) ObserveWorktree(context.Context, string, BootstrapIntent) (txn.StepStatus, error) {
	if f.applied {
		return txn.StatusApplied, nil
	}
	return txn.StatusNotApplied, nil
}

func (f *fakeWorktree) ApplyWorktree(context.Context, string, BootstrapIntent) error {
	f.applyCalls++
	if f.failFirst && f.applyCalls == 1 {
		return errors.New("simulated worktree provisioning failure")
	}
	f.applied = true
	return nil
}

func (f *fakeWorktree) ConfirmWorktree(context.Context, string, BootstrapIntent) error { return nil }

type fakeClassifier struct {
	res fsclass.Result
	err error
}

func (f fakeClassifier) Classify(string) (fsclass.Result, error) { return f.res, f.err }

func supportedFS() fakeClassifier {
	return fakeClassifier{res: fsclass.Result{Class: fsclass.SupportedLocal, Reason: "local fixed drive"}}
}

func policyBytes() []byte {
	p := config.DefaultRunPolicy()
	p.TestGate = config.TestGate{Disabled: true}
	b, _ := json.Marshal(p)
	return b
}

func taskBytes() []byte {
	return []byte(`{"schema_version":1,"goal":"build the attach protocol","current_behavior":"none","desired_behavior":"two terminals converge","scope":"coordinator core","non_goals":[],"constraints":[],"acceptance_criteria":["it works"],"required_tests":[],"relevant_files":[],"open_questions":[]}`)
}

func newRequest(t *testing.T, repoDir string, wt WorktreeProvisioner) FirstAttachRequest {
	t.Helper()
	return FirstAttachRequest{
		RepoDir:         repoDir,
		Agent:           state.AgentClaude,
		OperationID:     opID("a"),
		TaskCanonical:   taskBytes(),
		PolicyCanonical: policyBytes(),
		CreatedUnix:     1000,
		RNG:             bytes.NewReader(bytes.Repeat([]byte{0x3c, 0x9a, 0x17, 0x42}, 64)), // 256 bytes
		Base:            fakeBase{commit: strings.Repeat("a", 40)},
		Preflight:       fakePreflight{},
		Worktree:        wt,
		Classifier:      supportedFS(),
	}
}

func TestFirstAttachHappyPath(t *testing.T) {
	repo := t.TempDir()
	res, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if !state.IsRunID(res.RunID) || !state.IsSessionID(res.SessionID) || res.Role != state.SlotLead || res.Agent != state.AgentClaude {
		t.Fatalf("result identity = %+v", res)
	}
	// The join command names the complementary agent as the pair.
	if got := strings.Join(res.JoinArgv, " "); !strings.Contains(got, "--agent codex") || !strings.Contains(got, "--role pair") {
		t.Fatalf("join argv = %q", got)
	}

	lay := layoutFor(repo)
	runDir := lay.runDir(".claudex/runs/" + res.RunID)

	// Snapshots are on disk with the frozen bytes.
	if b, err := os.ReadFile(filepath.Join(runDir, "inputs", "task.json")); err != nil || !bytes.Equal(b, taskBytes()) {
		t.Fatalf("task snapshot = %q err=%v", b, err)
	}

	// Run state is at INIT with no assignment (waiting for pair), snapshots bound.
	rs, ok, err := state.Open(filepath.Join(runDir, "state"), lay.repoLock).Load()
	if err != nil || !ok {
		t.Fatalf("load state: ok=%v err=%v", ok, err)
	}
	if rs.RunID != res.RunID || rs.Phase != state.PhaseInit || rs.Assignment != nil {
		t.Fatalf("run state = %+v", rs)
	}

	// Registry has the lead slot; the session resolves current.
	reg, ok, err := state.OpenRegistry(filepath.Join(runDir, "registry"), lay.repoLock).Load()
	if err != nil || !ok {
		t.Fatalf("load registry: ok=%v err=%v", ok, err)
	}
	if r := reg.Resolve(res.SessionID); r.Status != state.RegCurrent || r.Role != state.SlotLead || r.Agent != state.AgentClaude {
		t.Fatalf("lead resolve = %+v", r)
	}

	// The run is discoverable in the catalog.
	cat, ok, err := state.OpenCatalog(lay.catalogDir, lay.repoLock).Load()
	if err != nil || !ok {
		t.Fatalf("load catalog: ok=%v err=%v", ok, err)
	}
	if _, found := cat.Lookup(res.RunID); !found {
		t.Fatalf("run not in catalog")
	}
}

// A different first-attach (a new operation) when a run is already active refuses
// with ErrRunExists (route to join). The SAME operation id instead returns the
// incumbent lead session — idempotent across a lost response after completion.
func TestFirstAttachActiveRunSemantics(t *testing.T) {
	repo := t.TempDir()
	first, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}

	// A genuinely different operation refuses.
	other := newRequest(t, repo, &fakeWorktree{})
	other.OperationID = opID("d")
	if _, err := FirstAttach(context.Background(), other); !errors.Is(err, ErrRunExists) {
		t.Fatalf("different-op attach err = %v, want ErrRunExists", err)
	}

	// The same operation id (a retry after a lost response) returns the incumbent
	// run and lead session — never a forced replacement — even though the journal
	// is already terminal.
	retry := newRequest(t, repo, &fakeWorktree{}) // same OperationID opID("a")
	got, err := FirstAttach(context.Background(), retry)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if got.RunID != first.RunID || got.SessionID != first.SessionID {
		t.Fatalf("retry returned different identity: got %s/%s want %s/%s", got.RunID, got.SessionID, first.RunID, first.SessionID)
	}
}

// A bootstrap that crashes mid-flight (worktree apply fails after the snapshots
// commit) is recovered forward on retry, returning the SAME persisted session id
// — so a lost response never forces a fresh run.
func TestFirstAttachRecoversMidBootstrap(t *testing.T) {
	repo := t.TempDir()
	wt := &fakeWorktree{failFirst: true}
	req := newRequest(t, repo, wt)

	if _, err := FirstAttach(context.Background(), req); err == nil {
		t.Fatalf("first attach should fail at the worktree step")
	}

	// The pending journal holds the frozen identity.
	lay := layoutFor(repo)
	rec, ok, err := txn.Open(lay.bootstrapJournal, lay.repoLock).Latest()
	if err != nil || !ok || rec.Terminal() {
		t.Fatalf("expected a pending bootstrap journal: ok=%v terminal=%v err=%v", ok, rec.Terminal(), err)
	}
	var pending BootstrapIntent
	if err := json.Unmarshal(rec.Intent.Payload, &pending); err != nil {
		t.Fatalf("decode pending intent: %v", err)
	}

	// Retry: recovery drives the bootstrap to completion with the same ids.
	res, err := FirstAttach(context.Background(), req)
	if err != nil {
		t.Fatalf("recovery attach: %v", err)
	}
	if res.SessionID != pending.SessionID || res.RunID != pending.RunID {
		t.Fatalf("recovery returned different identity: got %s/%s want %s/%s", res.RunID, res.SessionID, pending.RunID, pending.SessionID)
	}
	if !wt.applied {
		t.Fatalf("worktree was not applied on recovery")
	}
	// The run is now fully bootstrapped and discoverable.
	cat, ok, _ := state.OpenCatalog(lay.catalogDir, lay.repoLock).Load()
	if !ok {
		t.Fatalf("catalog missing after recovery")
	}
	if _, found := cat.Lookup(res.RunID); !found {
		t.Fatalf("run not discoverable after recovery")
	}
}

// A known-unsupported filesystem is refused end-to-end before any mutation: the
// base resolver and worktree provisioner are never called, and no journal,
// catalog, or run directory is created.
func TestFirstAttachRefusesKnownUnsupportedFS(t *testing.T) {
	repo := t.TempDir()
	base := &countingBase{commit: strings.Repeat("a", 40)}
	wt := &fakeWorktree{}
	req := newRequest(t, repo, wt)
	req.Base = base
	req.Classifier = fakeClassifier{res: fsclass.Result{Class: fsclass.KnownUnsupported, Reason: "network share"}}

	_, err := FirstAttach(context.Background(), req)
	var ufs *UnsupportedFSError
	if !errors.As(err, &ufs) || !errors.Is(err, ErrUnsupportedFS) || ufs.AckRequired {
		t.Fatalf("known-unsupported err = %v, want *UnsupportedFSError (ack not required)", err)
	}
	if base.calls != 0 || wt.applyCalls != 0 {
		t.Fatalf("git seams were called: base=%d worktree=%d", base.calls, wt.applyCalls)
	}
	// No .claudex allocation state was created.
	// No mutation at all: .claudex itself was never created.
	if _, err := os.Stat(filepath.Join(repo, ".claudex")); !os.IsNotExist(err) {
		t.Fatalf(".claudex created despite pre-lock filesystem refusal: %v", err)
	}
}

// An invalid task contract (duplicate acceptance criteria) is refused before any
// mutation: the task is parsed first, so the git base/worktree seams are never
// called and .claudex is never created. This pins the ordering the config-level
// uniqueness check relies on rather than inferring it.
func TestFirstAttachRefusesDuplicateCriteria(t *testing.T) {
	repo := t.TempDir()
	base := &countingBase{commit: strings.Repeat("a", 40)}
	wt := &fakeWorktree{}
	req := newRequest(t, repo, wt)
	req.Base = base
	req.TaskCanonical = []byte(`{"schema_version":1,"goal":"g","current_behavior":"c","desired_behavior":"d","scope":"s","non_goals":[],"constraints":[],"acceptance_criteria":["same","same"],"required_tests":[],"relevant_files":[],"open_questions":[]}`)

	if _, err := FirstAttach(context.Background(), req); err == nil {
		t.Fatalf("duplicate acceptance_criteria should be refused")
	}
	if base.calls != 0 || wt.applyCalls != 0 {
		t.Fatalf("git seams were called on an invalid task: base=%d worktree=%d", base.calls, wt.applyCalls)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claudex")); !os.IsNotExist(err) {
		t.Fatalf(".claudex created despite an invalid task contract: %v", err)
	}
}

// The typed decision preserves known-unsupported vs unknown-without-ack.
func TestDecideFSTypedError(t *testing.T) {
	pol := config.DefaultRunPolicy()
	_, _, _, err := decideFS(fsResultUnknown(), pol)
	var ufs *UnsupportedFSError
	if !errors.As(err, &ufs) || !ufs.AckRequired {
		t.Fatalf("unknown-without-ack err = %v, want AckRequired", err)
	}
	pol.UnknownFSPolicy = config.UnknownFSAcknowledge
	if class, _, ack, err := decideFS(fsResultUnknown(), pol); err != nil || class != "unknown" || !ack {
		t.Fatalf("unknown+ack = %q ack=%v err=%v", class, ack, err)
	}
}

// A pre-pivot Python run (repo-level .claudex/current -> a run dir with a legacy
// state.json) is refused before any mutation; the legacy bytes are untouched and
// no allocation state is created.
func TestFirstAttachRefusesLegacyRun(t *testing.T) {
	repo := t.TempDir()
	claudex := filepath.Join(repo, ".claudex")
	legacyRun := filepath.Join(claudex, "runs", "20260714-legacy")
	if err := os.MkdirAll(legacyRun, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	statePath := filepath.Join(legacyRun, "state.json")
	legacyBytes := []byte(`{"lead":"claude","phase":"init","driver":"headless"}`)
	if err := os.WriteFile(statePath, legacyBytes, 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudex, "current"), []byte("20260714-legacy\n"), 0o600); err != nil {
		t.Fatalf("write current pointer: %v", err)
	}

	_, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
	var lre *legacy.LegacyRunError
	if !errors.As(err, &lre) {
		t.Fatalf("legacy attach err = %v, want *legacy.LegacyRunError", err)
	}
	if _, e := os.Stat(filepath.Join(claudex, "catalog")); !os.IsNotExist(e) {
		t.Fatalf("allocation state created despite legacy refusal: %v", e)
	}
	if b, _ := os.ReadFile(statePath); !bytes.Equal(b, legacyBytes) {
		t.Fatalf("legacy state bytes changed")
	}
}

// The repo allocation lock gates bootstrap: with the lock held, FirstAttach is busy.
func TestFirstAttachBusyWhenLocked(t *testing.T) {
	repo := t.TempDir()
	lay := layoutFor(repo)
	if err := os.MkdirAll(filepath.Dir(lay.repoLock), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	g, ok, err := genstore.Acquire(lay.repoLock)
	if err != nil || !ok {
		t.Fatalf("acquire: %v", err)
	}
	defer g.Release()
	if _, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{})); !errors.Is(err, genstore.ErrBusy) {
		t.Fatalf("locked attach err = %v, want ErrBusy", err)
	}
}

// A forged or tampered intent whose derived layout, fs coherence, or policy
// binding is wrong is rejected, so it can never drive writes outside the run dir.
func TestForgedIntentRejected(t *testing.T) {
	if err := validIntent().validate(); err != nil {
		t.Fatalf("the baseline intent should validate: %v", err)
	}
	tampers := map[string]func(*BootstrapIntent){
		"rel dir elsewhere":    func(in *BootstrapIntent) { in.RelDir = ".claudex/runs/other" },
		"worktree escape":      func(in *BootstrapIntent) { in.WorktreeRelPath = "../escape" },
		"snapshot path":        func(in *BootstrapIntent) { in.TaskRelPath = "inputs/evil.json" },
		"run branch":           func(in *BootstrapIntent) { in.RunBranch = "attacker/branch" },
		"fs incoherent ack":    func(in *BootstrapIntent) { in.FSAck = true },
		"policy mismatch":      func(in *BootstrapIntent) { in.EffectivePolicy.BaseBranch = "evil" },
		"base not oid":         func(in *BootstrapIntent) { in.BaseCommit = "not-a-commit" },
		"task digest mismatch": func(in *BootstrapIntent) { in.TaskDigest = strings.Repeat("0", 64) },
		"non minted session":   func(in *BootstrapIntent) { in.SessionID = "sess-UPPER" },
	}
	for name, tamper := range tampers {
		t.Run(name, func(t *testing.T) {
			in := validIntent()
			tamper(&in)
			if err := in.validate(); err == nil {
				t.Fatalf("%s should be rejected", name)
			}
		})
	}
}

// A conflicting pre-existing snapshot (different bytes at the immutable target) is
// Indeterminate, so recovery fails closed rather than overwriting it.
func TestSnapshotConflictIsIndeterminate(t *testing.T) {
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runDir, "inputs"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "inputs", "task.json"), []byte("different prior bytes"), 0o600); err != nil {
		t.Fatalf("pre-write: %v", err)
	}
	step := snapshotStep("snapshot-task", runDir, "inputs/task.json", taskBytes(), config.Hash(taskBytes()))
	st, err := step.Status()
	if err != nil || st != txn.StatusIndeterminate {
		t.Fatalf("conflicting snapshot status = %q err=%v, want indeterminate", st, err)
	}
}

// A different operation that arrives while a bootstrap is pending completes the
// recovery but is told the run exists — it never receives the incumbent session.
func TestFirstAttachDifferentOpDoesNotGetSession(t *testing.T) {
	repo := t.TempDir()
	if _, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{failFirst: true})); err == nil {
		t.Fatalf("first attach should fail at the worktree step")
	}
	other := newRequest(t, repo, &fakeWorktree{}) // healthy worktree drives recovery
	other.OperationID = opID("d")
	if _, err := FirstAttach(context.Background(), other); !errors.Is(err, ErrRunExists) {
		t.Fatalf("different-op pending recovery err = %v, want ErrRunExists", err)
	}
	// Recovery still completed: an active run now exists.
	lay := layoutFor(repo)
	cur, ok, _ := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if !ok || !cur.Active {
		t.Fatalf("recovery did not complete the run")
	}
}

// A run that has actually reached terminal is reconciled and a second run
// bootstrapped; a still-running run is never displaced. Both catalog refs are
// retained and the second run is current.
func TestFirstAttachSecondRunAfterClear(t *testing.T) {
	repo := t.TempDir()
	a, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
	if err != nil {
		t.Fatalf("run A: %v", err)
	}
	lay := layoutFor(repo)
	// Drive A terminal through a PER-RUN lock (the two-tier protocol: after
	// bootstrap, run-scoped work uses the run's own lock, not the repo lock).
	aRunDir := lay.runDir(state.RunDirRelFor(a.RunID))
	aStore := state.Open(filepath.Join(aRunDir, "state"), filepath.Join(aRunDir, "run.lock"))

	// While A is still RUNNING, a different operation must be refused (no split-brain).
	running := newRequest(t, repo, &fakeWorktree{})
	running.OperationID = opID("e")
	running.RNG = bytes.NewReader(bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 64))
	if _, err := FirstAttach(context.Background(), running); !errors.Is(err, ErrRunExists) {
		t.Fatalf("attach over a running run err = %v, want ErrRunExists", err)
	}

	// Drive A to a terminal lifecycle (as a real completion/cancellation would).
	rsA, _, _ := aStore.Load()
	if _, err := aStore.Mutate(rsA.Revision, func(_ uint64, n *state.RunState) error { n.Lifecycle = state.LifecycleCancelled; return nil }); err != nil {
		t.Fatalf("terminate A: %v", err)
	}

	// Now a different operation reconciles the terminal A and bootstraps B.
	reqB := newRequest(t, repo, &fakeWorktree{})
	reqB.OperationID = opID("b")
	reqB.RNG = bytes.NewReader(bytes.Repeat([]byte{0x77, 0x11, 0x88, 0x22}, 64)) // distinct ids from run A
	b, err := FirstAttach(context.Background(), reqB)
	if err != nil {
		t.Fatalf("run B: %v", err)
	}
	if b.RunID == a.RunID {
		t.Fatalf("run B reused run A's id")
	}
	// Both catalog refs retained.
	cat, _, _ := state.OpenCatalog(lay.catalogDir, lay.repoLock).Load()
	if _, okA := cat.Lookup(a.RunID); !okA {
		t.Fatalf("run A dropped from catalog")
	}
	if _, okB := cat.Lookup(b.RunID); !okB {
		t.Fatalf("run B missing from catalog")
	}
	// B is current.
	cur, _, _ := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if !cur.Active || cur.RunID != b.RunID {
		t.Fatalf("current run = %+v, want B active", cur)
	}
}

// A crash after each durable step (effect applied, progress not recorded) is
// recovered forward on retry to a fully bootstrapped run.
func TestBootstrapCutRecovers(t *testing.T) {
	for _, step := range []string{"snapshot-task", "snapshot-policy", "worktree", "registry-init", "state-init", "catalog-allocate", "current-run-set"} {
		t.Run(step, func(t *testing.T) {
			repo := t.TempDir()
			fired := false
			stepFailpoint = func(s string) error {
				if s == step && !fired {
					fired = true
					return errors.New("injected crash after " + s)
				}
				return nil
			}
			defer func() { stepFailpoint = nil }()

			// One worktree instance across both calls, so it reports its provisioned
			// state on recovery (a real provisioner observes the on-disk worktree).
			wt := &fakeWorktree{}
			if _, err := FirstAttach(context.Background(), newRequest(t, repo, wt)); err == nil {
				t.Fatalf("expected a crash after %s", step)
			}
			stepFailpoint = nil // healthy retry
			got, err := FirstAttach(context.Background(), newRequest(t, repo, wt))
			if err != nil {
				t.Fatalf("recovery after %s cut: %v", step, err)
			}
			lay := layoutFor(repo)
			cur, ok, _ := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
			if !ok || !cur.Active || cur.RunID != got.RunID {
				t.Fatalf("run not fully bootstrapped after recovering the %s cut", step)
			}
		})
	}
}

// Concurrent first attaches serialize on the repo lock: exactly one bootstraps a
// run, and the losing operation, retried, CONVERGES to ErrRunExists — only one
// run is ever allocated.
func TestFirstAttachConcurrent(t *testing.T) {
	repo := t.TempDir()
	ops := []string{opID("a"), opID("b")}
	seeds := [][]byte{{0x11, 0x22, 0x33, 0x44}, {0x55, 0x66, 0x77, 0x88}}
	type outcome struct {
		op  string
		err error
	}
	results := make(chan outcome, 2)
	for i, op := range ops {
		i, op := i, op
		go func() {
			req := newRequest(t, repo, &fakeWorktree{})
			req.OperationID = op
			req.RNG = bytes.NewReader(bytes.Repeat(seeds[i], 64))
			_, err := FirstAttach(context.Background(), req)
			results <- outcome{op, err}
		}()
	}
	got := []outcome{<-results, <-results}
	successes, loser := 0, ""
	for _, o := range got {
		switch {
		case o.err == nil:
			successes++
		case errors.Is(o.err, ErrRunExists) || errors.Is(o.err, genstore.ErrBusy):
			loser = o.op
		default:
			t.Fatalf("unexpected concurrent error: %v", o.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent successes = %d, want exactly 1", successes)
	}
	// The loser retried converges to ErrRunExists (never a second run).
	retry := newRequest(t, repo, &fakeWorktree{})
	retry.OperationID = loser
	retry.RNG = bytes.NewReader(bytes.Repeat([]byte{0x9a, 0xbc, 0xde, 0xf0}, 64))
	if _, err := FirstAttach(context.Background(), retry); !errors.Is(err, ErrRunExists) {
		t.Fatalf("loser retry err = %v, want ErrRunExists", err)
	}
	lay := layoutFor(repo)
	cat, _, _ := state.OpenCatalog(lay.catalogDir, lay.repoLock).Load()
	if len(cat.Runs) != 1 {
		t.Fatalf("catalog has %d runs, want exactly 1", len(cat.Runs))
	}
}

// minimalRequest carries only what a recovery/idempotent return needs: repo,
// operation id, and a worktree observer — no task/policy/RNG/base/agent/clock.
func minimalRequest(repo, op string, wt WorktreeProvisioner) FirstAttachRequest {
	return FirstAttachRequest{RepoDir: repo, OperationID: op, Worktree: wt}
}

// A definite-new-run preflight refusal (a dirty or runtime-not-ignored repo) fails closed
// BEFORE any mutation: no .claudex directory is created.
func TestFirstAttachPreflightRefusalNoMutation(t *testing.T) {
	repo := t.TempDir()
	req := newRequest(t, repo, &fakeWorktree{})
	req.Preflight = fakePreflight{err: errors.New("dirty working tree")}
	if _, err := FirstAttach(context.Background(), req); err == nil {
		t.Fatal("a preflight refusal should fail the attach")
	}
	if _, e := os.Stat(filepath.Join(repo, ".claudex")); !os.IsNotExist(e) {
		t.Fatalf(".claudex created despite a preflight refusal")
	}
}

// A classifier I/O error is propagated (fail-closed) BEFORE any mutation — never
// fabricated into a positive known-unsupported classification.
func TestFirstAttachClassifierError(t *testing.T) {
	repo := t.TempDir()
	req := newRequest(t, repo, &fakeWorktree{})
	req.Classifier = fakeClassifier{err: errors.New("classify io failure")}
	_, err := FirstAttach(context.Background(), req)
	if err == nil || errors.Is(err, ErrUnsupportedFS) {
		t.Fatalf("classifier error = %v, want the propagated cause (not ErrUnsupportedFS)", err)
	}
	if _, e := os.Stat(filepath.Join(repo, ".claudex")); !os.IsNotExist(e) {
		t.Fatalf(".claudex created despite a classifier error")
	}
}

// A recovery and a completed-response retry both work from ONLY {repo, operation
// id, worktree}, even when the fresh inputs are gone/nil.
func TestFirstAttachMinimalRecoveryInputs(t *testing.T) {
	t.Run("pending recovery", func(t *testing.T) {
		repo := t.TempDir()
		if _, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{failFirst: true})); err == nil {
			t.Fatalf("first attach should fail at the worktree step")
		}
		// Retry with NOTHING but repo + op + a healthy worktree.
		got, err := FirstAttach(context.Background(), minimalRequest(repo, opID("a"), &fakeWorktree{}))
		if err != nil {
			t.Fatalf("minimal pending recovery: %v", err)
		}
		if !state.IsSessionID(got.SessionID) {
			t.Fatalf("recovery returned no session")
		}
	})
	t.Run("completed response retry", func(t *testing.T) {
		repo := t.TempDir()
		a, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
		if err != nil {
			t.Fatalf("first attach: %v", err)
		}
		got, err := FirstAttach(context.Background(), minimalRequest(repo, opID("a"), &fakeWorktree{}))
		if err != nil {
			t.Fatalf("minimal completed retry: %v", err)
		}
		if got.RunID != a.RunID || got.SessionID != a.SessionID {
			t.Fatalf("minimal retry identity = %s/%s, want %s/%s", got.RunID, got.SessionID, a.RunID, a.SessionID)
		}
	})
}

// A same-op retry after the lead session was replaced in the Registry returns
// run-exists, never the stale credential — the Registry is the sole session truth.
func TestSameOpRefusesReplacedSession(t *testing.T) {
	repo := t.TempDir()
	a, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	lay := layoutFor(repo)
	// A same-role replacement happens through the run's own lock, not the repo lock.
	aRunDir := lay.runDir(state.RunDirRelFor(a.RunID))
	regStore := state.OpenRegistry(filepath.Join(aRunDir, "registry"), filepath.Join(aRunDir, "run.lock"))
	reg, _, _ := regStore.Load()
	newSess := "sess-" + strings.Repeat("f", 32)
	if _, err := regStore.Mutate(reg.Revision, func(gen uint64, next *state.Registry) error {
		next.Lead.Sessions = append(next.Lead.Sessions, state.SessionRecord{SessionID: newSess, Generation: 2, IssuedRegistryRevision: gen})
		next.Lead.CurrentSessionID = newSess
		return nil
	}); err != nil {
		t.Fatalf("replace lead session: %v", err)
	}
	if _, err := FirstAttach(context.Background(), minimalRequest(repo, opID("a"), &fakeWorktree{})); !errors.Is(err, ErrRunExists) {
		t.Fatalf("retry after replacement err = %v, want ErrRunExists (never the stale session)", err)
	}
}

// A crash after current-clear or B's current-run-set recovers forward: A and B
// both stay in catalog history and B ends exactly current.
func TestSecondRunCutRecovers(t *testing.T) {
	for _, step := range []string{"current-clear", "current-run-set"} {
		t.Run(step, func(t *testing.T) {
			repo := t.TempDir()
			a, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
			if err != nil {
				t.Fatalf("run A: %v", err)
			}
			lay := layoutFor(repo)
			aRunDir := lay.runDir(state.RunDirRelFor(a.RunID))
			aStore := state.Open(filepath.Join(aRunDir, "state"), filepath.Join(aRunDir, "run.lock"))
			rsA, _, _ := aStore.Load()
			if _, err := aStore.Mutate(rsA.Revision, func(_ uint64, n *state.RunState) error { n.Lifecycle = state.LifecycleCancelled; return nil }); err != nil {
				t.Fatalf("terminate A: %v", err)
			}

			fired := false
			stepFailpoint = func(s string) error {
				if s == step && !fired {
					fired = true
					return errors.New("injected crash after " + s)
				}
				return nil
			}
			defer func() { stepFailpoint = nil }()

			wtB := &fakeWorktree{}
			reqB := newRequest(t, repo, wtB)
			reqB.OperationID = opID("b")
			reqB.RNG = bytes.NewReader(bytes.Repeat([]byte{0x77, 0x11, 0x88, 0x22}, 64))
			if _, err := FirstAttach(context.Background(), reqB); err == nil {
				t.Fatalf("expected a crash after %s", step)
			}
			stepFailpoint = nil

			gotB, err := FirstAttach(context.Background(), minimalRequest(repo, opID("b"), wtB))
			if err != nil {
				t.Fatalf("recover B after %s cut: %v", step, err)
			}
			cat, _, _ := state.OpenCatalog(lay.catalogDir, lay.repoLock).Load()
			if _, okA := cat.Lookup(a.RunID); !okA {
				t.Fatalf("run A dropped from catalog")
			}
			if _, okB := cat.Lookup(gotB.RunID); !okB {
				t.Fatalf("run B missing from catalog")
			}
			cur, ok, _ := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
			if !ok || !cur.Active || cur.RunID != gotB.RunID {
				t.Fatalf("B is not exactly current after the %s cut", step)
			}
		})
	}
}

// The combined same-op read fails closed on any journal<->pointer<->registry
// mismatch (hand-built pointer) and a missing registry.
func TestSameOpResultRejectsMismatch(t *testing.T) {
	repo := t.TempDir()
	FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{}))
	lay := layoutFor(repo)
	journal := txn.Open(lay.bootstrapJournal, lay.repoLock)
	good, _, _ := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load()
	if _, err := sameOpResult(lay, journal, good); err != nil {
		t.Fatalf("baseline combined read: %v", err)
	}
	other := "run-" + strings.Repeat("e", 32)
	mismatches := map[string]state.CurrentRun{
		"run id":    {SchemaVersion: 1, Revision: good.Revision, Active: true, RunID: other, RelDir: state.RunDirRelFor(other), OperationID: good.OperationID},
		"rel dir":   {SchemaVersion: 1, Revision: good.Revision, Active: true, RunID: good.RunID, RelDir: ".claudex/runs/other", OperationID: good.OperationID},
		"operation": {SchemaVersion: 1, Revision: good.Revision, Active: true, RunID: good.RunID, RelDir: good.RelDir, OperationID: opID("f")},
	}
	for name, bad := range mismatches {
		if _, err := sameOpResult(lay, journal, bad); !errors.Is(err, ErrRunExists) {
			t.Fatalf("%s mismatch err = %v, want ErrRunExists", name, err)
		}
	}
	// A missing registry for the journal's run also fails closed.
	if err := os.RemoveAll(filepath.Join(lay.runDir(good.RelDir), "registry")); err != nil {
		t.Fatalf("remove registry: %v", err)
	}
	if _, err := sameOpResult(lay, journal, good); !errors.Is(err, ErrRunExists) {
		t.Fatalf("missing registry err = %v, want ErrRunExists", err)
	}
}

// planFor refuses a forged clear-prior that names a still-running run, so a
// tampered journal can never evict a live run.
func TestPlanForRejectsNonterminalClearPrior(t *testing.T) {
	repo := t.TempDir()
	a, err := FirstAttach(context.Background(), newRequest(t, repo, &fakeWorktree{})) // A is RUNNING
	if err != nil {
		t.Fatalf("run A: %v", err)
	}
	lay := layoutFor(repo)
	in := validIntent()
	in.ClearPriorRunID = a.RunID
	in.ClearPriorRevision = 1
	payload, _ := in.marshal()

	g, ok, err := genstore.Acquire(lay.repoLock)
	if err != nil || !ok {
		t.Fatalf("acquire: %v", err)
	}
	defer g.Release()
	_, perr := planFor(context.Background(), lay, seams{base: fakeBase{commit: strings.Repeat("a", 40)}, worktree: &fakeWorktree{}}, g,
		txn.Intent{Version: txn.IntentVersion, Kind: intentKind, TxnID: in.TxnID, Payload: payload})
	if perr == nil {
		t.Fatalf("planFor accepted a clear-prior naming a running run")
	}
}

// current-clear only clears the EXACT frozen prior revision; a same-id newer
// activation is Indeterminate, never clearable.
func TestCurrentClearStepFrozenRevision(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "run.lock")
	store := state.OpenCurrentRun(filepath.Join(dir, "active-run"), lock)
	g, ok, err := genstore.Acquire(lock)
	if err != nil || !ok {
		t.Fatalf("acquire: %v", err)
	}
	defer g.Release()
	if _, err := store.Activate(g, 0, "run-a", state.RunDirRelFor("run-a"), opID("a")); err != nil {
		t.Fatalf("activate: %v", err)
	}
	in := BootstrapIntent{
		RunID: "run-b", RelDir: state.RunDirRelFor("run-b"), OperationID: opID("b"),
		ClearPriorRunID: "run-a", ClearPriorRevision: 99, // != the pointer's actual revision (1)
	}
	st, err := currentClearStep(store, g, in).Status()
	if err != nil || st != txn.StatusIndeterminate {
		t.Fatalf("frozen-revision clear status = %q err=%v, want indeterminate", st, err)
	}
}
