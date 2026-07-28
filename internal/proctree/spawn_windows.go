package proctree

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NullDevice is what the command receives as stdin.
const NullDevice = `NUL`

// ContainedProcess is a started command, already a member of its containment domain.
//
// It carries no group id: on Windows the domain is the job, and the job is named. A pid here would be
// a second identity for the same thing, which is the defect the Linux side just finished removing.
type ContainedProcess struct {
	handle windows.Handle
	pid    uint32
}

// PID is the leader's process id, for reporting only.
func (c *ContainedProcess) PID() uint32 { return c.pid }

// Close releases the process handle. The job, not this handle, is what contains the tree.
func (c *ContainedProcess) Close() error {
	if c.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(c.handle)
	c.handle = 0
	return err
}

// Wait blocks until the command exits or the deadline passes.
func (c *ContainedProcess) Wait(deadline time.Time) (bool, error) {
	if c.handle == 0 {
		return false, errors.New("proctree: contained process handle is closed")
	}
	ms := time.Until(deadline).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	ev, err := windows.WaitForSingleObject(c.handle, uint32(ms))
	switch {
	case err != nil:
		return false, fmt.Errorf("proctree: wait for contained process: %w", err)
	case ev == uint32(windows.WAIT_OBJECT_0):
		return true, nil
	case ev == uint32(windows.WAIT_TIMEOUT):
		return false, nil
	default:
		return false, fmt.Errorf("proctree: unexpected wait result 0x%08x", ev)
	}
}

// ExitCode reports the command's status. It is only meaningful once Wait has observed the exit.
func (c *ContainedProcess) ExitCode() (uint32, error) {
	if c.handle == 0 {
		return 0, errors.New("proctree: contained process handle is closed")
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.handle, &code); err != nil {
		return 0, fmt.Errorf("proctree: read exit code: %w", err)
	}
	return code, nil
}

// Spawn starts the command INSIDE this job, with membership established by the OS at creation.
//
// It is a method on the job rather than a package function because on Windows containment owns the
// spawn. There is no supervisor to hand a command to: the assignment happens as part of process
// creation, so "create the process" and "contain it" are one operation and cannot be separated by a
// caller who forgets the second half.
//
// Four things are deliberate:
//
//   - PROC_THREAD_ATTRIBUTE_JOB_LIST, not AssignProcessToJobObject. Assigning afterwards leaves a
//     window — however short — in which a running process is outside the domain, and a process that
//     spawns fast enough during that window escapes containment entirely. With the attribute the
//     process is a member before its first instruction.
//   - CREATE_SUSPENDED is kept even though the attribute already closes that window, and for a
//     different reason than the one it usually serves: it provides an instant at which the WHOLE
//     containment claim can be OBSERVED before any code runs — membership, kill-on-close in force, and
//     both breakaway flags absent. Membership alone would have been the weaker check: a process can be
//     in a job whose limits no longer contain anything. Any failure terminates the still-suspended
//     process instead of resuming it, which is the only moment at which refusing costs nothing.
//   - PROC_THREAD_ATTRIBUTE_HANDLE_LIST is the Windows analogue of marking every descriptor CLOEXEC.
//     bInheritHandles must be TRUE for the standard handles to arrive, and without a handle list that
//     inherits EVERY inheritable handle this process happens to hold. With one, exactly the three
//     listed handles cross the boundary.
//   - The executable is passed as lpApplicationName and the argv separately, so no path searching of
//     any kind happens here. The frozen environment authorizes a PATH; it must not then be a different,
//     ambient PATH that chose the binary.
//
// It TAKES OWNERSHIP of the stream write ends, closing both on every error path, exactly as the Linux
// spawn does — the coordinator is blocked on their EOF either way.
func (j *ArmedJob) Spawn(s CommandSpawn) (cp *ContainedProcess, err error) {
	defer func() {
		if err == nil {
			return
		}
		if cerr := CloseStreamCopies(s); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	if j.handle == 0 {
		return nil, fmt.Errorf("proctree: containment job for %q is closed", j.attemptID)
	}
	if s.Stdout == nil || s.Stderr == nil {
		return nil, fmt.Errorf("proctree: stream write ends are required")
	}
	if err = s.Spec.Validate(); err != nil {
		return nil, err
	}
	if err = validateExecutionBoundaryFor(s.Spec, NameASCIIFold); err != nil {
		return nil, err
	}

	var devNull *os.File
	devNull, err = os.OpenFile(NullDevice, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("proctree: open %s: %w", NullDevice, err)
	}
	defer devNull.Close()

	// Inheritability is granted for the duration of this call and then put back. Leaving it on would
	// let these handles cross into an UNRELATED CreateProcess elsewhere in the program that inherits
	// handles without a list — the leak the handle list prevents for our own child, reintroduced
	// through somebody else's.
	stdHandles := []windows.Handle{
		windows.Handle(devNull.Fd()),
		windows.Handle(s.Stdout.Fd()),
		windows.Handle(s.Stderr.Fd()),
	}
	var restore func()
	if restore, err = makeInheritable(stdHandles); err != nil {
		return nil, err
	}
	defer restore()

	var attrs *windows.ProcThreadAttributeListContainer
	if attrs, err = windows.NewProcThreadAttributeList(2); err != nil {
		return nil, fmt.Errorf("proctree: allocate process attribute list: %w", err)
	}
	defer attrs.Delete()
	jobs := []windows.Handle{j.handle}
	if err = attrs.Update(procThreadAttributeJobList, unsafe.Pointer(&jobs[0]), uintptr(len(jobs))*unsafe.Sizeof(jobs[0])); err != nil {
		return nil, fmt.Errorf("proctree: attach containment job to process creation: %w", err)
	}
	if err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&stdHandles[0]), uintptr(len(stdHandles))*unsafe.Sizeof(stdHandles[0])); err != nil {
		return nil, fmt.Errorf("proctree: restrict inherited handles: %w", err)
	}

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput = stdHandles[0]
	si.StdOutput = stdHandles[1]
	si.StdErr = stdHandles[2]
	si.ProcThreadAttributeList = attrs.List()

	var appName, cmdLine, cwd *uint16
	if appName, err = windows.UTF16PtrFromString(string(s.Spec.Executable)); err != nil {
		return nil, fmt.Errorf("%w: executable: %v", ErrSpecInvalid, err)
	}
	if cmdLine, err = windows.UTF16PtrFromString(windows.ComposeCommandLine(s.Spec.ArgvStrings())); err != nil {
		return nil, fmt.Errorf("%w: argv: %v", ErrSpecInvalid, err)
	}
	if cwd, err = windows.UTF16PtrFromString(string(s.Spec.Cwd)); err != nil {
		return nil, fmt.Errorf("%w: cwd: %v", ErrSpecInvalid, err)
	}
	var block []uint16
	if block, err = envBlockUTF16(s.Spec); err != nil {
		return nil, err
	}

	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED)
	if err = windows.CreateProcess(appName, cmdLine, nil, nil, true, flags, &block[0], cwd, &si.StartupInfo, &pi); err != nil {
		return nil, fmt.Errorf("proctree: start command: %w", err)
	}
	// The attribute list and the arrays it points at must outlive the call that reads them.
	runtime.KeepAlive(jobs)
	runtime.KeepAlive(stdHandles)
	runtime.KeepAlive(block)

	// From here the process EXISTS, suspended and contained. Every failure below terminates it rather
	// than returning while it holds the domain: a suspended member would keep the job alive and it
	// would never drain.
	//
	// Terminating is not enough on its own, and a test caught the difference. TerminateProcess only
	// REQUESTS the kill; the process stays a job member until it has actually exited, so a refused
	// spawn was reporting "nothing started" while the domain still listed a process. Waiting for the
	// exit is what makes the failure mean the containment is as it was — and it is cheap, because this
	// process was created suspended and never ran a single instruction. A wait that does not complete
	// is joined into the error rather than swallowed, because then the domain really is not clear.
	defer func() {
		if err == nil {
			return
		}
		if terr := windows.TerminateProcess(pi.Process, 1); terr != nil {
			err = errors.Join(err, fmt.Errorf("proctree: terminate the refused process: %w", terr))
		} else if ev, werr := windows.WaitForSingleObject(pi.Process, 10_000); werr != nil || ev != uint32(windows.WAIT_OBJECT_0) {
			err = errors.Join(err, fmt.Errorf("proctree: the refused process did not leave the containment domain (wait 0x%08x): %v", ev, werr))
		}
		_ = windows.CloseHandle(pi.Thread)
		_ = windows.CloseHandle(pi.Process)
	}()

	if err = j.confirmProcessContained(pi.Process); err != nil {
		return nil, err
	}
	if _, err = windows.ResumeThread(pi.Thread); err != nil {
		err = fmt.Errorf("proctree: resume contained process: %w", err)
		return nil, err
	}
	if err = windows.CloseHandle(pi.Thread); err != nil {
		err = fmt.Errorf("proctree: close contained thread handle: %w", err)
		return nil, err
	}
	return &ContainedProcess{handle: pi.Process, pid: pi.ProcessId}, nil
}

