package proctree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// NullDevice is what the command receives as stdin.
const NullDevice = "/dev/null"

// CommandSpawn is everything needed to start the test command under containment.
type CommandSpawn struct {
	// Spec is the frozen execution description. Nothing about the command is taken from anywhere else.
	Spec ExecSpec
	// Stdout and Stderr are the write ends the COORDINATOR created. The supervisor passes them to the
	// child and closes its own copies immediately afterwards, so the coordinator can see stream EOF
	// when the command exits.
	Stdout *os.File
	Stderr *os.File
}

// SpawnContained starts the command as the leader of a fresh process group, holding exactly the
// descriptors it is entitled to and nothing else.
//
// Three things are deliberate:
//
//   - It builds exec.Cmd by hand rather than calling exec.Command, because exec.Command resolves a
//     bare name through LookPath at CONSTRUCTION time — before Env is assigned — and would therefore
//     consult the coordinator's ambient PATH. The frozen environment would then authorize one PATH
//     while a different ambient one chose the binary. Path is set from the already-resolved absolute
//     executable, so no lookup happens here at all.
//   - Setpgid puts the child in a fresh group whose id is its own pid, established between fork and
//     exec, so it holds before any user code runs. The supervisor stays OUTSIDE that group: it is the
//     process that must survive to signal and reap the group, and a group kill that included it would
//     destroy the only reaper.
//   - stdin is the null device. Inheriting a terminal would let a test command block waiting for
//     input — hanging until the timeout and then reporting a failure that says nothing about the code
//     — or consume operator data from a shared terminal.
//
// The caller is responsible for having set FD_CLOEXEC on every capability descriptor before this
// runs; ExtraFiles is deliberately left nil, so the child receives only fds 0, 1 and 2.
// It TAKES OWNERSHIP of the stream write ends. On every error it closes both itself; on success the
// caller closes them immediately, via CloseStreamCopies. Ownership has to be mechanical rather than a
// documented convention: an earlier version returned directly from each error and left the copies
// open, so a failed spawn meant the coordinator never saw stream EOF and the drain the terminal
// ordering depends on would wait forever. The comment claiming "every failure path" was true of the
// intent and false of the code.
func SpawnContained(s CommandSpawn) (cmd *exec.Cmd, err error) {
	// Installed FIRST, before any validation, because ownership begins when the handles arrive rather
	// than when they are found acceptable. With the pair check above this defer, passing a real stdout
	// end and a nil stderr returned an error while leaving the supplied end open — the same
	// no-EOF-forever deadlock, reachable through the very check meant to reject a malformed call.
	// CloseStreamCopies tolerates nil, so a half-supplied pair is closed correctly.
	defer func() {
		if err == nil {
			return
		}
		// A failed ownership release is NOT an ordinary pre-spawn refusal: the caller would treat the
		// error as "nothing started, streams are yours again" while a write end stayed open. Joined so
		// both facts reach the supervisor.
		if cerr := CloseStreamCopies(s); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	if s.Stdout == nil || s.Stderr == nil {
		return nil, fmt.Errorf("proctree: stream write ends are required")
	}
	if err = s.Spec.Validate(); err != nil {
		return nil, err
	}
	if err = validateExecutionBoundary(s.Spec); err != nil {
		return nil, err
	}
	var devNull *os.File
	devNull, err = os.OpenFile(NullDevice, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("proctree: open %s: %w", NullDevice, err)
	}
	defer devNull.Close()

	cmd = &exec.Cmd{
		Path:   string(s.Spec.Executable),
		Args:   s.Spec.ArgvStrings(),
		Env:    s.Spec.Environ(),
		Dir:    string(s.Spec.Cwd),
		Stdin:  devNull,
		Stdout: s.Stdout,
		Stderr: s.Stderr,
		// nil on purpose: ExtraFiles is the only way a child could inherit a capability descriptor
		// that Go itself passes.
		ExtraFiles: nil,
		SysProcAttr: &syscall.SysProcAttr{
			Setpgid: true,
			Pgid:    0, // 0 means "become the leader of a new group with pgid == pid"
		},
	}
	// ExtraFiles: nil is NOT sufficient on its own. It controls only what Go explicitly PASSES; any
	// descriptor the parent happens to hold without FD_CLOEXEC crosses exec regardless of intent.
	// This is not hypothetical — the child was observed inheriting the Go runtime's cgroup cpu.max
	// handle and a /dev/ptmx handle, neither of which anything here passed.
	if err = markInheritedCloexec(); err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("proctree: start command: %w", err)
	}
	return cmd, nil
}

