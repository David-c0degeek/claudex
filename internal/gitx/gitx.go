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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// runObserver is a test-only seam invoked with the full argv (the -c overrides plus the caller's
// args) of every git invocation, so a test can assert, e.g., that object writers carry the fsync
// configuration. Nil in production.
var runObserver func(argv []string)

// ErrClosed is returned when Run/RunCode is called after Close. A closed handle no longer owns
// its hooks directory, so running git would emit the ambiguous empty core.hooksPath value the
// hardening exists to prevent; the handle fails closed instead.
var ErrClosed = errors.New("gitx: handle is closed")

// Git is a hardened handle to the native git executable. It owns a fresh empty hooks
// directory reused across invocations; Close removes it and disables the handle.
type Git struct {
	hooksDir string
	closed   bool
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

// Close removes the owned hooks directory and marks the handle unusable so a later Run/RunCode
// fails closed rather than running git with an empty (ambiguous) hooks path. It is idempotent.
func (g *Git) Close() error {
	if g.closed {
		return nil
	}
	g.closed = true
	dir := g.hooksDir
	g.hooksDir = ""
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
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

// ErrOutputTooLarge means a machine inventory exceeded the total output bound (fail closed, never a
// truncated success). ErrUnterminatedRecord means a NUL-record stream ended without a final NUL.
var (
	ErrOutputTooLarge     = errors.New("gitx: git output exceeded the bound")
	ErrUnterminatedRecord = errors.New("gitx: unterminated NUL record")
)

// maxNulRecord bounds a SINGLE NUL-separated record (a path or a diff header). maxInventoryBytes is
// the TOTAL output ceiling for RunNulRecords — a documented, realistic bound (a very large tree's
// `ls-tree -r` inventory is tens of MiB) that keeps memory bounded per 04.0's hardened-leaf
// requirement while comfortably covering legitimate >1 MiB inventories. Both are package vars so a
// test can shrink them to exercise the fail-closed overflow paths cheaply.
var (
	maxNulRecord      = 1 << 20  // 1 MiB per record
	maxInventoryBytes = 64 << 20 // 64 MiB total
)

// RunNulRecords runs git and returns its stdout split into NUL-separated records, reading past
// plain Run's 1 MiB cap so a machine inventory (`ls-tree -r -z`, `diff-index -z`) is never silently
// truncated — while staying BOUNDED: total output over maxInventoryBytes, a single record over
// maxNulRecord, an UNTERMINATED final record, a cancelled context, or a non-zero exit are all
// fail-closed errors. On any early abort the child is killed and its pipe drained before Wait, so a
// runaway producer cannot deadlock the wait.
func (g *Git) RunNulRecords(ctx context.Context, dir string, extraEnv map[string]string, args ...string) ([]string, error) {
	if g.closed {
		return nil, ErrClosed
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("%w: no git subcommand", ErrGit)
	}
	full := append([]string{
		"-c", "core.hooksPath=" + g.hooksDir,
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
	}, args...)
	if runObserver != nil {
		runObserver(full)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = scrubbedEnv(extraEnv)
	var stderr boundedBuffer
	stderr.limit = maxGitOutput
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: git %s: %v", ErrGit, args[0], err)
	}
	// abort kills the child and drains the remaining buffered output before Wait, so a producer
	// still writing cannot block the wait; the caller's error is returned.
	abort := func(retErr error) ([]string, error) {
		_ = cmd.Process.Kill()
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
		return nil, retErr
	}

	var records []string
	var total int
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxNulRecord)
	scanner.Split(scanNul)
	for scanner.Scan() {
		b := scanner.Bytes()
		total += len(b) + 1 // include the record's NUL terminator
		if total > maxInventoryBytes {
			return abort(fmt.Errorf("%w: git %s", ErrOutputTooLarge, args[0]))
		}
		records = append(records, string(b))
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return abort(fmt.Errorf("%w: git %s: %v", ErrGit, args[0], scanErr))
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			return nil, fmt.Errorf("%w: git %s: exit %d: %s", ErrGit, args[0], ee.ExitCode(), redact.Text(strings.TrimSpace(stderr.String())))
		}
		return nil, fmt.Errorf("%w: git %s: %v", ErrGit, args[0], waitErr)
	}
	return records, nil
}

// scanNul is a bufio.SplitFunc yielding NUL-terminated records. Because these commands promise a
// NUL after EVERY record, output that ends without one is malformed and fails closed rather than
// yielding an unterminated final token.
func scanNul(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		if len(data) > 0 {
			return 0, nil, ErrUnterminatedRecord
		}
		return 0, nil, nil
	}
	return 0, nil, nil // need more data
}

// exec runs git under the hardened isolation and returns trimmed stdout, raw stderr, the process
// exit code, and a run error. The run error is set only for a cancelled context or a failure to
// start the process (code -1); a git command that runs and exits non-zero returns that code with
// a nil error, so callers that treat exit codes as data can distinguish the two.
func (g *Git) exec(ctx context.Context, dir string, extraEnv map[string]string, args ...string) (stdout, stderr []byte, code int, err error) {
	if g.closed {
		return nil, nil, -1, ErrClosed
	}
	if len(args) == 0 {
		return nil, nil, -1, fmt.Errorf("%w: no git subcommand", ErrGit)
	}
	// -c overrides precede the subcommand: no hooks, no signing, no ambient config surprises.
	full := append([]string{
		"-c", "core.hooksPath=" + g.hooksDir,
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
	}, args...)
	if runObserver != nil {
		runObserver(full)
	}
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
