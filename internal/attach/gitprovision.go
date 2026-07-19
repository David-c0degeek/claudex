package attach

import (
	"context"

	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// gitWorktreeProvisioner adapts the primitive gitx.Worktree to the attach WorktreeProvisioner
// seam, translating the frozen BootstrapIntent into a gitx.WorktreeSpec and the four-state
// classification into the transaction's StepStatus. It carries no state beyond the git handle.
type gitWorktreeProvisioner struct{ wt gitx.Worktree }

// NewGitWorktreeProvisioner builds the real worktree provisioner over a hardened git handle. The
// handle's lifecycle (its owned hooks directory) belongs to the caller.
func NewGitWorktreeProvisioner(g *gitx.Git) WorktreeProvisioner {
	return gitWorktreeProvisioner{wt: gitx.NewWorktree(g)}
}

func specOf(in BootstrapIntent) gitx.WorktreeSpec {
	return gitx.WorktreeSpec{RelPath: in.WorktreeRelPath, Branch: in.RunBranch, BaseCommit: in.BaseCommit}
}

// ObserveWorktree maps the provisioning classification onto the idempotent step status: Applied
// is applied; Absent and OwnPartial are both not-applied (Apply completes them forward); Foreign
// is indeterminate, so the transaction fails closed rather than touch a foreign ref/path.
func (p gitWorktreeProvisioner) ObserveWorktree(ctx context.Context, repoDir string, in BootstrapIntent) (txn.StepStatus, error) {
	st, err := p.wt.Observe(ctx, repoDir, specOf(in))
	if err != nil {
		return "", err
	}
	switch st {
	case gitx.WorktreeApplied:
		return txn.StatusApplied, nil
	case gitx.WorktreeAbsent, gitx.WorktreeOwnPartial:
		return txn.StatusNotApplied, nil
	default:
		return txn.StatusIndeterminate, nil
	}
}

func (p gitWorktreeProvisioner) ApplyWorktree(ctx context.Context, repoDir string, in BootstrapIntent) error {
	return p.wt.Apply(ctx, repoDir, specOf(in))
}

func (p gitWorktreeProvisioner) ConfirmWorktree(ctx context.Context, repoDir string, in BootstrapIntent) error {
	return p.wt.Confirm(ctx, repoDir, specOf(in))
}
