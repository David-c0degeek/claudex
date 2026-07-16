package attach

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

const snapshotPerm = 0o600

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
	registry := state.OpenRegistry(filepath.Join(runDir, "registry"), lay.repoLock)
	runState := state.Open(filepath.Join(runDir, "state"), lay.repoLock)
	catalog := state.OpenCatalog(lay.catalogDir, lay.repoLock)
	current := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock)

	steps := []txn.Step{
		snapshotStep("snapshot-task", runDir, in.TaskRelPath, in.TaskCanonical, in.TaskDigest),
		snapshotStep("snapshot-policy", runDir, in.PolicyRelPath, in.PolicyCanonical, in.PolicyDigest),
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
	return txn.Plan{Intent: raw, Steps: steps}, nil
}

// snapshotStep persists an immutable input snapshot with rooted, no-clobber
// publication (an ancestor symlink cannot redirect the write, and an existing
// different target is never overwritten). Observe compares the target under the
// same confined root: absent is NotApplied, exact bytes+digest is Applied, and a
// present-but-different or non-regular file is Indeterminate.
func snapshotStep(name, runDir, rel string, canonical []byte, digest string) txn.Step {
	within := filepath.ToSlash(rel)
	return txn.Step{
		Name: name,
		Status: func() (txn.StepStatus, error) {
			root, err := os.OpenRoot(runDir)
			if err != nil {
				if os.IsNotExist(err) {
					return txn.StatusNotApplied, nil
				}
				return "", err
			}
			defer root.Close()
			got, err := atomicfile.ReadInRoot(root, within, maxSnapshotBytes+1)
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
			if err := os.MkdirAll(runDir, 0o700); err != nil {
				return err
			}
			root, err := os.OpenRoot(runDir)
			if err != nil {
				return err
			}
			defer root.Close()
			if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." && dir != "" {
				if err := atomicfile.MkdirInRoot(root, dir, 0o700); err != nil {
					return err
				}
			}
			return atomicfile.InstallInRoot(root, within, canonical, snapshotPerm)
		},
	}
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

// catalogStep is the discoverability commit: the immutable run ref, allocated
// last of the durable-state steps. A matching ref is Applied; a different ref for
// the same id/dir is Indeterminate (never a second allocation).
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

// currentRunStep sets the authoritative active-run pointer (with the operation id
// for lost-response idempotency) as the final step, so a completed bootstrap is
// discoverable as the current run and its exact identity is returned on retry.
func currentRunStep(store *state.CurrentRunStore, g *genstore.Guard, in BootstrapIntent) txn.Step {
	want := wantCurrentRun(in)
	return txn.Step{
		Name: "current-run-set",
		Status: func() (txn.StepStatus, error) {
			cur, ok, err := store.Load()
			if err != nil {
				return "", err
			}
			if !ok {
				return txn.StatusNotApplied, nil
			}
			if cur == want {
				return txn.StatusApplied, nil
			}
			return txn.StatusIndeterminate, nil
		},
		Apply: func() error {
			_, err := store.MutateLocked(g, 0, func(next *state.CurrentRun) error {
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

func wantCurrentRun(in BootstrapIntent) state.CurrentRun {
	return state.CurrentRun{
		SchemaVersion: state.CurrentRunVersion,
		Revision:      1,
		Active:        true,
		RunID:         in.RunID,
		RelDir:        in.RelDir,
		OperationID:   in.OperationID,
		LeadSessionID: in.SessionID,
		LeadAgent:     in.Agent,
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
