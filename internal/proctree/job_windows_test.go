package proctree

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// attemptID returns a name unique to this test AND this process. The job is unnamed, so nothing
// depends on it being distinctive; it is here so failures identify themselves.
func attemptID(t *testing.T) string {
	t.Helper()
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, t.Name())
	return fmt.Sprintf("%s-%d", clean, os.Getpid())
}

func armForTest(t *testing.T) *ArmedJob {
	t.Helper()
	job, err := ArmContainment(attemptID(t))
	if err != nil {
		t.Fatalf("ArmContainment: %v", err)
	}
	t.Cleanup(func() { _ = job.Close() })
	return job
}

// TestProcThreadAttributeJobListDerivation grounds a constant x/sys/windows does not publish.
//
// PROC_THREAD_ATTRIBUTE_JOB_LIST has to be written by hand, and a transcribed hex literal is exactly
// the kind of thing that is wrong in a way nothing notices until containment silently does not apply.
// The value is built from Windows' own formula, and the SAME formula with the handle-list number must
// reproduce a constant the platform package does publish — so the derivation is checked against an
// independent source rather than against the comment that explains it.
func TestProcThreadAttributeJobListDerivation(t *testing.T) {
	const handleListNumber = 2
	if got := uintptr(handleListNumber | (1 << 17)); got != windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST {
		t.Fatalf("the attribute formula gives 0x%08x for the handle list, but the platform says 0x%08x", got, windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST)
	}
	if procThreadAttributeJobList != 0x0002000D {
		t.Fatalf("job list attribute = 0x%08x, want 0x0002000D", procThreadAttributeJobList)
	}
}

// TestArmContainmentAdoptsTheLimitsItClaims. Requesting kill-on-close is not observing it, and the
// difference is the entire guarantee: if the flag did not take, owner death leaves the tree running
// while everything downstream behaves as though it could not.
func TestArmContainmentAdoptsTheLimitsItClaims(t *testing.T) {
	job := armForTest(t)

	var got windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var retlen uint32
	if err := windows.QueryInformationJobObject(job.handle, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&got)), uint32(unsafe.Sizeof(got)), &retlen); err != nil {
		t.Fatalf("QueryInformationJobObject: %v", err)
	}
	flags := got.BasicLimitInformation.LimitFlags
	if flags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Fatalf("kill-on-close not set (flags 0x%08x)", flags)
	}
	// Breakaway must be DENIED, which on Windows means the flags are simply absent: with either
	// breakaway bit set, a child passing CREATE_BREAKAWAY_FROM_JOB leaves the domain deliberately.
	if escape := flags & (windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK | windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK); escape != 0 {
		t.Fatalf("breakaway is permitted (flags 0x%08x)", flags)
	}
}

// TestConfirmContainmentRefusesAWeakenedJob covers the reason the check runs again before the command
// is resumed rather than only at arming: it is the limits IN FORCE at that instant that contain the
// command, and membership alone would not have noticed.
func TestConfirmContainmentRefusesAWeakenedJob(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags uint32
	}{
		{"kill-on-close cleared", 0},
		{"breakaway permitted", windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK},
		{"silent breakaway permitted", windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := armForTest(t)
			var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
			info.BasicLimitInformation.LimitFlags = tc.flags
			if _, err := windows.SetInformationJobObject(job.handle, windows.JobObjectExtendedLimitInformation,
				uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
				t.Fatalf("weaken the job: %v", err)
			}
			if err := confirmContainmentLimits(job.handle); !errors.Is(err, ErrJobLimitsUnenforced) {
				t.Fatalf("err = %v, want ErrJobLimitsUnenforced", err)
			}
		})
	}
}

