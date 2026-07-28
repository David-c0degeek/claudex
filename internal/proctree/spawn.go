package proctree

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CommandSpawn is everything needed to start the test command under containment.
//
// It is portable because the ownership rule it carries is portable: whoever receives these stream
// write ends owns them, and the coordinator on the other side of the pipes cannot see EOF until every
// copy is gone. Only the mechanism that spawns under containment differs by platform.
type CommandSpawn struct {
	// Spec is the frozen execution description. Nothing about the command is taken from anywhere else.
	Spec ExecSpec
	// Stdout and Stderr are the write ends the COORDINATOR created. The spawner passes them to the
	// child and closes its own copies immediately afterwards, so the coordinator can see stream EOF
	// when the command exits.
	Stdout *os.File
	Stderr *os.File
}

// CloseStreamCopies drops the spawner's own handles on the stream write ends.
//
// It runs immediately after a successful start and on every failure path. If any copy survives here,
// the coordinator never sees EOF on those streams when the command exits, so the drain that the
// terminal ordering depends on would hang forever. This is a separate function because it must also
// be called when the spawn fails, where there is no started command to hang it off.
func CloseStreamCopies(s CommandSpawn) error {
	var first error
	for _, f := range []*os.File{s.Stdout, s.Stderr} {
		if f == nil {
			continue
		}
		// Idempotent: the spawn closes these on its own error paths, and the caller closes them after
		// a successful start, so an already-closed handle is an expected state rather than a fault.
		// Anything else is reported.
		if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) && first == nil {
			first = fmt.Errorf("proctree: close stream copy: %w", err)
		}
	}
	return first
}

// validateExecutionBoundaryFor refuses anything that would reintroduce resolution at execution time.
//
// ExecSpec.Validate proves the spec is well formed; it does not prove it is EXECUTABLE as written. A
// relative executable or cwd reaching the OS would be resolved against whatever directory this process
// happens to be in — exactly the ambient-resolution defect the resolved absolute path exists to
// eliminate. The supervisor will separately compare these against the rooted intent, but this is the
// last boundary before the kernel and it refuses independently rather than trusting that an earlier
// check ran.
//
// It lives in the portable file, parameterised by the identity the platform requires, so the Windows
// and Linux guards cannot drift apart. They were written twice once; the second copy is how a rule
// silently stops matching the first.
func validateExecutionBoundaryFor(spec ExecSpec, want NameIdentity) error {
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
	// The identity rule belongs to one platform each. Executing under the wrong one would mean the
	// digest bound one comparison rule while the kernel applied another, so names differing only in
	// case would be one variable to the record and two to the process — or the reverse.
	if spec.Identity != want {
		return fmt.Errorf("%w: environment identity %v is not %v semantics", ErrSpecInvalid, spec.Identity, want)
	}
	return nil
}

// envPairs renders the frozen environment as NAME=VALUE byte slices in canonical order.
//
// Both platforms need exactly this, and neither may take it from anywhere else: the bytes here are the
// ones the digest in the attempt intent was computed over.
func envPairs(s ExecSpec) [][]byte {
	env := s.canonicalEnv()
	out := make([][]byte, 0, len(env))
	for _, e := range env {
		var b bytes.Buffer
		b.Grow(len(e.Name) + 1 + len(e.Value))
		b.Write(e.Name)
		b.WriteByte('=')
		b.Write(e.Value)
		out = append(out, b.Bytes())
	}
	return out
}
