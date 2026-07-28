package proctree

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows containment is a kill-on-close Job Object that the coordinator holds the ONLY handle to, and
// the job is deliberately UNNAMED.
//
// The name was the first design and it was wrong, which a native probe settled rather than an argument.
// A named job is reopenable by anyone with the creator's authority — and the test command runs with
// exactly that authority — so the command could open the attempt-bound name, hold its own handle, and
// prevent kill-on-close from ever firing when the coordinator died. Measured non-elevated on Windows 11:
//
//	plain named job:   second_open=true, and after the creator closed its handle the name still
//	                   resolved; only after the SECOND handle closed did it disappear (error 2).
//	empty DACL ("D:"): reopen with all-access denied, reopen with query denied — but reopen with
//	                   WRITE_DAC SUCCEEDED, because the object manager always grants the owner
//	                   READ_CONTROL and WRITE_DAC regardless of the DACL. Rewriting the DACL then
//	                   restored full access. An ACL is a speed bump here, not a barrier.
//
// Unnamed removes the attack surface instead of guarding it: there is no argument OpenJobObject could
// be given that would reach this object. The handle is never made inheritable and never duplicated, so
// "the coordinator holds the only handle" is structural.
//
// What that costs, stated plainly: the completion fact can no longer be a name lookup, and nothing
// replaces it. Kill-on-close is enforced by the KERNEL when the last handle closes, and process exit
// closes every handle a process held, so the tree really is terminated when the coordinator dies — but
// that is not something a LATER process can read. See RecoveryGuarantee.

// procThreadAttributeJobList is PROC_THREAD_ATTRIBUTE_JOB_LIST, which x/sys/windows does not define.
//
// It is not a transcribed magic number. Windows builds these values as
// ProcThreadAttributeValue(number, thread, input, additive) = number | (thread<<16) | (input<<17) |
// (additive<<18); the job list is number 13, an input, neither thread-scoped nor additive. The same
// formula with number 2 gives 0x00020002, which x/sys DOES publish as PROC_THREAD_ATTRIBUTE_HANDLE_LIST
// — so the derivation is checked against a value the platform package already agrees with, and a test
// pins that check rather than leaving it in this comment.
const procThreadAttributeJobList = 13 | (1 << 17)

var (
	modkernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	procIsProcessInJob            = modkernel32.NewProc("IsProcessInJob")
	procGetHandleInformation      = modkernel32.NewProc("GetHandleInformation")
	procSetInformationJobObject   = modkernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObject = modkernel32.NewProc("QueryInformationJobObject")
)

// setJobInfo and queryJobInfo call the kernel DIRECTLY rather than through the x/sys wrappers, and the
// reason is a correctness rule rather than taste.
//
// Those wrappers take the buffer as a plain `uintptr`, so the `uintptr(unsafe.Pointer(&buf))`
// conversion has to happen at the CALL SITE and then be carried through an ordinary Go function. The
// unsafe.Pointer rules permit that conversion only when it appears IN THE ARGUMENT LIST OF THE SYSCALL
// ITSELF; passing it through an intervening function leaves the runtime with no reason to treat the
// buffer as referenced, and its address is not guaranteed to remain meaningful for the duration.
//
// This is not theoretical. Through the wrapper, SetInformationJobObject returned SUCCESS while the
// limits it had just written read back as 0x00000000 — kill-on-close silently absent, and with it the
// entire Windows containment guarantee. Reproduced 12 times in 20 under `-race`, whose different
// allocation behaviour is what exposes it, and never once in an ordinary build. Neither
// runtime.KeepAlive nor moving the struct to a heap-allocated buffer fixed it; re-issuing the identical
// Set on the SAME handle immediately afterwards succeeded, which is what proved the object was fine and
// the call was not. Making the conversion inline at the syscall is the documented fix, and it is the
// only one that addresses the actual rule being broken.
func setJobInfo(job windows.Handle, class uint32, info unsafe.Pointer, size uint32) error {
	r0, _, e1 := syscall.SyscallN(procSetInformationJobObject.Addr(),
		uintptr(job), uintptr(class), uintptr(info), uintptr(size))
	if r0 == 0 {
		return e1
	}
	return nil
}

