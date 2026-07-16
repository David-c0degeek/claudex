package attach

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"reflect"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

const snapshotPerm = 0o600

// stepFailpoint is a test-only hook: when set, it is called after a step's real
// Apply succeeds, letting a test inject the "effect applied, progress not
// recorded" crash cut at a named step. It is nil in production.
var stepFailpoint func(step string) error

// seams bundles the git-touching participants subject 04 fills.
type seams struct {
	base     BaseResolver
	worktree WorktreeProvisioner
}

// planFor reconstructs the deterministic, ordered plan from an intent — the same
// steps in the same order whether preparing a new bootstrap or recovering a
// pending one. It first binds the envelope to the payload (kind, expected state
// revision, txn id) so a forged or mismatched journal record cannot drive writes.
// No step returns identity after a mutation: each is an idempotent Observe
// (Status) / Apply over the frozen intent.
func planFor(lay layout, sm seams, g *genstore.Guard, raw txn.Intent) (txn.Plan, error) {
	if raw.Kind != intentKind {
		return txn.Plan{}, fmt.Errorf("attach: intent kind %q is not a bootstrap", raw.Kind)
	}
	if raw.ExpectedStateRevision != 0 {
		return txn.Plan{}, fmt.Errorf("attach: a bootstrap intent must expect state revision 0")
	}
	in, err := decodeIntent(raw.Payload)
	if err != nil {
		return txn.Plan{}, err
	}
	if raw.TxnID != in.TxnID {
		return txn.Plan{}, fmt.Errorf("attach: envelope txn id disagrees with the payload")
	}
	runDir := lay.runDir(in.RelDir)
	registry := state.OpenRegistry(path.Join(runDir, "registry"), lay.repoLock)
	runState := state.Open(path.Join(runDir, "state"), lay.repoLock)
	catalog := state.OpenCatalog(lay.catalogDir, lay.repoLock)
	current := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock)

	steps := []txn.Step{
		snapshotStep("snapshot-task", lay.repoDir, in.RelDir+"/"+in.TaskRelPath, in.TaskCanonical, in.TaskDigest),
		snapshotStep("snapshot-policy", lay.repoDir, in.RelDir+"/"+in.PolicyRelPath, in.PolicyCanonical, in.PolicyDigest),
		{
			Name:   "worktree",
			Status: func() (txn.StepStatus, error) { return sm.worktree.ObserveWorktree(lay.repoDir, in) },
			Apply:  func() error { return sm.worktree.ApplyWorktree(lay.repoDir, in) },
		},
		registryInitStep(registry, g, in),
		stateInitStep(runState, g, in),
		catalogStep(catalog, g, in),
		currentRunStep(current, g, in),
	}
	for i := range steps {
		steps[i] = withFailpoint(steps[i])
	}
	return txn.Plan{Intent: raw, Steps: steps}, nil
}

// withFailpoint wraps a step's Apply so a test can fail AFTER the real durable
// effect, exercising the "effect applied, progress not recorded" recovery cut.
func withFailpoint(s txn.Step) txn.Step {
	apply := s.Apply
	s.Apply = func() error {
		if err := apply(); err != nil {
			return err
		}
		if stepFailpoint != nil {
			return stepFailpoint(s.Name)
		}
		return nil
	}
	return s
}

