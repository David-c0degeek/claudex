package gitx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// WorktreeSpec is the frozen identity of a run's provisioned worktree: the repo-relative path
// it lives at, the run branch it checks out, and the exact base commit both are anchored to. It
// carries only primitives so this leaf never imports the attach BootstrapIntent it derives from.
type WorktreeSpec struct {
	RelPath    string // run-relative worktree path (slash form), e.g. ".claudex/runs/<id>/worktree"
	Branch     string // the run branch short name, e.g. "claudex/<id>" (already name-validated)
	BaseCommit string // the frozen base OID the branch and worktree start at
}

// WorktreeState is the idempotent classification of a run's provisioning against the repository.
type WorktreeState int

const (
	// WorktreeAbsent: no branch ref, no registration, no directory — a clean new provision.
	WorktreeAbsent WorktreeState = iota
	// WorktreeApplied: the run branch is at BaseCommit AND a clean worktree is registered at the
	// path with HEAD == BaseCommit on that branch. The exact deliverable.
	WorktreeApplied
	// WorktreeOwnPartial: our own incomplete prefix (ref-only, a stale/broken registration, an
	// unregistered leftover directory at our unique path, or a dirty/partial checkout). Apply
	// completes it forward; it is never a foreign occupant, since the path/branch embed a fresh
	// unpredictable run id.
	WorktreeOwnPartial
	// WorktreeForeign: a ref of our name at a DIFFERENT commit, or a worktree registered at our
	// path to a DIFFERENT branch. Never ours to delete or overwrite — fail closed.
	WorktreeForeign
)

func (s WorktreeState) String() string {
	switch s {
	case WorktreeAbsent:
		return "absent"
	case WorktreeApplied:
		return "applied"
	case WorktreeOwnPartial:
		return "own-partial"
	case WorktreeForeign:
		return "foreign"
	default:
		return "unknown"
	}
}

// Worktree provisions a run's branch + isolated worktree through the hardened git leaf. Ref
// creation (a CAS-create update-ref) is SPLIT from worktree registration (worktree add) so each
// is independently idempotent and every partial cut recovers forward. Mutations run with
// core.fsync=all so git durably persists the refs, index, and worktree metadata it writes.
type Worktree struct{ git *Git }

// NewWorktree wraps a hardened git handle as a worktree provisioner. The handle's lifecycle is
// the caller's; this type never closes it.
func NewWorktree(g *Git) Worktree { return Worktree{git: g} }

func worktreeAbs(repoDir string, spec WorktreeSpec) string {
	return filepath.Join(repoDir, filepath.FromSlash(spec.RelPath))
}

// fsyncArgs prepends the durability config to a MUTATING git command so git fsyncs everything it
// writes (refs, index, loose objects, derived metadata) before it returns.
func fsyncArgs(args ...string) []string {
	return append([]string{"-c", "core.fsync=all", "-c", "core.fsyncMethod=fsync"}, args...)
}

// Observe classifies the provisioning state without mutating anything.
func (w Worktree) Observe(ctx context.Context, repoDir string, spec WorktreeSpec) (WorktreeState, error) {
	abs := worktreeAbs(repoDir, spec)
	oid, err := w.refOID(ctx, repoDir, spec.Branch)
	if err != nil {
		return WorktreeForeign, err
	}
	regs, err := w.listWorktrees(ctx, repoDir)
	if err != nil {
		return WorktreeForeign, err
	}
	reg := findReg(regs, abs)

	// A ref of our name at a different commit, or a worktree at our path on a different branch,
	// is foreign: fail closed and never touch it.
	if (oid != "" && oid != spec.BaseCommit) || (reg != nil && !reg.onBranch(spec.Branch)) {
		return WorktreeForeign, nil
	}

	dirExists := isDir(abs)
	if oid == spec.BaseCommit && reg != nil && reg.onBranch(spec.Branch) && !reg.prunable && dirExists && reg.head == spec.BaseCommit {
		clean, err := w.cleanTree(ctx, abs)
		if err != nil {
			return WorktreeForeign, err
		}
		if clean {
			return WorktreeApplied, nil
		}
		return WorktreeOwnPartial, nil // registered to us but not a clean base checkout
	}
	if oid == "" && reg == nil && !dirExists {
		return WorktreeAbsent, nil
	}
	// Any other mix (ref-only, dir-without-registration, stale/prunable registration) is our own
	// incomplete prefix — nothing foreign survived the check above.
	return WorktreeOwnPartial, nil
}