func queryJobInfo(job windows.Handle, class uint32, info unsafe.Pointer, size uint32, retlen *uint32) error {
	r0, _, e1 := syscall.SyscallN(procQueryInformationJobObject.Addr(),
		uintptr(job), uintptr(class), uintptr(info), uintptr(size), uintptr(unsafe.Pointer(retlen)))
	if r0 == 0 {
		return e1
	}
	return nil
}

var (
	// ErrJobLimitsUnenforced means the job did not adopt, or no longer holds, the limits containment
	// depends on. Requesting them is not observing them.
	ErrJobLimitsUnenforced = errors.New("proctree: containment job does not hold the required limits")
	// ErrJobNotEmpty means the containment domain still had members at the cleanup deadline.
	ErrJobNotEmpty = errors.New("proctree: containment job did not empty before the cleanup deadline")
	// ErrProcessNotContained means a created process could not be OBSERVED inside its job. The process
	// is terminated while still suspended rather than resumed.
	ErrProcessNotContained = errors.New("proctree: created process is not a member of its containment job")
)

// RecoveryGuarantee is the exact Windows claim, written down so it cannot drift into the stronger one
// it replaced.
//
// Holds, against the command's own authority:
//
//   - Every descendant is a member. Membership is inherited, so a grandchild nobody arranged for is
//     contained without cooperation.
//   - The command cannot leave. Breakaway is not permitted, so CREATE_BREAKAWAY_FROM_JOB fails rather
//     than escaping, and there is no name to reopen. This is the part the named design could not
//     claim: it let the contained command escape using nothing but its own authority.
//   - Owner death is the teardown. The kernel terminates every member when the last handle closes, and
//     process exit closes handles unconditionally.
//
// Does NOT hold:
//
//   - It is not a defence against a same-user adversary attacking the COORDINATOR — duplicating the
//     job handle out of it, injecting code, or terminating it in a way that matters. Nothing
//     unprivileged can defend that, and it is the same class as the Linux setsid escape the design
//     already places outside the authority model.
//   - **Owner death is not a fact RECOVERY can read.** An earlier version of this comment said it was,
//     reasoning that the kernel tears the domain down when the coordinator dies. The teardown does
//     happen; the inference does not follow. The run lease and the job are DIFFERENT handles — the
//     lease is a file lock released when its own handle closes — and Windows specifies no order in
//     which a terminating process's handles are closed. A recovering coordinator can therefore see the
//     lease unheld while job termination is still in flight, and would retry over a domain that is
//     dying rather than dead. So there is no post-crash completion fact on Windows and recovery must
//     BLOCK, exactly as it does on a POSIX platform with no supervisor.
//   - Consequently there is no reap accounting after a crash either: nothing holds a handle with which
//     to enumerate the domain.
//
// The clean-shutdown path is unaffected. A LIVE coordinator terminates the job, observes the domain
// empty through the handle it still holds, and only then releases the lease — that ordering belongs to
// the coordinator and is provable.
const RecoveryGuarantee = "windows: kernel-enforced kill-on-close on an unnamed job while the owner lives; NO readable post-crash fact, so recovery blocks"

// ArmedJob is containment with no command in it yet.
//
// Arming precedes the active CAS, which is what makes the crash rows reachable: any state carrying an
// active attempt is a state in which containment already existed. Before a command is spawned the job
// is an empty domain that costs nothing to destroy.
type ArmedJob struct {
	handle    windows.Handle
	attemptID string
}

// AttemptID is what this containment belongs to. It is carried for diagnostics only — nothing about
// the job's identity is derived from it, which is the point of the object being unnamed.
func (j *ArmedJob) AttemptID() string { return j.attemptID }

// ArmContainment creates the unnamed, kill-on-close job before any process exists.
//
// The limits are REQUESTED and then READ BACK. Requesting kill-on-close is not observing it, and the
// difference is the whole guarantee: if the flag did not take, the coordinator's death would leave the
// command tree running while every later step behaved as though it could not.
func ArmContainment(attemptID string) (*ArmedJob, error) {
	if attemptID == "" {
		return nil, errors.New("proctree: containment must be bound to an attempt")
	}
	// nil name: unnamed. There is then no entry in any object namespace, so no reopen is possible from
	// any session by any authority.
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("proctree: create containment job: %w", err)
	}
	if err := setContainmentLimits(h); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if err := confirmContainmentLimits(h); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &ArmedJob{handle: h, attemptID: attemptID}, nil
}