// makeInheritable turns inheritance on for exactly these handles and returns the undo.
//
// The previous flags are read rather than assumed to be zero. Go creates pipes and opens files
// non-inheritable today, but restoring a value that was never observed is how a handle quietly stays
// inheritable for the rest of the process's life.
func makeInheritable(hs []windows.Handle) (func(), error) {
	prev := make([]uint32, len(hs))
	done := 0
	undo := func() {
		for i := 0; i < done; i++ {
			_ = windows.SetHandleInformation(hs[i], windows.HANDLE_FLAG_INHERIT, prev[i])
		}
	}
	for i, h := range hs {
		var flags uint32
		if err := getHandleInformation(h, &flags); err != nil {
			undo()
			return nil, fmt.Errorf("proctree: read handle inheritance: %w", err)
		}
		prev[i] = flags & windows.HANDLE_FLAG_INHERIT
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			undo()
			return nil, fmt.Errorf("proctree: grant handle inheritance: %w", err)
		}
		done = i + 1
	}
	return undo, nil
}

// envBlockUTF16 renders the frozen environment as the double-NUL-terminated block CreateProcessW wants.
//
// An EMPTY frozen environment must still produce a block — two NULs — and never a nil pointer. A nil
// environment pointer means "inherit the parent's", which is precisely the ambient authority the frozen
// environment exists to eliminate, and it would fail silently: the command would run, with the
// coordinator's variables, against a digest that says otherwise. Appending one terminator to an empty
// list would produce a single NUL, so the empty case is written out rather than falling out of the
// loop — the same shape Go's own syscall package uses.
func envBlockUTF16(s ExecSpec) ([]uint16, error) {
	pairs := envPairs(s)
	if len(pairs) == 0 {
		return []uint16{0, 0}, nil
	}
	var out []uint16
	for _, pair := range pairs {
		u, err := windows.UTF16FromString(string(pair))
		if err != nil {
			return nil, fmt.Errorf("%w: environment entry: %v", ErrSpecInvalid, err)
		}
		// UTF16FromString appends the terminator, which is also the separator this block needs.
		out = append(out, u...)
	}
	return append(out, 0), nil
}
