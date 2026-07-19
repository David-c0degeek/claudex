// Package gitx is the coordinator's git participant: it shells out to the native git
// executable via exec.CommandContext with EXPLICIT ARGV (never a shell), so the coordinator
// owns git as a concrete participant in the prepared-transaction machine.
//
// Hardening (subject 04.0): every invocation disables hooks by pointing core.hooksPath at a
// FRESH EMPTY directory (an empty core.hooksPath value is ambiguous), neutralizes personal and
// system git configuration, strips ambient authority-changing GIT_* variables from the child
// environment, forces the C locale and non-interactive behavior, and bounds + redacts the
// captured output. This is NOT a sandbox: a repository's own .gitattributes or configured
// clean/smudge/diff filters can still execute helper programs with the user's OS authority —
// callers run git only against repositories the user already controls.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/David-c0degeek/claudex/internal/redact"
)

// maxGitOutput bounds each captured stream; git plumbing output (OIDs, short status) is tiny,
// so a generous cap only guards against a pathological/hostile command flooding memory.
const maxGitOutput = 1 << 20 // 1 MiB per stream

// ErrGit is the sentinel a non-zero git exit (or a failure to run git) wraps.
var ErrGit = errors.New("gitx: git command failed")

// Git is a hardened handle to the native git executable. It owns a fresh empty hooks
// directory reused across invocations; Close removes it.
type Git struct {
	hooksDir string
}

// New creates a Git handle backed by a fresh empty hooks directory. The directory exists only
// to give core.hooksPath an unambiguous empty target so no repository or global hook runs.
func New() (*Git, error) {
	dir, err := os.MkdirTemp("", "claudex-git-nohooks-")
	if err != nil {
		return nil, fmt.Errorf("gitx: create hooks dir: %w", err)
	}
	return &Git{hooksDir: dir}, nil
}

// Close removes the owned hooks directory.
func (g *Git) Close() error {
	if g.hooksDir == "" {
		return nil
	}
	err := os.RemoveAll(g.hooksDir)
	g.hooksDir = ""
	return err
}

// Run executes `git <args...>` in dir with the hardened isolation described on the package,
// returning trimmed stdout. extraEnv overlays explicit variables (e.g. GIT_INDEX_FILE,
// GIT_AUTHOR_*) onto the scrubbed environment. A non-zero exit wraps ErrGit with the redacted,
// bounded stderr. The context governs cancellation and timeout.
func (g *Git) Run(ctx context.Context, dir string, extraEnv map[string]string, args ...string) ([]byte, error) {
	stdout, stderr, code, err := g.exec(ctx, dir, extraEnv, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%w: git %s: exit %d: %s", ErrGit, args[0], code, redact.Text(strings.TrimSpace(string(stderr))))
	}
	return stdout, nil
}

// RunCode runs git and reports the process EXIT CODE as data rather than an error, so a caller
// can treat a specific non-zero code as a boolean answer (e.g. check-ignore's 1 = "not ignored",
// or `merge-base --is-ancestor`'s 1). err is non-nil ONLY when git could not be started or the
// context was cancelled (code -1 then); an ordinary non-zero git exit returns (stdout, code, nil).
func (g *Git) RunCode(ctx context.Context, dir string, extraEnv map[string]string, args ...string) ([]byte, int, error) {
	stdout, _, code, err := g.exec(ctx, dir, extraEnv, args...)
	return stdout, code, err
}

// exec runs git under the hardened isolation and returns trimmed stdout, raw stderr, the process
// exit code, and a run error. The run error is set only for a cancelled context or a failure to
// start the process (code -1); a git command that runs and exits non-zero returns that code with
// a nil error, so callers that treat exit codes as data can distinguish the two.
func (g *Git) exec(ctx context.Context, dir string, extraEnv map[string]string, args ...string) (stdout, stderr []byte, code int, err error) {
	// -c overrides precede the subcommand: no hooks, no signing, no ambient config surprises.
	full := append([]string{
		"-c", "core.hooksPath=" + g.hooksDir,
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = scrubbedEnv(extraEnv)

	var out, errb boundedBuffer
	out.limit = maxGitOutput
	errb.limit = maxGitOutput
	cmd.Stdout = &out
	cmd.Stderr = &errb

	runErr := cmd.Run()
	stdout = bytes.TrimRight(out.Bytes(), "\n")
	stderr = errb.Bytes()
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	if ctx.Err() != nil {
		return stdout, stderr, -1, ctx.Err() // cancellation/timeout, not a git verdict
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return stdout, stderr, ee.ExitCode(), nil // a git verdict; the code is data
	}
	// git could not be started (not on PATH, permission, etc.).
	return stdout, stderr, -1, fmt.Errorf("%w: git %s: %v", ErrGit, args[0], runErr)
}

// scrubbedEnv builds a minimal, git-config-neutral child environment. It keeps only what git
// needs to run (PATH, and on Windows the loader/user vars), neutralizes global+system config,
// forces the C locale and non-interactive behavior, drops every ambient GIT_* variable, then
// overlays the caller's explicit extra variables (which may include GIT_* the caller owns).
func scrubbedEnv(extra map[string]string) []string {
	env := map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull, // ignore ~/.gitconfig
		"GIT_CONFIG_SYSTEM":   os.DevNull, // ignore /etc/gitconfig
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0", // never block on credential/askpass prompts
		"GIT_OPTIONAL_LOCKS":  "0",
		"LC_ALL":              "C", // deterministic, parseable output
	}
	// Carry through only the non-GIT_* variables git needs to locate its executable and,
	// on Windows, load DLLs / resolve the user profile.
	keep := []string{"PATH", "Path", "SystemRoot", "SystemDrive", "windir", "TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "ComSpec", "PATHEXT"}
	if runtime.GOOS != "windows" {
		keep = []string{"PATH", "TMPDIR"}
	}
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range extra { // caller-owned explicit variables win
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// boundedBuffer captures up to limit bytes and silently discards the rest, so a hostile or
// runaway command cannot exhaust memory through git's streams.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil // always report full consumption; the cap is intentional
}

func (b *boundedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *boundedBuffer) String() string { return b.buf.String() }
