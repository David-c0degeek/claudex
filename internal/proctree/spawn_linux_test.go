package proctree

import (
	"bufio"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type childState struct {
	fds  []string
	args [][]byte
	env  [][]byte
	pgid int
}

// openInheritable opens a path and CLEARS close-on-exec, then proves it is clear.
//
// Go's os.Open sets FD_CLOEXEC on Linux, so a descriptor merely opened here can never cross exec —
// a "leak marker" created that way is pre-arranged to pass. That silently invalidated the mutation
// evidence for the descriptor sweep: removing the sweep still could not leak this marker, so whatever
// the mutation detected was an incidental ambient descriptor and would vanish in a clean environment.
// The flag is therefore cleared explicitly and ASSERTED clear, so the fixture is hostile by
// construction rather than by assumption.
func openInheritable(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_SETFD, 0); errno != 0 {
		t.Fatalf("clear FD_CLOEXEC: %v", errno)
	}
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatalf("read FD_CLOEXEC: %v", errno)
	}
	if flags&syscall.FD_CLOEXEC != 0 {
		t.Fatal("the marker still has FD_CLOEXEC set; it cannot test the descriptor sweep")
	}
	return f
}

// runContained spawns the dumper under SpawnContained and reads back what the CHILD actually holds.
// Asserting from the child's point of view is the whole point: the parent's intentions are not
// evidence about what was inherited.
func runContained(t *testing.T, extraEnv []EnvVar, extraArgs [][]byte, capabilityPath string) childState {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	defer outR.Close()
	defer errR.Close()

	// A capability descriptor the child must NOT inherit, standing in for the lease and attempt
	// directory handles the supervisor holds for real. It is opened WITHOUT close-on-exec on purpose:
	// the point is that the spawn closes the descriptor set itself rather than relying on every fd
	// having been created correctly somewhere else.
	if capabilityPath != "" {
		_ = openInheritable(t, capabilityPath)
	}

	argv := append([][]byte{[]byte(exe)}, extraArgs...)
	env := append([]EnvVar{{Name: []byte(helperEnv), Value: []byte("dump")}}, extraEnv...)
	spec := ExecSpec{
		Executable: []byte(exe),
		Argv:       argv,
		Cwd:        []byte(t.TempDir()),
		Env:        env,
		Identity:   NameByteExact,
	}
	s := CommandSpawn{Spec: spec, Stdout: outW, Stderr: errW}
	cmd, err := SpawnContained(s)
	if err != nil {
		t.Fatalf("SpawnContained: %v", err)
	}
	if err := CloseStreamCopies(s); err != nil {
		t.Fatalf("CloseStreamCopies: %v", err)
	}

	pgid := cmd.Process.Pid
	r, err := NewReaper(pgid)
	if err != nil {
		t.Fatalf("NewReaper: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	var st childState
	sc := bufio.NewScanner(outR)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "FDS "):
			st.fds = strings.Split(strings.TrimPrefix(line, "FDS "), ",")
		case strings.HasPrefix(line, "ARG "):
			// SplitN, not Fields: the base64 of an empty argument is the empty string, so the line
			// has a trailing space and no third field. Fields would drop it and index out of range —
			// which is exactly the intentionally empty argument this test exists to prove survives.
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 3 {
				parts = append(parts, "")
			}
			b, derr := base64.StdEncoding.DecodeString(parts[2])
			if derr != nil {
				t.Fatalf("decode arg %q: %v", parts[2], derr)
			}
			st.args = append(st.args, b)
		case strings.HasPrefix(line, "ENV "):
			b, derr := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "ENV "))
			if derr != nil {
				t.Fatalf("decode env: %v", derr)
			}
			st.env = append(st.env, b)
		case strings.HasPrefix(line, "PGID "):
			st.pgid, _ = strconv.Atoi(strings.TrimPrefix(line, "PGID "))
		case strings.HasPrefix(line, "FDERR"):
			t.Fatalf("child could not list its descriptors: %s", line)
		}
	}

	if _, err := r.Await(time.Now().Add(10*time.Second), 50*time.Millisecond); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if _, err := r.Teardown(fastPolicy); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	return st
}

