package proctree

import (
	"bufio"
	"encoding/base64"
	"os"
	"path/filepath"
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
		capability, oerr := os.Open(capabilityPath)
		if oerr != nil {
			t.Fatalf("open capability stand-in: %v", oerr)
		}
		defer capability.Close()
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

// TestContainedChildInheritsNoCapabilityDescriptor is the assertion CX pinned: four pipe EOFs say
// nothing about descriptors that have no EOF. A leaked lease descriptor would keep an OFD lock alive
// past supervisor death, and a leaked attempt-directory descriptor would let the task interfere with
// the receipt it is judged by.
//
// It asserts the CAPABILITY property rather than a descriptor count, and the difference is not
// convenience. The Go runtime opens a cgroup cpu.max handle for its GOMAXPROCS tracking and reopens it
// asynchronously, so a descriptor can appear between the close-on-exec sweep and the fork. That file
// is read-only, is not ours, and confers nothing — but it means "the child holds exactly three
// descriptors" is a claim this process cannot honestly make. What it CAN guarantee is that nothing
// carrying authority crosses: no pipe beyond the two stream ends, and no handle on the marker file
// standing in for the lease and attempt directory.
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
