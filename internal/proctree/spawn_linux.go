package proctree

import (
	"fmt"
	"os"
	"os/exec"
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
func SpawnContained(s CommandSpawn) (*exec.Cmd, error) {
	if err := s.Spec.Validate(); err != nil {
		return nil, err
	}
	if s.Stdout == nil || s.Stderr == nil {
		return nil, fmt.Errorf("proctree: stream write ends are required")
	}
	devNull, err := os.OpenFile(NullDevice, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("proctree: open %s: %w", NullDevice, err)
	}
	defer devNull.Close()

	cmd := &exec.Cmd{
		Path:   string(s.Spec.Executable),
		Args:   s.Spec.ArgvStrings(),
		Env:    s.Spec.Environ(),
		Dir:    string(s.Spec.Cwd),
		Stdin:  devNull,
		Stdout: s.Stdout,
		Stderr: s.Stderr,
		// nil on purpose: ExtraFiles is the only way a child could inherit a capability descriptor.
		ExtraFiles: nil,
		SysProcAttr: &syscall.SysProcAttr{
			Setpgid: true,
			Pgid:    0, // 0 means "become the leader of a new group with pgid == pid"
		},
	}
	// ExtraFiles: nil is NOT sufficient on its own. It controls only what Go explicitly PASSES; any
	// descriptor the parent happens to hold without FD_CLOEXEC crosses exec regardless of intent.
	// This is not hypothetical — the child was observed inheriting the Go runtime's cgroup cpu.max
	// handle and a /dev/ptmx handle, neither of which anything here passed. A containment guarantee
	// that depends on every descriptor the process ever opened having been created correctly is not a
	// guarantee, so the set is closed explicitly instead.
	if err := markInheritedCloexec(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("proctree: start command: %w", err)
	}
	return cmd, nil
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
		if err := f.Close(); err != nil && first == nil {
			first = fmt.Errorf("proctree: close stream copy: %w", err)
		}
	}
	return first
}