// snapshotStep persists an immutable input snapshot rooted at the REPOSITORY, so
// an ancestor symlink (.claudex, runs, the run dir) cannot redirect the write
// outside the repo, and an existing different target is never overwritten. Observe
// compares the target under the same confined root: absent is NotApplied, exact
// bytes+digest is Applied, and a present-but-different or non-regular file is
// Indeterminate. repoRel is a slash path relative to the repo root.
func snapshotStep(name, repoDir, repoRel string, canonical []byte, digest string) txn.Step {
	return txn.Step{
		Name: name,
		Status: func() (txn.StepStatus, error) {
			root, err := os.OpenRoot(repoDir)
			if err != nil {
				return "", err
			}
			defer root.Close()
			got, err := atomicfile.ReadInRoot(root, repoRel, maxSnapshotBytes+1)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return txn.StatusNotApplied, nil
				}
				if errors.Is(err, atomicfile.ErrNotRegular) {
					return txn.StatusIndeterminate, nil
				}
				return "", err
			}
			if config.Hash(got) == digest && bytes.Equal(got, canonical) {
				return txn.StatusApplied, nil
			}
			return txn.StatusIndeterminate, nil
		},
		Apply: func() error {
			root, err := os.OpenRoot(repoDir)
			if err != nil {
				return err
			}
			defer root.Close()
			for _, dir := range ancestorDirs(repoRel) {
				if err := atomicfile.MkdirInRoot(root, dir, 0o700); err != nil {
					return err
				}
			}
			return atomicfile.InstallInRoot(root, repoRel, canonical, snapshotPerm)
		},
	}
}

// ancestorDirs lists the directory components of repoRel from the outermost in,
// so each is created rooted (never via a redirectable path-based MkdirAll).
func ancestorDirs(repoRel string) []string {
	dir := path.Dir(repoRel)
	if dir == "." || dir == "" {
		return nil
	}
	parts := splitSlash(dir)
	dirs := make([]string, 0, len(parts))
	for i := 1; i <= len(parts); i++ {
		dirs = append(dirs, path.Join(parts[:i]...))
	}
	return dirs
}

func splitSlash(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	if start < len(p) {
		out = append(out, p[start:])
	}
	return out
}

// registryInitStep fills the lead slot in a fresh registry (generation 1). Observe
// compares the COMPLETE intended generation-1 registry, so a present-but-not-exact
// registry (extra history, a filled pair, a different session) is Indeterminate.
func registryInitStep(store *state.RegistryStore, g *genstore.Guard, in BootstrapIntent) txn.Step {
	want := wantRegistry(in)
	return txn.Step{
		Name: "registry-init",
		Status: func() (txn.StepStatus, error) {
			reg, ok, err := store.Load()
			if err != nil {
				return "", err
			}
			if !ok {
				return txn.StatusNotApplied, nil
			}
			if reflect.DeepEqual(reg, want) {
				return txn.StatusApplied, nil
			}
			return txn.StatusIndeterminate, nil
		},
		Apply: func() error {
			_, err := store.MutateLocked(g, 0, func(gen uint64, next *state.Registry) error {
				next.RunID = in.RunID
				next.Lead = &state.RoleSlot{
					Agent:            in.Agent,
					CurrentSessionID: in.SessionID,
					Sessions:         []state.SessionRecord{{SessionID: in.SessionID, Generation: 1, IssuedRegistryRevision: gen}},
				}
				return nil
			})
			return err
		},
	}
}

func wantRegistry(in BootstrapIntent) state.Registry {
	return state.Registry{
		SchemaVersion: state.RegistryVersion,
		RunID:         in.RunID,
		Revision:      1,
		Lead: &state.RoleSlot{
			Agent:            in.Agent,
			CurrentSessionID: in.SessionID,
			Sessions:         []state.SessionRecord{{SessionID: in.SessionID, Generation: 1, IssuedRegistryRevision: 1}},
		},
	}
}

// stateInitStep appends the INIT run state (generation 1). Observe compares the
// COMPLETE intended generation-1 run state, so anything not exactly the intended
// state is Indeterminate. Waiting for the pair is simply INIT + no assignment +
// a lead-only registry — no marker.
func stateInitStep(store *state.Store, g *genstore.Guard, in BootstrapIntent) txn.Step {
	want := wantRunState(in)
	return txn.Step{
		Name: "state-init",
		Status: func() (txn.StepStatus, error) {
			rs, ok, err := store.Load()
			if err != nil {
				return "", err
			}
			if !ok {
				return txn.StatusNotApplied, nil
			}
			if reflect.DeepEqual(rs, want) {
				return txn.StatusApplied, nil
			}
			return txn.StatusIndeterminate, nil
		},
		Apply: func() error {
			_, err := store.MutateLocked(g, 0, func(_ uint64, next *state.RunState) error {
				initRunState(next, in)
				return nil
			})
			return err
		},
	}
}

