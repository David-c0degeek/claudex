package proctree

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// dumpSpec builds a spec that runs the reporting fixture with exactly the supplied environment.
func dumpSpec(t *testing.T, extraEnv []EnvVar, extraArgv ...string) ExecSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	argv := [][]byte{[]byte(exe)}
	for _, a := range extraArgv {
		argv = append(argv, []byte(a))
	}
	env := []EnvVar{
		{Name: []byte(helperEnv), Value: []byte("dump")},
		{Name: []byte("SystemRoot"), Value: []byte(os.Getenv("SystemRoot"))},
	}
	env = append(env, extraEnv...)
	return ExecSpec{
		Executable: []byte(exe),
		Argv:       argv,
		Cwd:        []byte(filepath.Clean(os.TempDir())),
		Env:        env,
		Identity:   NameASCIIFold,
	}
}

// TestSpawnedCommandIsAMemberBeforeItRuns.
//
// The membership is asserted from the CHILD's existence rather than from the API having returned
// success: Spawn observes IsProcessInJob while the process is still suspended and terminates it
// otherwise, so a started command that reaches here is one whose containment was seen, not promised.
func TestSpawnedCommandIsAMemberBeforeItRuns(t *testing.T) {
	job := armForTest(t)
	spec := dumpSpec(t, nil)

	out := runDump(t, job, spec)
	if len(out) == 0 {
		t.Fatal("the contained command reported nothing")
	}
	// It ran, which means Spawn observed it inside the job and resumed it. A process that could not be
	// observed there is terminated instead, so reaching output at all is the assertion.
}

// TestSpawnedCommandReceivesTheFrozenArgvExactly. Empty arguments are meaningful and must survive the
// command-line composition that Windows requires and Unix does not.
func TestSpawnedCommandReceivesTheFrozenArgvExactly(t *testing.T) {
	job := armForTest(t)
	spec := dumpSpec(t, nil, "plain", "", "with space", `quote"inside`, `trailing\`)

	out := runDump(t, job, spec)
	var argv []string
	for _, line := range out {
		if rest, ok := strings.CutPrefix(line, "ARGV "); ok {
			if _, after, found := strings.Cut(rest, " "); found {
				argv = append(argv, after)
			} else {
				// An empty argument reports as "ARGV <n> " with nothing after the space.
				argv = append(argv, "")
			}
		}
	}
	want := []string{string(spec.Executable), "plain", "", "with space", `quote"inside`, `trailing\`}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full %q)", i, argv[i], want[i], argv)
		}
	}
}

// TestSpawnedCommandDoesNotInheritTheAmbientEnvironment.
//
// This is the failure that would be silent. A nil environment pointer means "inherit the parent's" to
// CreateProcessW, so a construction slip does not error — the command simply runs with the
// coordinator's variables while the digest in the attempt intent says it ran with the frozen ones.
func TestSpawnedCommandDoesNotInheritTheAmbientEnvironment(t *testing.T) {
	const marker = "CLAUDEX_AMBIENT_MARKER"
	t.Setenv(marker, "leaked")
	if os.Getenv(marker) != "leaked" {
		t.Fatal("the ambient marker is not set; the test would prove nothing")
	}

	job := armForTest(t)
	out := runDump(t, job, dumpSpec(t, nil))
	for _, line := range out {
		if strings.HasPrefix(line, "ENV "+marker+"=") {
			t.Fatalf("the contained command inherited an ambient variable: %q", line)
		}
	}
	// And the frozen ones DID arrive, so the absence above is containment rather than an empty
	// environment produced by a broken block.
	found := false
	for _, line := range out {
		if strings.HasPrefix(line, "ENV "+helperEnv+"=dump") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the frozen environment did not reach the command: %q", out)
	}
}