func setContainmentLimits(h windows.Handle) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	// Kill-on-close is the containment. Breakaway is NOT requested, and that omission is the denial:
	// without JOB_OBJECT_LIMIT_BREAKAWAY_OK a child passing CREATE_BREAKAWAY_FROM_JOB fails to start
	// rather than escaping.
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	err := setJobInfo(h, windows.JobObjectExtendedLimitInformation,
		unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)))
	if err != nil {
		return fmt.Errorf("proctree: set containment limits: %w", err)
	}
	return nil
}

// confirmContainmentLimits reads the effective limits back and refuses anything short of the contract.
//
// Both directions are checked. Kill-on-close must be present, or the coordinator's death does not tear
// the tree down; breakaway must be absent, or a child can leave the domain deliberately. A job that
// answers correctly on one and not the other is not partial containment, it is no containment with a
// convincing appearance.
//
// It runs at arming AND again before any command is allowed to run, because the containment claim is
// about the limits in force at the moment the command starts, not the ones that were requested earlier.
func confirmContainmentLimits(h windows.Handle) error {
	var got windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var retlen uint32
	err := queryJobInfo(h, windows.JobObjectExtendedLimitInformation,
		unsafe.Pointer(&got), uint32(unsafe.Sizeof(got)), &retlen)
	if err != nil {
		return fmt.Errorf("proctree: read back containment limits: %w", err)
	}
	flags := got.BasicLimitInformation.LimitFlags
	if flags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		return fmt.Errorf("%w: kill-on-close is not set (flags 0x%08x)", ErrJobLimitsUnenforced, flags)
	}
	if escape := flags & (windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK | windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK); escape != 0 {
		return fmt.Errorf("%w: breakaway is permitted (flags 0x%08x)", ErrJobLimitsUnenforced, flags)
	}
	return nil
}

// Close releases the sole handle, which is what terminates the members and destroys the object.
//
// This is the same thing that happens when the coordinator process dies, because process exit closes
// every handle it held. That equivalence is the containment: there is no cleanup path that only runs
// when the code remembers to run it.
func (j *ArmedJob) Close() error {
	if j.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(j.handle)
	j.handle = 0
	if err != nil {
		return fmt.Errorf("proctree: close containment job for %q: %w", j.attemptID, err)
	}
	return nil
}

// Terminate kills every member of the domain at once, transitively.
//
// Unlike the Linux path there is no signal, no grace and no escalation, because there is no cooperation
// to request: a job termination is not deliverable to a handler and cannot be ignored.
func (j *ArmedJob) Terminate(exitCode uint32) error {
	if j.handle == 0 {
		return fmt.Errorf("proctree: containment job for %q is closed", j.attemptID)
	}
	if err := windows.TerminateJobObject(j.handle, exitCode); err != nil {
		return fmt.Errorf("proctree: terminate containment job for %q: %w", j.attemptID, err)
	}
	return nil
}

// jobBasicProcessIDList is JOBOBJECT_BASIC_PROCESS_ID_LIST, which x/sys/windows does not define. The
// trailing array is variable length; the declared element is the first of however many were returned.
type jobBasicProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIDsInList  uint32
	ProcessIDList             [1]uintptr
}

// maxJobMembers bounds the growth loop. A test command tree larger than this is not a case to page in
// unboundedly during cleanup.
const maxJobMembers = 4096