func wantRunState(in BootstrapIntent) state.RunState {
	var rs state.RunState
	initRunState(&rs, in)
	rs.SchemaVersion = state.RunStateVersion
	rs.Revision = 1
	rs.AcceptedTurns = map[string]state.AcceptedTurn{}
	rs.Counters.StepFixes = []int{}
	return rs
}

// catalogStep records the immutable run ref in the full allocation history. The
// active-run pointer (next step) is the sole activation authority, so catalog
// presence alone never authorizes an attach. A matching ref is Applied; a
// different ref for the same id is Indeterminate (never a second allocation).
func catalogStep(store *state.CatalogStore, g *genstore.Guard, in BootstrapIntent) txn.Step {
	want := in.runRef()
	return txn.Step{
		Name: "catalog-allocate",
		Status: func() (txn.StepStatus, error) {
			cat, ok, err := store.Load()
			if err != nil {
				return "", err
			}
			if !ok {
				return txn.StatusNotApplied, nil
			}
			ref, found := cat.Lookup(in.RunID)
			if !found {
				return txn.StatusNotApplied, nil
			}
			if ref == want {
				return txn.StatusApplied, nil
			}
			return txn.StatusIndeterminate, nil
		},
		Apply: func() error {
			_, err := store.AllocateLocked(g, in.CatalogExpectedRevision, want)
			return err
		},
	}
}

// currentRunStep sets the authoritative active-run pointer (the SOLE activation
// authority) as the final step: it CASes against the intent's expected pointer
// revision, so a later run activates after a prior one was cleared. Observe is
// semantic — the active run must be exactly this one — so it tolerates the
// generation the CAS actually lands on.
func currentRunStep(store *state.CurrentRunStore, g *genstore.Guard, in BootstrapIntent) txn.Step {
	return txn.Step{
		Name: "current-run-set",
		Status: func() (txn.StepStatus, error) {
			cur, ok, err := store.Load()
			if err != nil {
				return "", err
			}
			if !ok || !cur.Active {
				return txn.StatusNotApplied, nil
			}
			if cur.RunID == in.RunID && cur.OperationID == in.OperationID && cur.RelDir == in.RelDir &&
				cur.LeadSessionID == in.SessionID && cur.LeadAgent == in.Agent {
				return txn.StatusApplied, nil
			}
			return txn.StatusIndeterminate, nil
		},
		Apply: func() error {
			_, err := store.MutateLocked(g, in.CurrentRunExpectedRevision, func(next *state.CurrentRun) error {
				next.Active = true
				next.RunID = in.RunID
				next.RelDir = in.RelDir
				next.OperationID = in.OperationID
				next.LeadSessionID = in.SessionID
				next.LeadAgent = in.Agent
				return nil
			})
			return err
		},
	}
}

// initRunState fills a fresh run state from the intent.
func initRunState(next *state.RunState, in BootstrapIntent) {
	next.RunID = in.RunID
	next.Lifecycle = state.LifecycleRunning
	next.Phase = state.PhaseInit
	next.CreatedUnix = in.CreatedUnix
	next.TaskSnapshot = state.SnapshotRef{RelPath: in.TaskRelPath, Digest: in.TaskDigest}
	next.PolicySnapshot = state.SnapshotRef{RelPath: in.PolicyRelPath, Digest: in.PolicyDigest}
	next.EffectivePolicy = in.EffectivePolicy
	next.FS = state.FSResult{Class: in.FSClass, Reason: in.FSReason, Acknowledged: in.FSAck}
	next.Base = in.Base
	next.BaseCommit = in.BaseCommit
	next.WorktreeRelPath = in.WorktreeRelPath
	next.RunBranch = in.RunBranch
}
