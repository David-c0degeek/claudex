package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/David-c0degeek/claudex/internal/atomicfile"
)

// WorktreeSpec is the frozen identity of a run's provisioned worktree: the repo-relative path
// it lives at, the run branch it checks out, and the exact base commit both are anchored to. It
// carries only primitives so this leaf never imports the attach BootstrapIntent it derives from.
type WorktreeSpec struct {
	RelPath    string // run-relative worktree path (slash form), e.g. ".claudex/runs/<id>/worktree"
	Branch     string // the run branch short name, e.g. "claudex/<id>" (already name-validated)
	BaseCommit string // the frozen base OID the branch and worktree start at
	// OwnerToken is an unpredictable, operation-bound token (the frozen txn id). Written to a
	// durable no-clobber marker BEFORE `git worktree add`, it proves that a bare, unregistered
	// directory left by an interrupted `worktree add` (git creates the target directory before it
	// writes the linkage files) is OUR own crashed prefix — safe to clear and retry — rather than a
	// foreign occupant, which never carries our token and always fails closed.
	OwnerToken string
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

// ownerMarkerName is the durable ownership certificate placed BESIDE the worktree directory (in
// the run directory), so it survives an interrupted `git worktree add` that leaves a bare target.
const ownerMarkerName = ".claudex-worktree-owner"

// maxOwnerMarker bounds the marker read; the token is a short id.
const maxOwnerMarker = 256

// ownerMarkerRel is the marker's slash path relative to the repository root (a sibling of the
// worktree directory, in the run directory).
func ownerMarkerRel(spec WorktreeSpec) string {
	return path.Dir(spec.RelPath) + "/" + ownerMarkerName
}

// ownerState is whether the ownership marker proves the target prefix is ours.
type ownerState int

const (
	ownerAbsent  ownerState = iota // no marker: we have not started provisioning here
	ownerOurs                      // marker carries our exact token: our own interrupted prefix
	ownerForeign                   // marker missing our bytes, or a non-regular node: never ours
)

// classifyOwner reads the ownership marker through the confined root (refusing a symlink or other
// non-regular node) and matches its EXACT bytes against the operation token.
func classifyOwner(repoDir string, spec WorktreeSpec) (ownerState, error) {
	root, err := os.OpenRoot(repoDir)
	if err != nil {
		return ownerAbsent, err
	}
	defer root.Close()
	got, err := atomicfile.ReadInRoot(root, ownerMarkerRel(spec), maxOwnerMarker)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ownerAbsent, nil
		}
		if errors.Is(err, atomicfile.ErrNotRegular) {
			return ownerForeign, nil
		}
		return ownerAbsent, err
	}
	if spec.OwnerToken != "" && string(got) == spec.OwnerToken {
		return ownerOurs, nil
	}
	return ownerForeign, nil
}

// ensureOwnerMarker publishes our ownership marker BEFORE `git worktree add`, and RE-CONFIRMS its
// durability on every call. Publication is complete-or-absent and no-clobber (InstallInRoot writes
// and fsyncs a temp, then durably installs it), so a halt mid-publish never leaves a truncated
// marker that recovery would read as foreign. On a retry the existing marker's exact bytes are
// verified and its content + directory entry are re-forced durable (SyncInRoot + ConfirmParentInRoot),
// so a prior halt after the content write but before the entry barrier is repaired rather than
// trusted. The certificate is therefore safe under its own crash cuts.
func ensureOwnerMarker(repoDir string, spec WorktreeSpec) error {
	if spec.OwnerToken == "" {
		return fmt.Errorf("gitx: a worktree owner token is required")
	}
	root, err := os.OpenRoot(repoDir)
	if err != nil {
		return err
	}
	defer root.Close()
	rel := ownerMarkerRel(spec)
	if err := mkdirAllInRoot(root, path.Dir(rel)); err != nil {
		return err
	}
	err = atomicfile.InstallInRoot(root, rel, []byte(spec.OwnerToken), 0o600)
	if err == nil {
		return nil // freshly installed: content and entry are durable
	}
	if errors.Is(err, fs.ErrExist) {
		got, rerr := atomicfile.ReadInRoot(root, rel, maxOwnerMarker)
		if rerr != nil {
			return rerr
		}
		if string(got) != spec.OwnerToken {
			return fmt.Errorf("gitx: worktree owner marker is not ours")
		}
		return markerReconfirm(root, rel) // re-force content + entry durable on every retry
	}
	return err // includes *PostCommitSyncError (committed but unsynced) -> a retry re-confirms
}

