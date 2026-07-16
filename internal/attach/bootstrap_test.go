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
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

func fsResultUnknown() fsclass.Result {
	return fsclass.Result{Class: fsclass.Unknown, Reason: "unclassified test filesystem"}
}

type fakeBase struct{ commit string }

func (f fakeBase) ResolveBase(string, string) (string, error) { return f.commit, nil }

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

func testPolicy() (config.RunPolicy, []byte) {
	p := config.DefaultRunPolicy()
	p.TestGate = config.TestGate{Disabled: true}
	b, _ := json.Marshal(p)
	return p, b
}

func newRequest(t *testing.T, repoDir string, wt WorktreeProvisioner) FirstAttachRequest {
	t.Helper()
	pol, polBytes := testPolicy()
	return FirstAttachRequest{
		RepoDir:         repoDir,
		Agent:           state.AgentClaude,
		TaskCanonical:   []byte(`{"goal":"do the thing"}`),
		PolicyCanonical: polBytes,
		EffectivePolicy: pol,
		CreatedUnix:     1000,
		RNG:             bytes.NewReader(bytes.Repeat([]byte{0x3c, 0x9a, 0x17, 0x42}, 32)), // 128 bytes
		Base:            fakeBase{commit: strings.Repeat("a", 40)},
		Worktree:        wt,
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

	// Snapshots are on disk with the frozen digest.
	if b, err := os.ReadFile(filepath.Join(runDir, "inputs", "task.json")); err != nil || string(b) != `{"goal":"do the thing"}` {
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

// A second first-attach when a run already exists refuses (route to join).
func TestFirstAttachRefusesSecondRun(t *testing.T) {
	repo := t.TempDir()
	if _, err := FirstAttach(newRequest(t, repo, &fakeWorktree{})); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if _, err := FirstAttach(newRequest(t, repo, &fakeWorktree{})); !errors.Is(err, ErrRunExists) {
		t.Fatalf("second attach err = %v, want ErrRunExists", err)
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

// An unknown filesystem without the acknowledge policy is refused before any
// mutation (no run directory, journal, or catalog entry is created).
func TestFirstAttachRefusesUnknownFSByDefault(t *testing.T) {
	// The default policy is UnknownFSRefuse; force the classify result via a
	// non-existent nonsense path is not possible here, so assert the mapping
	// directly through classifyFS (the participant-free decision).
	pol := config.DefaultRunPolicy()
	if _, _, _, err := classifyFS(fsResultUnknown(), pol); !errors.Is(err, ErrUnsupportedFS) {
		t.Fatalf("unknown fs under refuse policy err = %v, want ErrUnsupportedFS", err)
	}
	pol.UnknownFSPolicy = config.UnknownFSAcknowledge
	if class, _, ack, err := classifyFS(fsResultUnknown(), pol); err != nil || class != "unknown" || !ack {
		t.Fatalf("unknown fs under acknowledge = %q ack=%v err=%v", class, ack, err)
	}
}