// Apply provisions forward from Absent or OwnPartial to Applied. It is only invoked by the
// transaction after Observe reported a non-foreign state, and re-checks foreignness defensively.
func (w Worktree) Apply(ctx context.Context, repoDir string, spec WorktreeSpec) error {
	abs := worktreeAbs(repoDir, spec)

	// 1) Ensure the run branch exists at exactly BaseCommit (CAS-create; ours if already at base).
	oid, err := w.refOID(ctx, repoDir, spec.Branch)
	if err != nil {
		return err
	}
	switch oid {
	case "":
		if _, err := w.git.Run(ctx, repoDir, nil, fsyncArgs("update-ref", "refs/heads/"+spec.Branch, spec.BaseCommit, "")...); err != nil {
			return err
		}
	case spec.BaseCommit:
		// already ours, at the frozen base
	default:
		return fmt.Errorf("gitx: run branch %q is at %s, not the frozen base %s", spec.Branch, shortOID(oid), shortOID(spec.BaseCommit))
	}

	// 2) Ensure a clean worktree is registered at the path on that branch.
	regs, err := w.listWorktrees(ctx, repoDir)
	if err != nil {
		return err
	}
	reg := findReg(regs, abs)
	if reg != nil && !reg.onBranch(spec.Branch) {
		return fmt.Errorf("gitx: worktree at %s is registered to %q, not run branch %q", abs, reg.branch, spec.Branch)
	}
	if reg != nil && reg.onBranch(spec.Branch) && !reg.prunable && isDir(abs) && reg.head == spec.BaseCommit {
		if clean, err := w.cleanTree(ctx, abs); err != nil {
			return err
		} else if clean {
			return nil // already fully applied
		}
	}
	// Tear down our own partial registration/leftover (only ours — foreign was rejected above),
	// then add a fresh checkout. Removing a registered worktree also deletes its directory;
	// prune clears a stale admin entry whose directory is gone; a bare leftover directory at our
	// unique path is ours to remove.
	if reg != nil {
		if _, err := w.git.Run(ctx, repoDir, nil, "worktree", "remove", "--force", abs); err != nil {
			if _, perr := w.git.Run(ctx, repoDir, nil, "worktree", "prune"); perr != nil {
				return perr
			}
		}
	}
	if isDir(abs) {
		if err := os.RemoveAll(abs); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	if _, err := w.git.Run(ctx, repoDir, nil, fsyncArgs("worktree", "add", abs, spec.Branch)...); err != nil {
		return err
	}
	return nil
}

// Confirm re-proves the applied identity AND forces the directory entries durable. Observe ==
// Applied proves the branch/registration/HEAD/clean-tree identity; git core.fsync=all already
// persisted the ref/index/metadata during Apply, and on POSIX the containing directory entries
// are additionally fsynced so the worktree survives a crash before the journal records progress.
func (w Worktree) Confirm(ctx context.Context, repoDir string, spec WorktreeSpec) error {
	st, err := w.Observe(ctx, repoDir, spec)
	if err != nil {
		return err
	}
	if st != WorktreeApplied {
		return fmt.Errorf("gitx: worktree is %s, not durably applied", st)
	}
	abs := worktreeAbs(repoDir, spec)
	admin, err := w.adminDir(ctx, abs)
	if err != nil {
		return err
	}
	return syncDirs(abs, filepath.Dir(abs), admin, filepath.Dir(admin))
}

// refOID returns the exact OID of refs/heads/<branch>, or "" if the branch does not exist.
// for-each-ref exits 0 with empty output for a missing ref, so absence is unambiguous.
func (w Worktree) refOID(ctx context.Context, repoDir, branch string) (string, error) {
	out, err := w.git.Run(ctx, repoDir, nil, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", nil
	}
	return parseOID(s)
}

func (w Worktree) cleanTree(ctx context.Context, wtPath string) (bool, error) {
	out, err := w.git.Run(ctx, wtPath, nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "", nil
}

func (w Worktree) adminDir(ctx context.Context, wtPath string) (string, error) {
	out, err := w.git.Run(ctx, wtPath, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// worktreeReg is one parsed record of `git worktree list --porcelain`.
type worktreeReg struct {
	path     string
	head     string
	branch   string // "refs/heads/<name>", empty when detached
	prunable bool
}

func (r worktreeReg) onBranch(short string) bool { return r.branch == "refs/heads/"+short }

func (w Worktree) listWorktrees(ctx context.Context, repoDir string) ([]worktreeReg, error) {
	out, err := w.git.Run(ctx, repoDir, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreePorcelain(string(out)), nil
}

func parseWorktreePorcelain(s string) []worktreeReg {
	var regs []worktreeReg
	var cur *worktreeReg
	flush := func() {
		if cur != nil {
			regs = append(regs, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			cur = &worktreeReg{path: val}
		case "HEAD":
			if cur != nil {
				cur.head = val
			}
		case "branch":
			if cur != nil {
				cur.branch = val
			}
		case "prunable":
			if cur != nil {
				cur.prunable = true
			}
		}
	}
	flush()
	return regs
}

// findReg returns the registration whose path is the same filesystem location as abs, or nil.
func findReg(regs []worktreeReg, abs string) *worktreeReg {
	for i := range regs {
		if samePath(regs[i].path, abs) {
			return &regs[i]
		}
	}
	return nil
}

// samePath compares two filesystem paths for identity. git prints absolute paths with forward
// slashes (even on Windows); it normalizes separators and, on Windows, case, then falls back to
// resolving symlinks (a temp dir may be a symlink into /private on macOS).
func samePath(a, b string) bool {
	if normPath(a) == normPath(b) {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea == nil && eb == nil {
		return normPath(ra) == normPath(rb)
	}
	return false
}

func normPath(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func shortOID(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// syncDirs fsyncs each existing directory so its entries survive a crash. It is a no-op on
// Windows, where directory handles cannot be flushed and git core.fsync governs durability.
func syncDirs(dirs ...string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	for _, d := range dirs {
		f, err := os.Open(d)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		serr := f.Sync()
		cerr := f.Close()
		if serr != nil {
			return serr
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}