// TestContainedChildInheritsNoCapabilityDescriptor is the CAPABILITY-specific half, and it does not
// replace the exact-set assertion above.
//
// It runs against the Go dumper, whose own runtime opens descriptors after exec, so no count is
// meaningful here — that is what TestContainedChildInheritsExactlyStdioFDs uses a non-Go fixture for.
// What this adds is the class-specific check over a process that really is the supervisor's own
// binary re-executed: no pipe beyond the two stream ends (ctrl, stat, spec and the streams are all
// pipes), and no handle on the marker standing in for the lease and attempt directory.
func TestContainedChildInheritsNoCapabilityDescriptor(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "capability-marker")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	st := runContained(t, nil, nil, marker)

	if len(st.fds) < 3 {
		t.Fatalf("child holds %d descriptors (%v), want at least stdin/stdout/stderr", len(st.fds), st.fds)
	}
	for i, want := range []string{"0=", "1=", "2="} {
		if !strings.HasPrefix(st.fds[i], want) {
			t.Fatalf("descriptor %d is %q, want one starting %q", i, st.fds[i], want)
		}
	}
	// stdin must be the null device, not an inherited terminal: a command that blocks reading stdin
	// would hang to the timeout and then report a failure that says nothing about the code.
	if !strings.HasSuffix(st.fds[0], NullDevice) {
		t.Fatalf("stdin is %q, want %s", st.fds[0], NullDevice)
	}
	for _, fd := range st.fds[1:3] {
		if !strings.Contains(fd, "pipe:") {
			t.Fatalf("stream descriptor %q is not a pipe", fd)
		}
	}

	// The guarantee: nothing with authority crossed.
	for _, fd := range st.fds[3:] {
		if strings.Contains(fd, "pipe:") {
			t.Fatalf("a pipe leaked into the child: %q — ctrl, stat, spec and the stream ends are all pipes", fd)
		}
		if strings.Contains(fd, marker) {
			t.Fatalf("the capability marker leaked into the child: %q", fd)
		}
	}
}

func TestContainedChildLeadsItsOwnGroup(t *testing.T) {
	st := runContained(t, nil, nil, "")
	if st.pgid == 0 {
		t.Fatal("child reported no process group")
	}
	if st.pgid == syscall.Getpgrp() {
		t.Fatal("child shares the supervisor's group; a group kill would destroy the only reaper")
	}
}

// TestContainedChildEnvironmentIsExactlyTheSpec proves the obligation the execution spec exists for:
// the child's environment and argv equal what was authorized, byte for byte, including a value that
// is not valid UTF-8 and an intentionally empty argument — and including the ABSENCE of everything
// the supervisor itself carries.
func TestContainedChildEnvironmentIsExactlyTheSpec(t *testing.T) {
	odd := EnvVar{Name: []byte("ODDVAL"), Value: invalidUTF8}
	empty := []byte("")
	st := runContained(t, []EnvVar{odd}, [][]byte{empty, []byte("tail")}, "")

	var got []string
	for _, e := range st.env {
		got = append(got, string(e))
	}
	joined := strings.Join(got, "\x00")
	if !strings.Contains(joined, "ODDVAL="+string(invalidUTF8)) {
		t.Fatalf("invalid-UTF-8 value did not survive into the child: %q", got)
	}
	// The supervisor's own environment must not leak: the test process has variables the spec never
	// authorized, and exactly two names were authorized here.
	if len(st.env) != 2 {
		t.Fatalf("child environment has %d entries (%q), want only the two the spec authorized", len(st.env), got)
	}

	if len(st.args) != 3 {
		t.Fatalf("child argv has %d elements, want 3", len(st.args))
	}
	if len(st.args[1]) != 0 {
		t.Fatalf("the intentionally empty argument was dropped: %q", st.args)
	}
	if string(st.args[2]) != "tail" {
		t.Fatalf("argv[2] = %q, want tail", st.args[2])
	}
}

// TestSpawnRefusesAnInvalidSpec: the spec is validated before anything is started, so a bad policy
// cannot produce a half-started attempt.
func TestSpawnRefusesAnInvalidSpec(t *testing.T) {
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	defer outR.Close()
	defer errR.Close()
	bad := ExecSpec{Executable: []byte("/bin/true"), Cwd: []byte("/"), Identity: NameByteExact}
	if _, err := SpawnContained(CommandSpawn{Spec: bad, Stdout: outW, Stderr: errW}); err == nil {
		t.Fatal("SpawnContained started a command from a spec with no argv")
	}
}