// TestEnvBlockTerminatesAnEmptyEnvironment. Appending a single terminator to an empty list produces one
// NUL, which is not a block — and the consequence of getting it wrong is the inheritance the previous
// test forbids.
func TestEnvBlockTerminatesAnEmptyEnvironment(t *testing.T) {
	block, err := envBlockUTF16(ExecSpec{
		Executable: []byte(`C:\x.exe`),
		Argv:       [][]byte{[]byte("x")},
		Cwd:        []byte(`C:\`),
		Identity:   NameASCIIFold,
	})
	if err != nil {
		t.Fatalf("envBlockUTF16: %v", err)
	}
	if len(block) != 2 || block[0] != 0 || block[1] != 0 {
		t.Fatalf("empty environment block = %v, want two NULs", block)
	}

	block, err = envBlockUTF16(ExecSpec{
		Executable: []byte(`C:\x.exe`),
		Argv:       [][]byte{[]byte("x")},
		Cwd:        []byte(`C:\`),
		Env:        []EnvVar{{Name: []byte("A"), Value: []byte("1")}},
		Identity:   NameASCIIFold,
	})
	if err != nil {
		t.Fatalf("envBlockUTF16: %v", err)
	}
	// "A=1\0\0"
	if got := len(block); got != 5 || block[3] != 0 || block[4] != 0 {
		t.Fatalf("populated block = %v (len %d), want A=1 with a double terminator", block, got)
	}
}

// TestSpawnRefusesTheLinuxIdentity mirrors the Linux guard. Executing under the wrong identity rule
// would mean the digest bound one comparison rule while the OS applied another.
func TestSpawnRefusesTheLinuxIdentity(t *testing.T) {
	job := armForTest(t)
	spec := dumpSpec(t, nil)
	spec.Identity = NameByteExact

	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	defer outR.Close()
	defer errR.Close()
	if _, err := job.Spawn(CommandSpawn{Spec: spec, Stdout: outW, Stderr: errW}); !errors.Is(err, ErrSpecInvalid) {
		t.Fatalf("err = %v, want ErrSpecInvalid", err)
	}
}

// TestSpawnRefusesResolutionAtExecutionTime. A relative executable or cwd would be resolved against
// whatever directory this process happens to be in, which is the ambient resolution the frozen absolute
// path exists to eliminate.
func TestSpawnRefusesResolutionAtExecutionTime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutet func(*ExecSpec)
	}{
		{"relative executable", func(s *ExecSpec) { s.Executable = []byte(`some\tool.exe`) }},
		{"relative cwd", func(s *ExecSpec) { s.Cwd = []byte(`work`) }},
		{"unclean cwd", func(s *ExecSpec) { s.Cwd = []byte(`C:\work\..\work`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := armForTest(t)
			spec := dumpSpec(t, nil)
			tc.mutet(&spec)

			outR, outW, _ := os.Pipe()
			errR, errW, _ := os.Pipe()
			defer outR.Close()
			defer errR.Close()
			if _, err := job.Spawn(CommandSpawn{Spec: spec, Stdout: outW, Stderr: errW}); !errors.Is(err, ErrSpecInvalid) {
				t.Fatalf("err = %v, want ErrSpecInvalid", err)
			}
		})
	}
}

// TestSpawnFailureReleasesTheStreamEnds. The coordinator is blocked on stream EOF whether or not a
// command ever started, so a refused spawn that kept a write end open would hang the drain forever —
// the same ownership rule the Linux side had to be corrected for.
func TestSpawnFailureReleasesTheStreamEnds(t *testing.T) {
	job := armForTest(t)
	spec := dumpSpec(t, nil)
	spec.Identity = NameByteExact // any refusal will do; this one is cheap and deterministic

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

	if _, err := job.Spawn(CommandSpawn{Spec: spec, Stdout: outW, Stderr: errW}); err == nil {
		t.Fatal("Spawn accepted a spec it should have refused")
	}
	if err := outW.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdout write end was not released by the failed spawn: %v", err)
	}
	if err := errW.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stderr write end was not released by the failed spawn: %v", err)
	}
}

// TestSpawnRestoresHandleInheritance.
//
// The standard handles have to be made inheritable for the duration of the call, and must be put back
// afterwards so they cannot cross into an unrelated CreateProcess elsewhere in the program.
//
// The inheritance flag is cleared FIRST, and that is not tidiness. Measured on this platform,
// os.Pipe returns handles that are ALREADY inheritable — Go's syscall.Pipe creates them with
// InheritHandle set — so a version of this test that trusted the default read 1 before and 1 after and
// could not fail whatever the code did. Starting from a known-off state is what gives the assertion
// something to detect.
func TestSpawnRestoresHandleInheritance(t *testing.T) {
	job := armForTest(t)
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

	if err := windows.SetHandleInformation(windows.Handle(outW.Fd()), windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		t.Fatalf("clear inheritance: %v", err)
	}
	var before uint32
	if err := getHandleInformation(windows.Handle(outW.Fd()), &before); err != nil {
		t.Fatalf("read handle flags: %v", err)
	}
	if before&windows.HANDLE_FLAG_INHERIT != 0 {
		t.Fatal("could not establish a non-inheritable starting state; the test would prove nothing")
	}

	cp, err := job.Spawn(CommandSpawn{Spec: dumpSpec(t, nil), Stdout: outW, Stderr: errW})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	var after uint32
	if err := getHandleInformation(windows.Handle(outW.Fd()), &after); err != nil {
		t.Fatalf("read handle flags after spawn: %v", err)
	}
	if after&windows.HANDLE_FLAG_INHERIT != 0 {
		t.Fatalf("Spawn left the stream handle inheritable (flags 0x%x); it can now cross into an unrelated CreateProcess", after)
	}
	_ = CloseStreamCopies(CommandSpawn{Stdout: outW, Stderr: errW})
}

// TestSpawnedCommandInheritsNothingBeyondItsStreams is the handle-list proof, and it matters more here
// than the equivalent does on Unix.
//
// On Windows bInheritHandles is all-or-nothing: with it TRUE and no PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
// the child inherits EVERY inheritable handle the parent holds — and, as measured above, Go's pipes are
// already inheritable. So the handle list is the only thing standing between a contained command and a
// copy of every pipe in the coordinator.
//
// The canary is a pipe write end the command is never given. If it leaks into the child, the read end
// never sees EOF after the parent drops its copy — which is the exact failure the Linux side had to be
// corrected for, arriving here through a different mechanism.
func TestSpawnedCommandInheritsNothingBeyondItsStreams(t *testing.T) {
	job := armForTest(t)
	canaryR, canaryW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer canaryR.Close()
	// Explicitly inheritable, so the test is about the handle list rather than about the default.
	if err := windows.SetHandleInformation(windows.Handle(canaryW.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		t.Fatalf("mark the canary inheritable: %v", err)
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

	// A long-lived command, so a leaked handle would still be held when the check runs.
	spec := treeSpec(t, exe)
	sp := CommandSpawn{Spec: spec, Stdout: outW, Stderr: errW}
	cp, err := job.Spawn(sp)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })
	if err := CloseStreamCopies(sp); err != nil {
		t.Fatalf("CloseStreamCopies: %v", err)
	}

	// The parent drops its own copy. EOF on the read end now means nobody else holds the write end.
	if err := canaryW.Close(); err != nil {
		t.Fatalf("close the canary write end: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		var b [1]byte
		_, rerr := canaryR.Read(b[:])
		done <- rerr
	}()
	select {
	case rerr := <-done:
		if !errors.Is(rerr, io.EOF) {
			t.Fatalf("canary read = %v, want EOF", rerr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the canary never reached EOF: the contained command inherited a handle it was never given")
	}
}
