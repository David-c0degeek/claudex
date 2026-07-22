package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/coordinator"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// maxSubmitFile bounds a --file read. The transport layer re-bounds and validates the artifact;
// this only stops an accidental/hostile huge file before it is read into memory.
const maxSubmitFile = 1 << 20

// resolveRunID returns the run id a verb operates on and an exit code (0 = ok). Only a
// malformed CLI id is a USAGE error (2); a failed active-bootstrap binding, no active run, or a
// store/I/O/legacy error is OPERATIONAL (1) per D6 — never collapsed into usage. A discovered or
// explicit id that fails the binding is an error, never a silent fallback.
func resolveRunID(repo, run string, stderr io.Writer) (string, int) {
	if run != "" {
		if !state.IsRunID(run) {
			fmt.Fprintln(stderr, "claudex: --run is not a canonical run id")
			return "", 2 // malformed CLI id = usage
		}
		if _, err := attach.ResolveRun(repo, run); err != nil {
			fmt.Fprintf(stderr, "claudex: --run does not bind to an active run: %v\n", err)
			return "", 1 // binding/store failure = operational
		}
		return run, 0
	}
	id, ok, err := attach.ActiveRunID(repo)
	if err != nil {
		fmt.Fprintf(stderr, "claudex: discover active run: %v\n", err)
		return "", 1 // I/O / legacy refusal = operational
	}
	if !ok {
		fmt.Fprintln(stderr, "claudex: no active run in this repository (pass --run or run attach first)")
		return "", 1 // no active run = operational
	}
	return id, 0
}

// readFileArg reads a required flag's file, bounded and regular-file-checked on the OPENED
// handle (a swap between the stat and the open cannot slip a symlink/FIFO/device through).
func readFileArg(flagName, path string, max int64, stderr io.Writer) ([]byte, bool) {
	if path == "" {
		fmt.Fprintf(stderr, "claudex: %s is required\n", flagName)
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "claudex: %s: %v\n", flagName, err)
		return nil, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		fmt.Fprintf(stderr, "claudex: %s: %v\n", flagName, err)
		return nil, false
	} else if !fi.Mode().IsRegular() {
		fmt.Fprintf(stderr, "claudex: %s must be a regular file\n", flagName)
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		fmt.Fprintf(stderr, "claudex: %s: %v\n", flagName, err)
		return nil, false
	}
	if int64(len(b)) > max {
		fmt.Fprintf(stderr, "claudex: %s exceeds %d bytes\n", flagName, max)
		return nil, false
	}
	return b, true
}

// submitCmd submits an artifact for the caller's turn through the coordinator, then rebuilds the
// mailbox transcript mirror. It emits a schema-valid receipt on stdout. Exit codes: 0 success,
// 1 operational error, 2 usage error.
func submitCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "repository root")
	run := fs.String("run", "", "run id (defaults to the repository's active run)")
	sessionID := fs.String("session-id", "", "the submitting session id")
	file := fs.String("file", "", "path to the artifact JSON to submit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "claudex: submit takes no positional arguments, got %v\n", fs.Args())
		return 2
	}
	if !state.IsSessionID(*sessionID) {
		fmt.Fprintln(stderr, "claudex: submit requires a valid --session-id")
		return 2
	}
	raw, ok := readFileArg("--file", *file, maxSubmitFile, stderr)
	if !ok {
		return 2
	}
	runID, code := resolveRunID(*repo, *run, stderr)
	if code != 0 {
		return code
	}

	rn, err := coordinator.OpenRun(*repo, runID, rand.Reader)
	if err != nil {
		fmt.Fprintf(stderr, "claudex: submit: open run: %v\n", err)
		return 1
	}
	defer rn.Close()

	res, serr := rn.Submit(ctx, *sessionID, raw)
	emitReceipt, warn, doMirror, exit := classifySubmit(res, serr)
	if !emitReceipt {
		// An ordinary receipt-less failure (zero receipt + error).
		fmt.Fprintf(stderr, "claudex: submit: %v\n", warn)
		return exit
	}

	// Rebuild the human-readable mailbox mirror ONLY on a clean acceptance — never while
	// durability is unconfirmed (do not project un-power-safe state); a later replay repairs it.
	var mirrorErr error
	if doMirror {
		mirrorErr = rn.MirrorMailbox()
	}

	// The receipt is authoritative even alongside a durability/reconfirm/release warning: emit it.
	wire, merr := transport.MarshalReceipt(res.Receipt)
	if merr != nil {
		fmt.Fprintf(stderr, "claudex: submit: render receipt: %v\n", merr)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", wire); err != nil {
		fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
		return 1
	}
	if res.Idempotent {
		fmt.Fprintln(stderr, "claudex: submit: idempotent replay (receipt re-confirmed)")
	}
	if warn != nil {
		fmt.Fprintf(stderr, "claudex: submit WARNING: %v\n", warn)
	}
	if mirrorErr != nil {
		fmt.Fprintf(stderr, "claudex: submit WARNING: the acceptance is durable but the mailbox mirror could not be rebuilt: %v\n", mirrorErr)
		return 1
	}
	return exit
}

// classifySubmit maps a submit outcome to the CLI's behavior, so an authoritative receipt
// accompanying a post-commit/durability error is never dropped:
//   - success (nil error): emit the receipt, rebuild the mirror, exit 0 (a ReleaseWarning is a
//     non-fatal warning on an already-committed acceptance);
//   - receipt-bearing error (durability-unconfirmed accept, or a replay whose state re-confirm
//     failed): emit the receipt, surface the error, SKIP the mirror (state is unconfirmed; a
//     replay repairs it), exit 1;
//   - zero-receipt error: an ordinary receipt-less failure, exit 1.
//
// Pure, so the receipt/exit contract is unit-testable against both real result+error shapes.
func classifySubmit(res transport.SubmitResult, serr error) (emitReceipt bool, warn error, doMirror bool, exit int) {
	switch {
	case serr == nil:
		return true, res.ReleaseWarning, true, 0
	case res.Receipt != (state.Receipt{}):
		return true, serr, false, 1
	default:
		return false, serr, false, 1
	}
}