func TestSpawnRequiresStreamEnds(t *testing.T) {
	spec := ExecSpec{
		Executable: []byte("/bin/true"),
		Argv:       [][]byte{[]byte("true")},
		Cwd:        []byte("/"),
		Identity:   NameByteExact,
	}
	if _, err := SpawnContained(CommandSpawn{Spec: spec}); err == nil {
		t.Fatal("SpawnContained accepted a spawn with no stream write ends")
	}
}

// awaitChildAnnouncement blocks until the child has written its first byte.
//
// That byte is the only available proof that the process is past execve AND past the dynamic loader,
// which is what makes the descriptor listing that follows a statement about INHERITANCE rather than a
// snapshot of whatever the loader happened to be holding.
func awaitChildAnnouncement(t *testing.T, r *os.File) {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var one [1]byte
	if _, err := io.ReadFull(r, one[:]); err != nil {
		t.Fatalf("the child never announced itself: %v", err)
	}
	if err := r.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
}

// childFDs reads a live child's descriptor table from the PARENT, so the listing is not perturbed by
// the act of listing it.
func childFDs(t *testing.T, pid int) []string {
	t.Helper()
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		target, lerr := os.Readlink(dir + "/" + e.Name())
		if lerr != nil {
			continue
		}
		out = append(out, e.Name()+"="+target)
	}
	sort.Strings(out)
	return out
}

// TestContainedChildInheritsExactlyStdioFDs is the exact-set assertion the design pins.
//
// The Go dumper cannot support it: its own runtime opens descriptors after exec, and one of them
// (a cgroup cpu.max handle) is reopened asynchronously, so any count would be measuring the runtime
// rather than the containment. A non-Go fixture has no such behaviour, so /bin/sleep is spawned and
// its table is read FROM THE PARENT while it blocks. That distinguishes inherited descriptors from
// child-created ones, which is the property actually under test.
//
// An earlier version of this test rejected only extras containing "pipe:" or a marker path — which
// would have accepted a leaked socket, git directory handle, secret file or memfd. Narrowing an
// assertion because the fixture could not support it produced a predicate that no longer said what
// the design requires.
func TestContainedChildInheritsExactlyStdioFDs(t *testing.T) {
	// The child ANNOUNCES that it is running, and the listing below waits for that announcement.
	//
	// Enumerating straight after the spawn raced the dynamic loader: between execve and main, ld.so has
	// the process's shared objects OPEN, so a listing taken in that window sees libc as a fourth
	// descriptor and the test fails claiming a leak that does not exist. It passed in isolation and
	// failed under a loaded full-suite run, which is the worst possible failure mode - a gate nobody can
	// trust and a defect nobody can reproduce on demand.
	//
	// The fix is a FACT rather than a delay: the child writes a byte, the test reads it, and only a
	// process that has finished loading and reached its own code can have written it.
	//
	// ORDER: background the sleeper, THEN announce, THEN block in the shell's own `wait` builtin.
	//
	// Every part of that is load-bearing. A plain trailing command would let the shell take the usual
	// last-command optimisation and REPLACE ITSELF with it - a second execve after the announcement,
	// reopening the very window this is meant to close - so the sleeper is backgrounded, which forces a
	// fork, and `wait` is a builtin. And the announcement comes AFTER the fork rather than before it:
	// announcing first would only move the race, since the shell still had to create the background
	// child and could open descriptors transiently while doing so. Receipt of the byte has to mean that
	// BOTH the loading and the child setup are behind the observation point, or it proves nothing about
	// the listing that follows.
	//
	// HONESTY ABOUT THE EVIDENCE: the original failure was observed once, under a loaded full-suite run,
	// and 200 repeats of the old shape on a warm cache did not reproduce it. This fix is therefore
	// justified by the argument above rather than by a demonstrated red-to-green flip, and removing the
	// wait does not reliably turn the test red.
	const shellBin = "/bin/sh"
	if _, err := os.Stat(shellBin); err != nil {
		t.Skipf("%s unavailable: %v", shellBin, err)
	}
	marker := filepath.Join(t.TempDir(), "capability-marker")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	// Held with close-on-exec explicitly CLEARED, so the spawn's own sweep is the only thing that can
	// stop it from reaching the child.
	_ = openInheritable(t, marker)

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	defer outR.Close()
	defer errR.Close()

	spec := ExecSpec{
		Executable: []byte(shellBin),
		Argv:       [][]byte{[]byte("sh"), []byte("-c"), []byte("sleep 30 & echo r; wait")},
		Cwd:        []byte(t.TempDir()),
		Env:        []EnvVar{{Name: []byte("PATH"), Value: []byte("/usr/bin:/bin")}},
		Identity:   NameByteExact,
	}
	sp := CommandSpawn{Spec: spec, Stdout: outW, Stderr: errW}
	cmd, err := SpawnContained(sp)
	if err != nil {
		t.Fatalf("SpawnContained: %v", err)
	}
	if err := CloseStreamCopies(sp); err != nil {
		t.Fatalf("CloseStreamCopies: %v", err)
	}
	pgid := cmd.Process.Pid
	r, err := NewReaper(pgid)
	if err != nil {
		t.Fatalf("NewReaper: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_, _ = ReapGroup(pgid, fastPolicy)
	})

	awaitChildAnnouncement(t, outR)

	fds := childFDs(t, cmd.Process.Pid)
	if len(fds) != 3 {
		t.Fatalf("child holds %d descriptors (%v), want exactly stdin/stdout/stderr", len(fds), fds)
	}
	if !strings.HasSuffix(fds[0], NullDevice) {
		t.Fatalf("stdin is %q, want %s", fds[0], NullDevice)
	}
	for _, fd := range fds[1:] {
		if !strings.Contains(fd, "pipe:") {
			t.Fatalf("stream descriptor %q is not a pipe", fd)
		}
	}
}

