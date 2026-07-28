package proctree

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The Windows fixtures re-exec the test binary rather than shelling out.
//
// A `cmd /c` fixture would make the tests statements about cmd.exe's process handling as much as about
// the containment, and the owner-death case in particular needs a fixture that arms a REAL job with the
// shipped code — which only a Go process can do.
const (
	helperEnv     = "CLAUDEX_PROCTREE_TEST_HELPER"
	helperAttempt = "CLAUDEX_PROCTREE_TEST_ATTEMPT"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "sleep":
		// A leaf that stays alive until something kills it. Long enough that its survival is never
		// mistaken for the containment having worked.
		time.Sleep(10 * time.Minute)
		os.Exit(3)
	case "tree":
		// Spawns an ORDINARY grandchild with no job awareness at all. That is the point: job membership
		// is inherited, so the grandchild is contained without anyone arranging it, which is what
		// "transitive" means here.
		exe, err := os.Executable()
		if err != nil {
			os.Exit(4)
		}
		gc := exec.Command(exe)
		gc.Env = append(os.Environ(), helperEnv+"=sleep")
		if err := gc.Start(); err != nil {
			os.Exit(5)
		}
		fmt.Printf("GRANDCHILD %d\n", gc.Process.Pid)
		os.Stdout.Sync()
		time.Sleep(10 * time.Minute)
		os.Exit(3)
	case "owner":
		helperOwner()
	case "dump":
		// Reports the command's own view of its argv and environment, so containment can be asserted
		// from the CHILD's side rather than from the parent's intentions.
		fmt.Printf("ARGC %d\n", len(os.Args))
		for i, a := range os.Args {
			fmt.Printf("ARGV %d %s\n", i, a)
		}
		env := os.Environ()
		sort.Strings(env)
		for _, e := range env {
			fmt.Printf("ENV %s\n", e)
		}
		os.Stdout.Sync()
		os.Exit(0)
	case "":
		os.Exit(m.Run())
	default:
		os.Exit(2)
	}
}

// helperOwner is the stand-in for a coordinator: it arms containment, starts a command tree inside it,
// announces what it built, and then does nothing until it is killed.
//
// It exists so the owner-death test can kill a process that genuinely holds the sole job handle. Killing
// the test process itself is not an option, and faking the handle would test the fake.
func helperOwner() {
	job, err := ArmContainment(os.Getenv(helperAttempt))
	if err != nil {
		fmt.Fprintf(os.Stderr, "arm: %v\n", err)
		os.Exit(6)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		os.Exit(7)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		os.Exit(8)
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(9)
	}
	sp := CommandSpawn{Spec: treeSpecFor(exe), Stdout: outW, Stderr: errW}
	child, err := job.Spawn(sp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn: %v\n", err)
		os.Exit(10)
	}
	_ = CloseStreamCopies(sp)
	_ = errR.Close()

	grandchild, ok := scanGrandchild(outR)
	if !ok {
		fmt.Fprintln(os.Stderr, "child never reported a grandchild")
		os.Exit(11)
	}
	fmt.Printf("READY %d %d\n", child.PID(), grandchild)
	os.Stdout.Sync()
	time.Sleep(10 * time.Minute)
	os.Exit(3)
}

// treeSpecFor describes the fixture that spawns an unaware grandchild, with a frozen environment
// carrying only what a Go process needs to start.
func treeSpecFor(exe string) ExecSpec {
	return ExecSpec{
		Executable: []byte(filepath.Clean(exe)),
		Argv:       [][]byte{[]byte(exe)},
		Cwd:        []byte(filepath.Clean(os.TempDir())),
		Env: []EnvVar{
			{Name: []byte(helperEnv), Value: []byte("tree")},
			{Name: []byte("SystemRoot"), Value: []byte(os.Getenv("SystemRoot"))},
		},
		Identity: NameASCIIFold,
	}
}

func treeSpec(t *testing.T, exe string) ExecSpec {
	t.Helper()
	return treeSpecFor(exe)
}

// scanGrandchild reads the fixture's announcement of the process it spawned.
func scanGrandchild(r io.Reader) (int, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var pid int
		if _, err := fmt.Sscanf(strings.TrimRight(sc.Text(), "\r"), "GRANDCHILD %d", &pid); err == nil {
			return pid, true
		}
	}
	return 0, false
}

// startHelperOwner launches the owner fixture and waits for it to announce a live contained tree.
func startHelperOwner(t *testing.T, attemptID string) (owner *exec.Cmd, childPID, grandchildPID int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	owner = exec.Command(exe)
	owner.Env = append(os.Environ(), helperEnv+"=owner", helperAttempt+"="+attemptID)
	stdout, err := owner.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	owner.Stderr = os.Stderr
	if err := owner.Start(); err != nil {
		t.Fatalf("start owner helper: %v", err)
	}
	t.Cleanup(func() {
		_ = owner.Process.Kill()
		_, _ = owner.Process.Wait()
	})

	type ready struct{ child, grandchild int }
	got := make(chan ready, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var r ready
			if _, err := fmt.Sscanf(strings.TrimRight(sc.Text(), "\r"), "READY %d %d", &r.child, &r.grandchild); err == nil {
				got <- r
				return
			}
		}
	}()
	select {
	case r := <-got:
		return owner, r.child, r.grandchild
	case <-time.After(60 * time.Second):
		t.Fatal("owner helper never announced a contained tree")
	}
	return nil, 0, 0
}

// runDump starts the dump fixture inside a job and returns everything it reported about itself.
func runDump(t *testing.T, job *ArmedJob, spec ExecSpec) []string {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer errR.Close()

	sp := CommandSpawn{Spec: spec, Stdout: outW, Stderr: errW}
	cp, err := job.Spawn(sp)
	if err != nil {
		outR.Close()
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })
	if err := CloseStreamCopies(sp); err != nil {
		t.Fatalf("CloseStreamCopies: %v", err)
	}

	lines := make(chan []string, 1)
	go func() {
		var out []string
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			out = append(out, strings.TrimRight(sc.Text(), "\r"))
		}
		outR.Close()
		lines <- out
	}()
	if ok, err := cp.Wait(time.Now().Add(60 * time.Second)); err != nil || !ok {
		t.Fatalf("dump fixture did not exit (ok=%v err=%v)", ok, err)
	}
	select {
	case out := <-lines:
		return out
	case <-time.After(30 * time.Second):
		t.Fatal("dump fixture output never reached EOF")
	}
	return nil
}
