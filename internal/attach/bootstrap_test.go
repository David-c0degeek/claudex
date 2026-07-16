package attach

import (
	"bytes"
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

// validIntent builds a coherent intent whose derived layout validates.
func validIntent() BootstrapIntent {
	runID := "run-" + strings.Repeat("a", 32)
	pol, _ := config.ParseRunPolicy(policyBytes())
	return BootstrapIntent{
		RunID: runID, TxnID: "boot-x1", OperationID: "op-x1",
		SessionID: "sess-" + strings.Repeat("b", 32), Agent: state.AgentClaude, CreatedUnix: 1000,
		RelDir: ".claudex/runs/" + runID, TaskRelPath: "inputs/task.json",
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

func (f fakeBase) ResolveBase(string, string) (string, error) { return f.commit, nil }

// countingBase records how many times the base resolver was called.
type countingBase struct {
	commit string
	calls  int
}

func (b *countingBase) ResolveBase(string, string) (string, error) {
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

func (f *fakeWorktree) ObserveWorktree(string, BootstrapIntent) (txn.StepStatus, error) {
	if f.applied {
		return txn.StatusApplied, nil
	}
	return txn.StatusNotApplied, nil
}

func (f *fakeWorktree) ApplyWorktree(string, BootstrapIntent) error {
	f.applyCalls++
	if f.failFirst && f.applyCalls == 1 {
		return errors.New("simulated worktree provisioning failure")
	}
	f.applied = true
	return nil
}

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
		OperationID:     "op-abc123",
		TaskCanonical:   taskBytes(),
		PolicyCanonical: policyBytes(),
		CreatedUnix:     1000,
		RNG:             bytes.NewReader(bytes.Repeat([]byte{0x3c, 0x9a, 0x17, 0x42}, 64)), // 256 bytes
		Base:            fakeBase{commit: strings.Repeat("a", 40)},
		Worktree:        wt,
		Classifier:      supportedFS(),
	}
}

func TestFirstAttachHappyPath(t *testing.T) {
	repo := t.TempDir()
	res, err := FirstAttach(newRequest(t, repo, &fakeWorktree{}))
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
	first, err := FirstAttach(newRequest(t, repo, &fakeWorktree{}))
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}

	// A genuinely different operation refuses.
	other := newRequest(t, repo, &fakeWorktree{})
	other.OperationID = "op-different"
	if _, err := FirstAttach(other); !errors.Is(err, ErrRunExists) {
		t.Fatalf("different-op attach err = %v, want ErrRunExists", err)
	}

	// The same operation id (a retry after a lost response) returns the incumbent
	// run and lead session — never a forced replacement — even though the journal
	// is already terminal.
	retry := newRequest(t, repo, &fakeWorktree{}) // same OperationID "op-abc123"
	got, err := FirstAttach(retry)
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

	if _, err := FirstAttach(req); err == nil {
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
	res, err := FirstAttach(req)
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

	_, err := FirstAttach(req)
	var ufs *UnsupportedFSError
	if !errors.As(err, &ufs) || !errors.Is(err, ErrUnsupportedFS) || ufs.AckRequired {
		t.Fatalf("known-unsupported err = %v, want *UnsupportedFSError (ack not required)", err)
	}
	if base.calls != 0 || wt.applyCalls != 0 {
		t.Fatalf("git seams were called: base=%d worktree=%d", base.calls, wt.applyCalls)
	}
	// No .claudex allocation state was created.
	if _, err := os.Stat(filepath.Join(repo, ".claudex", "catalog")); !os.IsNotExist(err) {
		t.Fatalf("catalog dir created despite refusal: %v", err)
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

	_, err := FirstAttach(newRequest(t, repo, &fakeWorktree{}))
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
	if _, err := FirstAttach(newRequest(t, repo, &fakeWorktree{})); !errors.Is(err, genstore.ErrBusy) {
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