// TestSpawnClosesStreamCopiesOnFailure. Once SpawnContained accepts the handles it owns them, so a
// failed spawn must still let the coordinator see EOF. Leaving them open is the terminal-ordering
// deadlock the descriptor table forbids: the drain would wait forever for a command that never ran.
func TestSpawnClosesStreamCopiesOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec ExecSpec
	}{
		{"validation failure", ExecSpec{
			Executable: []byte("/bin/true"),
			Cwd:        []byte("/"),
			Identity:   NameByteExact,
			// no argv
		}},
		{"start failure", ExecSpec{
			Executable: []byte("/nonexistent/definitely-not-here"),
			Argv:       [][]byte{[]byte("x")},
			Cwd:        []byte("/"),
			Identity:   NameByteExact,
		}},
		{"execution boundary failure", ExecSpec{
			Executable: []byte("relative/path"),
			Argv:       [][]byte{[]byte("x")},
			Cwd:        []byte("/"),
			Identity:   NameByteExact,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outR, outW, err := os.Pipe()
			if err != nil {
				t.Fatalf("stdout pipe: %v", err)
			}
			defer outR.Close()
			errR, errW, err := os.Pipe()
			if err != nil {
				t.Fatalf("stderr pipe: %v", err)
			}
			defer errR.Close()

			if _, err := SpawnContained(CommandSpawn{Spec: tc.spec, Stdout: outW, Stderr: errW}); err == nil {
				t.Fatal("SpawnContained accepted a spawn it should have refused")
			}
			// Both reads must reach EOF promptly. If the write copy survived, they block forever.
			for name, r := range map[string]*os.File{"stdout": outR, "stderr": errR} {
				if derr := r.SetReadDeadline(time.Now().Add(2 * time.Second)); derr != nil {
					t.Fatalf("SetReadDeadline: %v", derr)
				}
				buf := make([]byte, 1)
				if _, rerr := r.Read(buf); rerr != io.EOF {
					t.Fatalf("%s: read err = %v, want io.EOF — SpawnContained returned with the write copy still open", name, rerr)
				}
			}
		})
	}
}