// markerReconfirm re-forces an existing marker's content and directory entry durable (a package
// var so a test can inject a fail-once barrier proving the retry actually re-confirms).
var markerReconfirm = func(root *os.Root, rel string) error {
	if err := atomicfile.SyncInRoot(root, rel); err != nil {
		return err
	}
	return atomicfile.ConfirmParentInRoot(root, rel)
}

// mkdirAllInRoot durably creates dir and each missing ancestor within the confined root, each
// entry re-confirmed durable (MkdirInRoot is idempotent on an existing directory).
func mkdirAllInRoot(root *os.Root, dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	cur := ""
	for _, p := range strings.Split(dir, "/") {
		if p == "" {
			continue
		}
		if cur == "" {
			cur = p
		} else {
			cur += "/" + p
		}
		if err := atomicfile.MkdirInRoot(root, cur, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// isEmptyDir reports whether a directory has no entries — the precise shape git's `worktree add`
// leaves when interrupted after it creates the target but before it writes any linkage/checkout.
func isEmptyDir(abs string) (bool, error) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// fsyncArgs prepends the durability config to a MUTATING git command so git fsyncs everything it
// writes (refs, index, loose objects, derived metadata) before it returns.
func fsyncArgs(args ...string) []string {
	return append([]string{"-c", "core.fsync=all", "-c", "core.fsyncMethod=fsync"}, args...)
}

// Observe classifies the provisioning state without mutating anything. Ownership is PROVEN, not
// assumed from the unique path: a directory at the target counts as ours only when a git worktree
// registration binds that path to our run branch. A bare directory, a non-directory node, a
// mismatched ref, or our branch checked out elsewhere are all foreign and fail closed.
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
	regAtPath := findRegByPath(regs, abs)
	regOfBranch := findRegByBranch(regs, spec.Branch)
	node, err := lstatKind(abs)
	if err != nil {
		return WorktreeForeign, err
	}
	owner, err := classifyOwner(repoDir, spec)
	if err != nil {
		return WorktreeForeign, err
	}

	// An UNREGISTERED directory at our path is recoverable ONLY when it is both proven ours by the
	// marker AND the precise empty shape git's interrupted `worktree add` leaves; a bare directory
	// carrying any content, or without our marker, is foreign — the marker (a sibling token) can
	// authorize touching only that exact empty prefix, never a populated target.
	bareForeign := false
	if node == nodeDir && regAtPath == nil {
		if owner != ownerOurs {
			bareForeign = true
		} else if empty, eerr := isEmptyDir(abs); eerr != nil {
			return WorktreeForeign, eerr
		} else {
			bareForeign = !empty // our marker only authorizes the exact empty interrupted prefix
		}
	}

	// Foreign / mismatched — fail closed, never touched:
	//   - our branch name resolves to a different commit;
	//   - a worktree at our path is registered on a different branch;
	//   - our branch is already checked out at a DIFFERENT path;
	//   - a non-directory node (regular file, symlink, device) occupies our path;
	//   - an unregistered directory that is not our own empty interrupted prefix.
	if (oid != "" && oid != spec.BaseCommit) ||
		(regAtPath != nil && !regAtPath.onBranch(spec.Branch)) ||
		(regOfBranch != nil && !samePath(regOfBranch.path, abs)) ||
		node == nodeOther ||
		bareForeign {
		return WorktreeForeign, nil
	}

	// Applied: ref at base, our registration present and not prunable, the directory present at
	// base, clean.
	if oid == spec.BaseCommit && regAtPath != nil && regAtPath.onBranch(spec.Branch) && !regAtPath.prunable && node == nodeDir && regAtPath.head == spec.BaseCommit {
		clean, err := w.cleanTree(ctx, abs)
		if err != nil {
			return WorktreeForeign, err
		}
		if clean {
			return WorktreeApplied, nil
		}
		return WorktreeOwnPartial, nil // registered to us but not a clean base checkout
	}

	// Absent: nothing of ours anywhere.
	if oid == "" && regAtPath == nil && regOfBranch == nil && node == nodeAbsent && owner == ownerAbsent {
		return WorktreeAbsent, nil
	}

	// Our own incomplete prefix: a ref-only branch (path absent), our marker plus the empty target
	// of an interrupted `git worktree add`, or a worktree registered to our branch that is not yet
	// a clean base checkout (present, stale, or its directory gone). Every foreign shape was
	// rejected above, so completing this forward is safe.
	return WorktreeOwnPartial, nil
}

// Apply provisions forward from Absent or OwnPartial to Applied. It is only invoked by the
// transaction after Observe reported a non-foreign state, and re-proves ownership defensively so
// it never removes or adopts a node that is not bound to this run. The only removal is
// `git worktree remove --force` on a registration proven to be ours — identity-bound to git's own
// worktree record, and effective even when the directory is already gone; there is no raw
// directory deletion.
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

	// 2) Re-prove non-foreign BEFORE touching anything (mirror Observe): a foreign ref/path/branch,
	// a non-directory node, a foreign marker, or a bare directory not vouched for by our marker all
	// fail closed — Apply never adopts, clears, or marks a node that is not provably ours.
	regs, err := w.listWorktrees(ctx, repoDir)
	if err != nil {
		return err
	}
	regAtPath := findRegByPath(regs, abs)
	regOfBranch := findRegByBranch(regs, spec.Branch)
	node, err := lstatKind(abs)
	if err != nil {
		return err
	}
	owner, err := classifyOwner(repoDir, spec)
	if err != nil {
		return err
	}
	if regAtPath != nil && !regAtPath.onBranch(spec.Branch) {
		return fmt.Errorf("gitx: worktree at %s is registered to %q, not run branch %q", abs, regAtPath.branch, spec.Branch)
	}
	if regOfBranch != nil && !samePath(regOfBranch.path, abs) {
		return fmt.Errorf("gitx: run branch %q is already checked out at %s", spec.Branch, regOfBranch.path)
	}
	if node == nodeOther {
		return fmt.Errorf("gitx: %s exists and is not a directory", abs)
	}
	// An unregistered directory must be BOTH proven ours and the exact empty interrupted prefix.
	if node == nodeDir && regAtPath == nil {
		if owner != ownerOurs {
			return fmt.Errorf("gitx: %s is an unregistered directory, not provisioned by this run", abs)
		}
		if empty, eerr := isEmptyDir(abs); eerr != nil {
			return eerr
		} else if !empty {
			return fmt.Errorf("gitx: %s is a non-empty unregistered directory; refusing to touch it", abs)
		}
	}

	// 3) Publish our durable ownership marker (atomic, no-clobber, re-confirmed) — so that if
	// `git worktree add` is interrupted after it creates the target directory but before it writes
	// the linkage files, a later recovery can prove that empty directory is ours. Only reached once
	// abs is confirmed non-foreign above, so the marker never vouches for a foreign occupant.
	if err := ensureOwnerMarker(repoDir, spec); err != nil {
		return err
	}

	// Already fully applied?
	if regAtPath != nil && !regAtPath.prunable && node == nodeDir && regAtPath.head == spec.BaseCommit {
		if clean, err := w.cleanTree(ctx, abs); err != nil {
			return err
		} else if clean {
			return nil
		}
	}
	// Clear our own incomplete prefix so `git worktree add` starts from a clean path:
	//   - a registered-to-us worktree: git removes its own record (identity-bound, works even when
	//     the directory is already gone);
	//   - the empty target of an interrupted worktree add: proven ours by the marker AND re-checked
	//     empty, then removed with a NON-recursive os.Remove (it fails on any content) — a
	//     populated or foreign directory was already rejected above and is never deleted.
	if regAtPath != nil {
		if _, err := w.git.Run(ctx, repoDir, nil, "worktree", "remove", "--force", abs); err != nil {
			return err
		}
	} else if node == nodeDir {
		if st, err := classifyOwner(repoDir, spec); err != nil {
			return err
		} else if st != ownerOurs {
			return fmt.Errorf("gitx: refusing to remove %s: ownership not proven", abs)
		}
		if empty, eerr := isEmptyDir(abs); eerr != nil {
			return eerr
		} else if !empty {
			return fmt.Errorf("gitx: refusing to remove non-empty directory %s", abs)
		}
		if err := os.Remove(abs); err != nil {
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

// Barrier seams (package vars so a test can inject a fail-once / counting barrier). The defaults
// are the vetted atomicfile primitives, which are the REAL platform barriers — a writable-handle
// FlushFileBuffers on Windows and an fsync on POSIX — never a no-op.
var (
	confirmFileBarrier = fsyncFileContent
	confirmDirBarrier  = atomicfile.ParentBarrier
)

// barrierSentinel is a never-created child name: ParentBarrier(join(dir, sentinel)) flushes dir
// itself (it only uses the parent of its argument), forcing the entries INSIDE dir durable.
const barrierSentinel = "_claudex_barrier"

// Confirm re-proves the applied identity AND forces every durability the completed worktree
// depends on to disk, on both platforms. Observe == Applied proves branch/registration/HEAD/
// clean-tree identity. Then it barriers the FILE CONTENTS that encode the run's structure (the
// worktree<->admin gitlinks, HEAD, the index, and the loose ref) and the DIRECTORY ENTRIES that
// publish them (the worktree dir, the admin dir, the first .git/worktrees dir, and the new
// loose-ref directory chain) — git's core.fsync covers ref/object/index content but not these
// worktree linkage files, directory entries, or the .git/worktrees publication. A barrier failure
// returns an error, so the transaction never records this step's progress over an unconfirmed
// worktree; the whole method is idempotent and re-runnable on recovery.
func (w Worktree) Confirm(ctx context.Context, repoDir string, spec WorktreeSpec) error {
	st, err := w.Observe(ctx, repoDir, spec)
	if err != nil {
		return err
	}
	if st != WorktreeApplied {
		return fmt.Errorf("gitx: worktree is %s, not durably applied", st)
	}
	abs := worktreeAbs(repoDir, spec)
	adminDir, err := w.revParsePath(ctx, abs, "--absolute-git-dir")
	if err != nil {
		return err
	}
	commonDir, err := w.revParsePath(ctx, abs, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	refFile := filepath.Join(commonDir, "refs", "heads", filepath.FromSlash(spec.Branch))

	// (a) File contents encoding the structural identity. A missing file (e.g. a packed ref) is a
	// no-op, not an error.
	for _, f := range []string{
		filepath.Join(abs, ".git"),           // worktree -> admin linkage
		filepath.Join(adminDir, "gitdir"),    // admin -> worktree linkage
		filepath.Join(adminDir, "commondir"), // admin -> main repo linkage
		filepath.Join(adminDir, "HEAD"),      // the checked-out branch
		filepath.Join(adminDir, "index"),     // the staged base tree
		refFile,                              // the loose branch ref
	} {
		if err := confirmFileBarrier(f); err != nil {
			return fmt.Errorf("gitx: confirm content %s: %w", f, err)
		}
	}

	// (b) Directory entries publishing those files/dirs. confirmDirBarrier(X) forces X's own entry
	// (it flushes X's parent); barriering join(D, sentinel) flushes D itself, forcing its children.
	for _, d := range []string{
		filepath.Join(abs, barrierSentinel),      // flush the worktree dir (its .git gitlink entry)
		abs,                                      // the worktree dir entry in .claudex/runs/<id>
		filepath.Join(adminDir, barrierSentinel), // flush the admin dir (gitdir/commondir/HEAD/index)
		adminDir,                                 // the admin dir entry in .git/worktrees
		filepath.Join(commonDir, "worktrees"),    // the first .git/worktrees dir entry in .git
	} {
		if err := confirmDirBarrier(d); err != nil {
			return fmt.Errorf("gitx: confirm entry %s: %w", d, err)
		}
	}

	// The loose-ref entry plus any NEW intermediate ref directory (e.g. refs/heads/claudex/),
	// walking up until the pre-existing refs/heads.
	headsDir := filepath.Join(commonDir, "refs", "heads")
	for p := refFile; ; p = filepath.Dir(p) {
		if err := confirmDirBarrier(p); err != nil {
			return fmt.Errorf("gitx: confirm ref entry %s: %w", p, err)
		}
		if samePath(filepath.Dir(p), headsDir) {
			break
		}
	}

	// (c) The checked-out working tree itself: git's core.fsync persists objects/index/refs but
	// NOT the working-tree files it writes, so a crash after this could lose or truncate a checkout
	// file while the bootstrap journal is already terminal. Force every regular file's content and
	// every directory's entries durable; a symlink has no content to fsync and its entry is forced
	// by its parent directory's flush. (git worktree add does not populate submodules, so a gitlink
	// is only an empty placeholder directory here.)
	if err := w.barrierCheckoutTree(ctx, abs); err != nil {
		return err
	}
	return nil
}

// barrierCheckoutTree forces every regular file's content and every directory's entries in the
// checked-out worktree durable. WalkDir uses Lstat, so symlinks are not followed (no loops) and a
// symlinked directory is treated as a leaf entry made durable by its parent's flush. The context
// is checked between entries so a cancellation stops the (O(checkout)) walk promptly — an
// individual fsync is left to complete rather than being interrupted mid-flight.
func (w Worktree) barrierCheckoutTree(ctx context.Context, abs string) error {
	return filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			// Flush this directory so its child entries (files, subdirs, symlinks) are durable.
			return confirmDirBarrier(filepath.Join(path, barrierSentinel))
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // a symlink has no content; its entry is durable via its parent's flush
		}
		if err := confirmFileBarrier(path); err != nil {
			return fmt.Errorf("gitx: confirm checkout file %s: %w", path, err)
		}
		return nil
	})
}

// fsyncFileContent forces a file's content durable (a writable-handle FlushFileBuffers on
// Windows, an fsync on POSIX). A missing file is a no-op — a loose ref may have been packed away.
func fsyncFileContent(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	serr := f.Sync()
	cerr := f.Close()
	if serr != nil {
		return serr
	}
	return cerr
}

// revParsePath runs `git -C wtPath rev-parse <args...>` and returns the trimmed single-line path.
func (w Worktree) revParsePath(ctx context.Context, wtPath string, args ...string) (string, error) {
	out, err := w.git.Run(ctx, wtPath, nil, append([]string{"rev-parse"}, args...)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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

// findRegByPath returns the registration whose path is the same filesystem location as abs, or
// nil — how we prove a directory at abs is a git-managed worktree of ours rather than a foreign
// occupant.
func findRegByPath(regs []worktreeReg, abs string) *worktreeReg {
	for i := range regs {
		if samePath(regs[i].path, abs) {
			return &regs[i]
		}
	}
	return nil
}

// findRegByBranch returns the registration whose branch is ours, at ANY path, or nil — so our
// branch being checked out at an unexpected path is detected as foreign, not adopted.
func findRegByBranch(regs []worktreeReg, branch string) *worktreeReg {
	for i := range regs {
		if regs[i].onBranch(branch) {
			return &regs[i]
		}
	}
	return nil
}

// nodeKind is the filesystem node type at a path, classified without following symlinks so a
// symlink or special file at the target is never mistaken for an absent path or a directory.
type nodeKind int

const (
	nodeAbsent nodeKind = iota
	nodeDir
	nodeOther // a regular file, symlink, device, or any non-directory node
)

// lstatKind classifies the node at path with Lstat (no symlink following). A genuine "not exist"
// is nodeAbsent; any other stat error fails closed.
func lstatKind(path string) (nodeKind, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nodeAbsent, nil
		}
		return nodeAbsent, err
	}
	if fi.IsDir() {
		return nodeDir, nil
	}
	return nodeOther, nil
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