// TestUnnamedJobIsUnreachableByName is the direct regression test for the escape that killed the first
// design, and it asserts the platform behaviour rather than trusting the reasoning.
//
// A NAMED job is reopenable by anyone with the creator's authority — which the test command has — so
// the command could hold its own handle and stop kill-on-close from ever firing. This test reproduces
// that on a job of its own, then shows the shipped arming produces an object with no name for such a
// call to reach.
func TestUnnamedJobIsUnreachableByName(t *testing.T) {
	name := fmt.Sprintf(`claudex-escape-probe-%d`, os.Getpid())
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	createJobW := modkernel32.NewProc("CreateJobObjectW")
	openJobW := modkernel32.NewProc("OpenJobObjectW")

	// A named job, exactly as the first design would have created it.
	r0, _, _ := syscall.SyscallN(createJobW.Addr(), 0, uintptr(unsafe.Pointer(namePtr)))
	named := windows.Handle(r0)
	if named == 0 {
		t.Fatal("could not create the named control job")
	}
	defer windows.CloseHandle(named)

	const jobObjectQuery = 0x0004
	r0, _, e1 := syscall.SyscallN(openJobW.Addr(), uintptr(jobObjectQuery), 0, uintptr(unsafe.Pointer(namePtr)))
	second := windows.Handle(r0)
	if second == 0 {
		t.Fatalf("the control case did not reproduce: a named job could not be reopened (%v). "+
			"If this platform genuinely refuses, the unnamed choice is still correct but this test no longer proves why", e1)
	}
	windows.CloseHandle(second)

	// The shipped arming must produce an object the kernel holds NO name for. Asked of the object
	// itself rather than of the code that created it, so giving the job a name again fails here
	// immediately — which a structural claim about the ArmedJob type would not have caught.
	job := armForTest(t)
	name, qerr := kernelObjectName(job.handle)
	if qerr != nil {
		t.Fatalf("query the job object's name: %v", qerr)
	}
	if name != "" {
		t.Fatalf("the containment job is named %q; a name is reopenable by the command's own authority, "+
			"which is the escape this design exists to remove", name)
	}
}

// kernelObjectName asks the object manager what a handle's object is called. An unnamed object reports
// an empty name.
func kernelObjectName(h windows.Handle) (string, error) {
	type unicodeString struct {
		Length        uint16
		MaximumLength uint16
		Buffer        *uint16
	}
	const objectNameInformation = 1
	ntQueryObject := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryObject")

	buf := make([]byte, 2048)
	var retlen uint32
	r0, _, _ := syscall.SyscallN(ntQueryObject.Addr(), uintptr(h), objectNameInformation,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&retlen)))
	if r0 != 0 {
		return "", fmt.Errorf("NtQueryObject: NTSTATUS 0x%x", r0)
	}
	us := (*unicodeString)(unsafe.Pointer(&buf[0]))
	if us.Length == 0 || us.Buffer == nil {
		return "", nil
	}
	return windows.UTF16ToString(unsafe.Slice(us.Buffer, us.Length/2)), nil
}

