package reviewpacket

import (
	"context"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
	"github.com/David-c0degeek/claudex/internal/evidence"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/state"
)

// Issuer resolves and publishes a run's review-evidence packets. It is the concrete implementation
// behind the issuance authorities' evidence seams (attach.EvidenceIssuer and the coordinator's
// submit/commit paths), and it is the ONLY place that turns a run into a packet.
//
// It carries a context because the seams it satisfies are invoked from call paths that take no
// context of their own (JoinAttach, ReplaceAttach). The context is bound once, at construction, by
// the caller that owns the operation's lifetime — storing it is deliberate here rather than
// incidental, and it is used only for the Git object reads a resolution performs.
type Issuer struct {
	ctx         context.Context
	deps        Deps
	evidenceDir string
}

// NewIssuer binds an issuer to one run: the repository whose committed objects supply content, the
// run directory holding the frozen inputs, and the run's evidence packet root.
func NewIssuer(ctx context.Context, g *gitx.Git, repoDir, runDir, evidenceDir string) *Issuer {
	return &Issuer{
		ctx:         ctx,
		deps:        Deps{RunDir: runDir, RepoDir: repoDir, Git: g},
		evidenceDir: evidenceDir,
	}
}

// IssueEvidence resolves the recipe for a read-only turn, publishes its packet, and returns the
// binding. Producing is idempotent: re-issuing the same turn re-verifies the already-committed
// packet and returns the same locator, so a recovered transaction re-binds exactly what the original
// authorization published.
func (i *Issuer) IssueEvidence(turnID string, phase state.Phase, rs state.RunState) (string, string, error) {
	r, err := Resolve(i.ctx, i.deps, rs, turnID, phase)
	if err != nil {
		return "", "", err
	}
	return i.publish(r)
}

// IssueEvidenceAt is IssueEvidence against an explicitly stated source object, for the commit
// transaction, whose reviewable commit does not exist in run state yet.
func (i *Issuer) IssueEvidenceAt(turnID string, phase state.Phase, rs state.RunState, src evidence.SourceObject) (string, string, error) {
	r, err := ResolveAt(i.ctx, i.deps, rs, turnID, phase, src)
	if err != nil {
		return "", "", err
	}
	return i.publish(r)
}

func (i *Issuer) publish(r evidence.Recipe) (string, string, error) {
	if err := ensureEvidenceRoot(i.evidenceDir); err != nil {
		return "", "", err
	}
	ref, err := evidence.Produce(i.ctx, i.evidenceDir, r, NewGitObjectReader(i.deps.Git, i.deps.RepoDir))
	if err != nil {
		return "", "", err
	}
	return ref.ManifestRelPath, ref.RootDigest, nil
}

// ensureEvidenceRoot creates the run's packet root durably. evidence.Produce roots itself there via
// os.Root and creates only paths INSIDE it, so the root itself must exist first; it is a per-run
// directory the run guard already serializes, and creating it is idempotent.
func ensureEvidenceRoot(dir string) error {
	return atomicfile.MkdirAllDurable(dir, 0o700)
}
