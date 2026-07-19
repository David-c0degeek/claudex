package gitx

import (
	"context"
	"fmt"
	"strings"
)

// ErrBranchName is the sentinel an invalid base-branch name wraps. It is returned BEFORE any
// git command runs: a name that could escape the refs/heads/ namespace (a "../" traversal) or
// pose as an option must never reach argv, even behind --end-of-options.
var ErrBranchName = fmt.Errorf("gitx: invalid base branch name")

// BaseResolver resolves a policy base branch to an EXACT commit OID through the hardened git
// leaf. It resolves the LOCAL branch specifically — refs/heads/<branch> peeled to a commit —
// never an ambiguous revision, a tag, or a remote-tracking ref, so a run is always anchored to
// the operator's local branch tip and never a same-named tag an attacker could add.
type BaseResolver struct{ git *Git }

// NewBaseResolver wraps a hardened git handle as a base resolver. The handle's lifecycle
// (its owned hooks directory) is the caller's; this type never closes it.
func NewBaseResolver(g *Git) BaseResolver { return BaseResolver{git: g} }

// ResolveBase resolves baseBranch in the repository at repoDir to a single 40/64 lower-hex
// commit OID. It validates the branch name first (namespace-escape / option-injection safe),
// then runs `git rev-parse --verify --end-of-options refs/heads/<branch>^{commit}` — --verify
// requires exactly one valid object, --end-of-options fixes the ref as a positional argument,
// refs/heads/ pins the local-branch namespace, and ^{commit} peels to a commit-ish. The output
// is strictly parsed as one OID. The context governs cancellation of the git command.
func (r BaseResolver) ResolveBase(ctx context.Context, repoDir, baseBranch string) (string, error) {
	if err := validateBranchName(baseBranch); err != nil {
		return "", err
	}
	out, err := r.git.Run(ctx, repoDir, nil,
		"rev-parse", "--verify", "--end-of-options", "refs/heads/"+baseBranch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("gitx: resolve base branch %q: %w", baseBranch, err)
	}
	oid, err := parseOID(string(out))
	if err != nil {
		return "", fmt.Errorf("gitx: resolve base branch %q: %w", baseBranch, err)
	}
	return oid, nil
}

// parseOID strictly parses git output as exactly one 40-hex (SHA-1) or 64-hex (SHA-256) lower-
// case object id. A hardened resolver never trusts loosely-shaped git output.
func parseOID(s string) (string, error) {
	oid := strings.TrimSpace(s)
	if len(oid) != 40 && len(oid) != 64 {
		return "", fmt.Errorf("gitx: expected a 40/64-hex OID, got %d chars", len(oid))
	}
	for _, c := range oid {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", fmt.Errorf("gitx: OID is not lower-hex")
		}
	}
	return oid, nil
}

// validateBranchName rejects any branch name that git check-ref-format would refuse for a
// refs/heads/<name> ref, PLUS a leading '-' (which would read as an option). This is the
// security boundary: it prevents a "../" component from normalizing out of refs/heads/ and
// prevents option/metacharacter injection, independent of git's own later rejection.
func validateBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrBranchName)
	}
	if len(name) > 255 {
		return fmt.Errorf("%w: too long", ErrBranchName)
	}
	if name[0] == '-' {
		return fmt.Errorf("%w: begins with '-'", ErrBranchName)
	}
	if name == "@" || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return fmt.Errorf("%w: reserved sequence", ErrBranchName)
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f { // ASCII controls + DEL
			return fmt.Errorf("%w: control character", ErrBranchName)
		}
		switch c {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return fmt.Errorf("%w: forbidden character %q", ErrBranchName, c)
		}
	}
	// Slash-separated components: no empty component (rejects a leading/trailing slash and any
	// "//"), none may begin with '.' or end with ".lock".
	for _, comp := range strings.Split(name, "/") {
		if comp == "" {
			return fmt.Errorf("%w: empty path component", ErrBranchName)
		}
		if comp[0] == '.' {
			return fmt.Errorf("%w: component begins with '.'", ErrBranchName)
		}
		if strings.HasSuffix(comp, ".lock") {
			return fmt.Errorf("%w: component ends with .lock", ErrBranchName)
		}
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("%w: ends with '.'", ErrBranchName)
	}
	return nil
}
