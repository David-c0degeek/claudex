package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrPreflight is the sentinel a repository-preflight refusal wraps.
var ErrPreflight = errors.New("gitx: repository preflight failed")

// minGitMajor/minGitMinor is the lowest git the coordinator supports: every relied-on feature
// must exist at this version. core.fsync/core.fsyncMethod (the worktree durability barrier)
// arrived in 2.36; --path-format (preflight's root bind) in 2.31 — so 2.36 is the floor.
const (
	minGitMajor = 2
	minGitMinor = 36
)

// Preflight validates that repoDir is a pristine git repository ready to host a NEW run:
//   - repoDir is the EXACT repository root (not a subdirectory of one);
//   - the runtime directory (runtimeDir, e.g. ".claudex") is git-ignored, so nothing the
//     coordinator writes there will ever pollute the user's working tree;
//   - no path under runtimeDir is tracked (a stray committed .claudex would defeat the ignore);
//   - the working tree is clean — no tracked modifications, no staged changes, and no
//     non-ignored untracked files.
//
// It is READ-ONLY and must run ONLY on the definite-new-run path, before any runtime dir, repo
// lock, or run directory is created. A recovering bootstrap must never rerun it: the bootstrap
// itself writes under runtimeDir and would perturb the ambient state this checks.
func (g *Git) Preflight(ctx context.Context, repoDir, runtimeDir string) error {
	// 0) minimum git version: every relied-on mechanism (notably the core.fsync durability
	// barrier) must be present, checked before any mutation so an old git is refused cleanly.
	if err := g.checkMinVersion(ctx, repoDir); err != nil {
		return err
	}

	// 1) exact repository-root bind: repoDir must BE the toplevel, so a run is never bootstrapped
	// from a subdirectory whose .claudex would sit at an unexpected place.
	top, err := g.Run(ctx, repoDir, nil, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%w: not a git repository root: %v", ErrPreflight, err)
	}
	if !samePath(strings.TrimSpace(string(top)), repoDir) {
		return fmt.Errorf("%w: %s is not the repository root (toplevel is %s)", ErrPreflight, repoDir, strings.TrimSpace(string(top)))
	}

	// 2) the runtime directory must be git-ignored. The ignore rule is commonly directory-only
	// (".claudex/"), which check-ignore matches for the directory form and any child but NOT a
	// bare non-existent path, so query the directory form explicitly.
	_, code, err := g.RunCode(ctx, repoDir, nil, "check-ignore", "-q", "--", runtimeDir+"/")
	if err != nil {
		return fmt.Errorf("%w: check-ignore: %v", ErrPreflight, err)
	}
	switch code {
	case 0: // ignored
	case 1:
		return fmt.Errorf("%w: %s is not git-ignored (add it to .gitignore)", ErrPreflight, runtimeDir)
	default:
		return fmt.Errorf("%w: check-ignore exited %d", ErrPreflight, code)
	}

	// 3) nothing under the runtime dir may be tracked (tracked overrides ignore).
	tracked, err := g.Run(ctx, repoDir, nil, "ls-files", "-z", "--", runtimeDir)
	if err != nil {
		return fmt.Errorf("%w: ls-files: %v", ErrPreflight, err)
	}
	if len(bytes.Trim(tracked, "\x00")) != 0 {
		return fmt.Errorf("%w: tracked paths exist under %s", ErrPreflight, runtimeDir)
	}

	// 4) a clean start: no tracked modifications, no staged changes, no non-ignored untracked.
	status, err := g.Run(ctx, repoDir, nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("%w: status: %v", ErrPreflight, err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("%w: the working tree is not clean", ErrPreflight)
	}
	return nil
}

// checkMinVersion runs `git version` and refuses a git below the supported floor.
func (g *Git) checkMinVersion(ctx context.Context, dir string) error {
	out, err := g.Run(ctx, dir, nil, "version")
	if err != nil {
		return fmt.Errorf("%w: git version: %v", ErrPreflight, err)
	}
	maj, min, perr := parseGitVersion(string(out))
	if perr != nil {
		return fmt.Errorf("%w: %v", ErrPreflight, perr)
	}
	if belowMinVersion(maj, min) {
		return fmt.Errorf("%w: git %d.%d is below the required minimum %d.%d", ErrPreflight, maj, min, minGitMajor, minGitMinor)
	}
	return nil
}

// belowMinVersion reports whether major.minor is below the supported floor.
func belowMinVersion(maj, min int) bool {
	return maj < minGitMajor || (maj == minGitMajor && min < minGitMinor)
}

// parseGitVersion strictly parses the `git version` line ("git version 2.54.0.windows.1") into
// its major and minor numbers, failing on any output that is not the recognized shape.
func parseGitVersion(s string) (int, int, error) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) < 3 || f[0] != "git" || f[1] != "version" {
		return 0, 0, fmt.Errorf("gitx: unrecognized git version output %q", s)
	}
	parts := strings.SplitN(f[2], ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("gitx: unparseable git version %q", f[2])
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("gitx: bad major version in %q", f[2])
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("gitx: bad minor version in %q", f[2])
	}
	return maj, min, nil
}