// TestSpawnRefusesAWeakenedJobBeforeTheCommandRuns pins the pre-resume check at the CALL SITE.
//
// Testing confirmContainmentLimits directly proves the predicate; it does not prove Spawn consults it.
// Here the job is weakened after arming — the case that motivates re-reading the limits rather than
// trusting the ones requested earlier — and the command must be refused with nothing left running.
func TestSpawnRefusesAWeakenedJobBeforeTheCommandRuns(t *testing.T) {
	job := armForTest(t)
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = 0 // kill-on-close dropped
	if _, err := windows.SetInformationJobObject(job.handle, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		t.Fatalf("weaken the job: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer outR.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer errR.Close()

	cp, err := job.Spawn(CommandSpawn{Spec: treeSpec(t, exe), Stdout: outW, Stderr: errW})
	if err == nil {
		_ = cp.Close()
		t.Fatal("Spawn ran a command inside a job that no longer contains anything")
	}
	if !errors.Is(err, ErrJobLimitsUnenforced) {
		t.Fatalf("err = %v, want ErrJobLimitsUnenforced", err)
	}
	// Refused means nothing survives: the process is created suspended and must be terminated rather
	// than left holding the domain, or the job could never drain.
	if pids, perr := job.MemberPIDs(); perr != nil {
		t.Fatalf("MemberPIDs: %v", perr)
	} else if len(pids) != 0 {
		t.Fatalf("a refused spawn left %v in the containment domain", pids)
	}
}

// TestContainmentIsTransitive: a grandchild nobody arranged for is in the domain, and terminating the
// job takes it with everything else.
func TestContainmentIsTransitive(t *testing.T) {
	job := armForTest(t)
	child, _ := startTreeInJob(t, job)

	// The grandchild is spawned by the child with a plain exec.Command and no awareness of any of this.
	// Its membership is the OS enforcing the domain, not the code remembering to.
	members := waitForMembers(t, job, 2)
	found := false
	for _, pid := range members {
		if pid == child.PID() {
			found = true
		}
	}
	if !found {
		t.Fatalf("job membership %v does not include the command leader %d", members, child.PID())
	}

	if err := job.Terminate(1); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	// Termination is asynchronous, so the domain being EMPTY is observed rather than assumed. Returning
	// as soon as Terminate succeeded would be the same defect as treating ECHILD as group-empty.
	if err := job.AwaitEmpty(time.Now().Add(30*time.Second), 10*time.Millisecond); err != nil {
		t.Fatalf("AwaitEmpty: %v", err)
	}
}

// TestOwnerDeathKillsTheContainedTree is the test the design requires, and on Windows it is also the
// proof behind the recovery contract.
//
// No signal is sent to the tree and no cleanup code runs — the owner is killed outright, exactly as a
// crashed coordinator would be. Process exit closes its handles, the last handle on a kill-on-close job
// terminates the members, and that chain is entirely inside the kernel. It is why the Windows recovery
// fact can be owner death itself rather than a published receipt.
func TestOwnerDeathKillsTheContainedTree(t *testing.T) {
	owner, childPID, grandchildPID := startHelperOwner(t, attemptID(t))

	// Handles are opened BEFORE the kill so the process objects survive their own exit. Without them a
	// later "is this pid gone" check could be answered by a recycled pid, which would make the test
	// pass for the wrong reason.
	childH := openForWait(t, childPID)
	defer windows.CloseHandle(childH)
	grandchildH := openForWait(t, grandchildPID)
	defer windows.CloseHandle(grandchildH)

	if err := owner.Process.Kill(); err != nil {
		t.Fatalf("kill the owner: %v", err)
	}
	if _, err := owner.Process.Wait(); err != nil {
		t.Fatalf("reap the owner: %v", err)
	}

	for _, tc := range []struct {
		what string
		h    windows.Handle
	}{{"command leader", childH}, {"grandchild", grandchildH}} {
		ev, err := windows.WaitForSingleObject(tc.h, 30_000)
		if err != nil {
			t.Fatalf("wait for %s: %v", tc.what, err)
		}
		if ev != uint32(windows.WAIT_OBJECT_0) {
			t.Fatalf("the %s outlived the owner that contained it", tc.what)
		}
	}
}

// TestClosingTheJobKillsTheTree is the same containment through the ordinary path: the coordinator is
// alive and simply drops the handle.
func TestClosingTheJobKillsTheTree(t *testing.T) {
	job, err := ArmContainment(attemptID(t))
	if err != nil {
		t.Fatalf("ArmContainment: %v", err)
	}
	child, _ := startTreeInJob(t, job)
	waitForMembers(t, job, 2)

	childH := openForWait(t, int(child.PID()))
	defer windows.CloseHandle(childH)

	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ev, err := windows.WaitForSingleObject(childH, 30_000)
	if err != nil {
		t.Fatalf("wait for the command leader: %v", err)
	}
	if ev != uint32(windows.WAIT_OBJECT_0) {
		t.Fatal("the command survived the closing of the only handle on its kill-on-close job")
	}
}

// openForWait pins a process identity so a later death check cannot be answered by a recycled pid.
func openForWait(t *testing.T, pid int) windows.Handle {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess(%d): %v", pid, err)
	}
	return h
}

// waitForMembers polls until the domain has at least n members, so the assertion is not racing the
// grandchild's creation.
func waitForMembers(t *testing.T, job *ArmedJob, n int) []uint32 {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		pids, err := job.MemberPIDs()
		if err != nil {
			t.Fatalf("MemberPIDs: %v", err)
		}
		if len(pids) >= n {
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("job membership stayed at %v, want at least %d members", pids, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startTreeInJob runs the tree fixture inside job and returns once its grandchild has been announced.
func startTreeInJob(t *testing.T, job *ArmedJob) (*ContainedProcess, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { outR.Close(); errR.Close() })

	sp := CommandSpawn{Spec: treeSpec(t, exe), Stdout: outW, Stderr: errW}
	cp, err := job.Spawn(sp)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })
	if err := CloseStreamCopies(sp); err != nil {
		t.Fatalf("CloseStreamCopies: %v", err)
	}

	got := make(chan int, 1)
	go func() {
		if pid, ok := scanGrandchild(outR); ok {
			got <- pid
		}
	}()
	select {
	case pid := <-got:
		return cp, pid
	case <-time.After(60 * time.Second):
		t.Fatal("the contained command never reported a grandchild")
	}
	return nil, 0
}
