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

	// The caller's real handles are NEVER made inheritable. Inheritable DUPLICATES are created for this
	// call and closed immediately after it.
	//
	// The earlier version flipped the originals inheritable for the duration of CreateProcess and put
	// them back afterwards, which left a window in which they were inheritable PROCESS-WIDE. A handle
	// list constrains only the CreateProcess that carries it, so any unrelated concurrent spawn in this
	// program with inheritance enabled could have received the coordinator's real stdout and stderr —
	// and the restore step could fail silently, making the window permanent. Duplicates remove the
	// window entirely rather than shortening it: the originals' inheritance is never touched, so there
	// is nothing to restore and nothing to get wrong.
	originals := []windows.Handle{
		windows.Handle(devNull.Fd()),
		windows.Handle(s.Stdout.Fd()),
		windows.Handle(s.Stderr.Fd()),
	}
	var stdHandles []windows.Handle
	var closeDups func()
	if stdHandles, closeDups, err = inheritableDuplicates(originals); err != nil {
		return nil, err
	}
	defer closeDups()

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
	//
	// It keys on `resumed`, NOT on err. Keying on err was the defect: once the command has started,
	// Spawn can still return an error for a cleanup fault, and the terminate path would then kill a
	// running command and close the very handle the caller was just handed — which is also a
	// double-close, since the caller closes it too.
	resumed := false
	defer func() {
		if resumed {
			return
		}
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

	// RESUMING IS THE IRREVERSIBLE BOUNDARY. Once this returns successfully the configured command has
	// started, and no later failure may unsay that.
	//
	// The earlier version treated a failure to close the thread handle as a Spawn failure, which sent
	// the error defer above through TerminateProcess on a command that might already have executed —
	// reporting `spawn_failed`, a class reserved for a command that could not start, about a command
	// that did. A leaked thread handle is an infrastructure fault in the caller's process; it is
	// returned ALONGSIDE the started command rather than converted into a claim about the command.
	if _, err = windows.ResumeThread(pi.Thread); err != nil {
		err = fmt.Errorf("proctree: resume contained process: %w", err)
		return nil, err
	}
	resumed = true
	started := &ContainedProcess{handle: pi.Process, pid: pi.ProcessId}
	if cerr := closeThreadHandle(pi.Thread); cerr != nil {
		return started, fmt.Errorf("proctree: the command started; closing its thread handle failed: %w", cerr)
	}
	return started, nil
}

// closeThreadHandle is a seam. The boundary it guards — that a started command is never reported as a
// spawn failure — cannot be exercised otherwise, because a CloseHandle on a valid handle does not fail
// on demand, and a boundary that is only asserted in a comment is one nothing prevents from moving.
var closeThreadHandle = windows.CloseHandle

// inheritableDuplicates makes inheritable copies of handles whose originals must stay private.
//
// DuplicateHandle with bInheritHandle set produces a NEW handle in this process that CreateProcess can
// pass on, while the source handle's own flags are untouched. That is the difference that matters: the
// originals are the coordinator's real streams, and any interval in which they are inheritable is an
// interval in which an unrelated spawn elsewhere in the program can receive them.
func inheritableDuplicates(src []windows.Handle) ([]windows.Handle, func(), error) {
	dups := make([]windows.Handle, 0, len(src))
	closeAll := func() {
		for _, h := range dups {
			_ = windows.CloseHandle(h)
		}
	}
	self := windows.CurrentProcess()
	for _, h := range src {
		var dup windows.Handle
		if err := windows.DuplicateHandle(self, h, self, &dup, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("proctree: duplicate a stream handle for inheritance: %w", err)
		}
		dups = append(dups, dup)
	}
	return dups, closeAll, nil
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