// MemberPIDs reports who is currently in the containment domain.
//
// This is the Windows counterpart of the ESRCH probe: the fact that makes "the domain is empty" an
// observation rather than an inference from having asked for a termination. It is available only to a
// LIVE coordinator, because it needs the handle — which is exactly the asymmetry RecoveryGuarantee
// spells out.
func (j *ArmedJob) MemberPIDs() ([]uint32, error) {
	if j.handle == 0 {
		return nil, fmt.Errorf("proctree: containment job for %q is closed", j.attemptID)
	}
	for n := 64; ; n *= 2 {
		// Allocated as []uintptr rather than []byte so the buffer is pointer-aligned by construction;
		// a byte slice would leave the alignment of the ULONG_PTR array to the allocator.
		words := (int(unsafe.Sizeof(jobBasicProcessIDList{})) + (n-1)*int(unsafe.Sizeof(uintptr(0))) + 7) / 8
		buf := make([]uintptr, words)
		var retlen uint32
		err := queryJobInfo(j.handle, windows.JobObjectBasicProcessIdList,
			unsafe.Pointer(&buf[0]), uint32(len(buf)*int(unsafe.Sizeof(uintptr(0)))), &retlen)
		runtime.KeepAlive(buf)
		list := (*jobBasicProcessIDList)(unsafe.Pointer(&buf[0]))
		if err != nil {
			// ERROR_MORE_DATA means the list was truncated, and a truncated membership read as complete
			// would report a populated domain as empty.
			if errors.Is(err, windows.ERROR_MORE_DATA) && n*2 <= maxJobMembers {
				continue
			}
			return nil, fmt.Errorf("proctree: read containment membership for %q: %w", j.attemptID, err)
		}
		count := int(list.NumberOfProcessIDsInList)
		if count > n {
			return nil, fmt.Errorf("proctree: containment job for %q reported %d ids into room for %d", j.attemptID, count, n)
		}
		raw := unsafe.Slice(&list.ProcessIDList[0], count)
		out := make([]uint32, count)
		for i, p := range raw {
			out[i] = uint32(p)
		}
		return out, nil
	}
}

// AwaitEmpty proves the containment domain has drained, or fails without claiming it.
//
// Termination is asynchronous: members are marked for death immediately but remain listed until they
// have actually exited. Returning as soon as Terminate succeeded would be the same defect as treating
// ECHILD as group-empty on Linux — a completion claimed one step before the fact.
func (j *ArmedJob) AwaitEmpty(deadline time.Time, poll time.Duration) error {
	if poll <= 0 {
		return fmt.Errorf("proctree: containment drain poll must be positive, got %v", poll)
	}
	for {
		pids, err := j.MemberPIDs()
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%w: %q still has %d members", ErrJobNotEmpty, j.attemptID, len(pids))
		}
		// Capped so the stated bound does not depend on the poll interval dividing evenly into it.
		nap := poll
		if remaining < nap {
			nap = remaining
		}
		time.Sleep(nap)
	}
}

// confirmProcessContained proves the WHOLE containment claim for a process that has not run yet.
//
// Membership alone is not the claim. The guarantee also rests on kill-on-close being in force and both
// breakaway flags being absent, and those are properties of the job at THIS instant rather than at
// arming — so they are re-read here rather than assumed to have survived. Any failure leaves the
// caller to terminate a process that is still suspended, which is the only moment at which refusing
// costs nothing.
func (j *ArmedJob) confirmProcessContained(process windows.Handle) error {
	contained, err := isProcessInJob(process, j.handle)
	if err != nil {
		return err
	}
	if !contained {
		return fmt.Errorf("%w: attempt %q", ErrProcessNotContained, j.attemptID)
	}
	return confirmContainmentLimits(j.handle)
}

// isProcessInJob observes membership. x/sys/windows does not wrap it.
func isProcessInJob(process, job windows.Handle) (bool, error) {
	var result int32
	r0, _, e1 := syscall.SyscallN(procIsProcessInJob.Addr(), uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&result)))
	if r0 == 0 {
		return false, fmt.Errorf("proctree: IsProcessInJob: %w", e1)
	}
	return result != 0, nil
}

// getHandleInformation reads a handle's flags. x/sys/windows wraps the setter but not the getter, and
// restoring a value that was never read is how a handle quietly stays inheritable for the rest of the
// process's life.
func getHandleInformation(h windows.Handle, flags *uint32) error {
	r0, _, e1 := syscall.SyscallN(procGetHandleInformation.Addr(), uintptr(h), uintptr(unsafe.Pointer(flags)))
	if r0 == 0 {
		return e1
	}
	return nil
}