// validateExecutionBoundary refuses anything that would reintroduce resolution at execution time.
//
// ExecSpec.Validate proves the spec is well formed; it does not prove it is EXECUTABLE as written. A
// relative executable or cwd reaching exec.Cmd would be resolved against whatever directory this
// process happens to be in — exactly the ambient-resolution defect the resolved absolute path exists
// to eliminate. The supervisor will separately compare these against the rooted intent, but this is
// the last boundary before the kernel and it refuses independently rather than trusting that an
// earlier check ran.
func validateExecutionBoundary(spec ExecSpec) error {
	for _, f := range []struct {
		what string
		path string
	}{
		{"executable", string(spec.Executable)},
		{"cwd", string(spec.Cwd)},
	} {
		if !filepath.IsAbs(f.path) {
			return fmt.Errorf("%w: %s %q is not absolute", ErrSpecInvalid, f.what, f.path)
		}
		if filepath.Clean(f.path) != f.path {
			return fmt.Errorf("%w: %s %q is not clean", ErrSpecInvalid, f.what, f.path)
		}
	}
	// The fold identity belongs to Windows. Executing under it on Linux would mean the digest bound
	// one comparison rule while the kernel applied another, so names differing only in case would be
	// one variable to the record and two to the process.
	if spec.Identity != NameByteExact {
		return fmt.Errorf("%w: environment identity %v is not Linux semantics", ErrSpecInvalid, spec.Identity)
	}
	return nil
}

// markInheritedCloexec sets FD_CLOEXEC on every descriptor above stderr.
//
// Marking a descriptor close-on-exec does not stop this process using it; it only stops the exec'd
// child inheriting it. Stdin, stdout and stderr are unaffected because os/exec dup2s its own handles
// onto the child's 0, 1 and 2, and dup2 clears FD_CLOEXEC on the descriptor it creates — so the
// streams still arrive even though their originals are marked here.
func markInheritedCloexec() error {
	const fdDir = "/proc/self/fd"
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return fmt.Errorf("proctree: enumerate open descriptors: %w", err)
	}
	for _, e := range entries {
		fd, aerr := strconv.Atoi(e.Name())
		if aerr != nil || fd <= 2 {
			continue
		}
		// EBADF is expected for the descriptor ReadDir itself used, which is already gone.
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, syscall.FD_CLOEXEC); errno != 0 && errno != syscall.EBADF {
			return fmt.Errorf("proctree: set FD_CLOEXEC on fd %d: %w", fd, errno)
		}
	}
	return nil
}

// CloseStreamCopies drops the supervisor's own handles on the stream write ends.
//
// It runs immediately after a successful Start and on every failure path. If any copy survives here,
// the coordinator never sees EOF on those streams when the command exits, so the drain that the
// terminal ordering depends on would hang forever. This is a separate function because it must also
// be called when the spawn fails, where there is no cmd to hang it off.
func CloseStreamCopies(s CommandSpawn) error {
	var first error
	for _, f := range []*os.File{s.Stdout, s.Stderr} {
		if f == nil {
			continue
		}
		// Idempotent: SpawnContained closes these on its own error paths, and the caller closes them
		// after a successful start, so an already-closed handle is an expected state rather than a
		// fault. Anything else is reported.
		if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) && first == nil {
			first = fmt.Errorf("proctree: close stream copy: %w", err)
		}
	}
	return first
}