// TestSpawnRefusesNonAbsoluteOrUnclean: a relative or unclean path would be resolved against whatever
// directory this process happens to be in, which is the ambient resolution the resolved absolute path
// exists to eliminate.
func TestSpawnRefusesNonAbsoluteOrUnclean(t *testing.T) {
	base := ExecSpec{
		Executable: []byte("/bin/true"),
		Argv:       [][]byte{[]byte("true")},
		Cwd:        []byte("/"),
		Identity:   NameByteExact,
	}
	for _, tc := range []struct {
		name  string
		mutet func(*ExecSpec)
	}{
		{"relative executable", func(s *ExecSpec) { s.Executable = []byte("bin/true") }},
		{"unclean executable", func(s *ExecSpec) { s.Executable = []byte("/bin/../bin/true") }},
		{"relative cwd", func(s *ExecSpec) { s.Cwd = []byte("work") }},
		{"unclean cwd", func(s *ExecSpec) { s.Cwd = []byte("/tmp/../tmp") }},
		{"windows identity on linux", func(s *ExecSpec) { s.Identity = NameASCIIFold }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			tc.mutet(&spec)
			if err := validateExecutionBoundary(spec); err == nil {
				t.Fatal("the execution boundary accepted a spec that reintroduces ambient resolution")
			}
		})
	}
	if err := validateExecutionBoundary(base); err != nil {
		t.Fatalf("a valid spec was refused: %v", err)
	}
}

// TestSpawnClosesTheProvidedHalfWhenTheOtherIsMissing. Ownership begins when the handles arrive, not
// when they are found acceptable: rejecting a malformed call while keeping one write end open is the
// same no-EOF-forever deadlock, reached through the check meant to prevent bad input.
func TestSpawnClosesTheProvidedHalfWhenTheOtherIsMissing(t *testing.T) {
	spec := ExecSpec{
		Executable: []byte("/bin/true"),
		Argv:       [][]byte{[]byte("true")},
		Cwd:        []byte("/"),
		Identity:   NameByteExact,
	}
	for _, tc := range []struct {
		name    string
		useOut  bool
		useErrS bool
	}{
		{"stdout provided, stderr missing", true, false},
		{"stderr provided, stdout missing", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			defer r.Close()
			s := CommandSpawn{Spec: spec}
			if tc.useOut {
				s.Stdout = w
			}
			if tc.useErrS {
				s.Stderr = w
			}
			if _, err := SpawnContained(s); err == nil {
				t.Fatal("SpawnContained accepted a half-supplied stream pair")
			}
			if derr := r.SetReadDeadline(time.Now().Add(2 * time.Second)); derr != nil {
				t.Fatalf("SetReadDeadline: %v", derr)
			}
			buf := make([]byte, 1)
			if _, rerr := r.Read(buf); rerr != io.EOF {
				t.Fatalf("read err = %v, want io.EOF — the provided write end was left open", rerr)
			}
		})
	}
}

// TestSpawnCallsTheExecutionBoundary proves the CALL SITE, not just the predicate.
//
// TestSpawnRefusesNonAbsoluteOrUnclean invokes validateExecutionBoundary directly, so deleting the
// call from SpawnContained left it green — a sound mutation harness caught that. The gap matters
// because a relative executable does not merely fail: exec resolves it against Dir, so `true` with a
// cwd of /bin starts /bin/true successfully. That is the ambient resolution the resolved absolute
// path exists to eliminate, and without the call site it would silently work.
func TestSpawnCallsTheExecutionBoundary(t *testing.T) {
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skipf("/bin/true unavailable: %v", err)
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

	// Relative executable that WOULD resolve and run against this cwd.
	spec := ExecSpec{
		Executable: []byte("true"),
		Argv:       [][]byte{[]byte("true")},
		Cwd:        []byte("/bin"),
		Env:        []EnvVar{{Name: []byte("PATH"), Value: []byte("/bin")}},
		Identity:   NameByteExact,
	}
	cmd, err := SpawnContained(CommandSpawn{Spec: spec, Stdout: outW, Stderr: errW})
	if err == nil {
		if cmd != nil && cmd.Process != nil {
			_, _ = ReapGroup(cmd.Process.Pid, fastPolicy)
		}
		t.Fatal("SpawnContained ran a relative executable resolved against its cwd")
	}
	if !errors.Is(err, ErrSpecInvalid) {
		t.Fatalf("err = %v, want ErrSpecInvalid", err)
	}
}
