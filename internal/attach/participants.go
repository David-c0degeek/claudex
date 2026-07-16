package attach

import (
	"bytes"
	"os"
	"path/filepath"

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
// pending one, so recovery re-drives exactly. No step returns identity after a
// mutation: each is an idempotent Observe (Status) / Apply over the frozen intent.
func planFor(lay layout, sm seams, g *genstore.Guard, raw txn.Intent) (txn.Plan, error) {
	in, err := decodeIntent(raw.Payload)
	if err != nil {
		return txn.Plan{}, err
	}
	runDir := lay.runDir(in.RelDir)
	registry := state.OpenRegistry(filepath.Join(runDir, "registry"), lay.repoLock)
	runState := state.Open(filepath.Join(runDir, "state"), lay.repoLock)
	catalog := state.OpenCatalog(lay.catalogDir, lay.repoLock)

	steps := []txn.Step{
		snapshotStep("snapshot-task", filepath.Join(runDir, filepath.FromSlash(in.TaskRelPath)), in.TaskCanonical, in.TaskDigest),
		snapshotStep("snapshot-policy", filepath.Join(runDir, filepath.FromSlash(in.PolicyRelPath)), in.PolicyCanonical, in.PolicyDigest),
		{
			Name:   "worktree",
			Status: func() (txn.StepStatus, error) { return sm.worktree.ObserveWorktree(lay.repoDir, in) },
			Apply:  func() error { return sm.worktree.ApplyWorktree(lay.repoDir, in) },
		},
		registryInitStep(registry, g, in),
		stateInitStep(runState, g, in),
		catalogStep(catalog, g, in),
	}
	return txn.Plan{Intent: raw, Steps: steps}, nil
}

// snapshotStep persists an input snapshot atomically. Observe reads the target
// and compares its digest: absent is NotApplied, exact match is Applied, and a
// present-but-different file is Indeterminate (never silently overwritten).
func snapshotStep(name, target string, canonical []byte, digest string) txn.Step {
	return txn.Step{
		Name: name,
		Status: func() (txn.StepStatus, error) {
			got, err := os.ReadFile(target)
			if err != nil {
				if os.IsNotExist(err) {
					return txn.StatusNotApplied, nil
				}
				return "", err
			}
			if config.Hash(got) == digest && bytes.Equal(got, canonical) {
				return txn.StatusApplied, nil
			}
			return txn.StatusIndeterminate, nil
		},
		Apply: func() error {
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			return atomicfile.Write(target, canonical, snapshotPerm)
		},
	}
}

// registryInitStep fills the lead slot in a fresh registry (generation 1).
func registryInitStep(store *state.RegistryStore, g *genstore.Guard, in BootstrapIntent) txn.Step {
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
			r := reg.Resolve(in.SessionID)
			if r.Status == state.RegCurrent && r.Role == state.SlotLead && r.Agent == in.Agent && reg.RunID == in.RunID {
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

// stateInitStep appends the INIT run state (generation 1). Waiting for the pair
// is simply INIT with no assignment and a lead-only registry — no marker.
func stateInitStep(store *state.Store, g *genstore.Guard, in BootstrapIntent) txn.Step {
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
			if rs.RunID == in.RunID && rs.Phase == state.PhaseInit && rs.TaskSnapshot.Digest == in.TaskDigest && rs.PolicySnapshot.Digest == in.PolicyDigest {
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

// catalogStep is the discoverability commit: the immutable run ref, allocated
// last. A matching ref is Applied; a different ref for the same id/dir is
// Indeterminate (never a second allocation).
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
}
