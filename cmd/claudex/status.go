package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/David-c0degeek/claudex/internal/coordinator"
)

// statusCmd prints the run's status projection as one line of canonical JSON. It reads the run
// over the coordinator's lock-free coherent snapshot (no run lock held). Exit codes: 0 success,
// 1 operational (recovery-required / no active run / I/O), 2 usage error.
func statusCmd(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "repository root")
	run := fs.String("run", "", "run id (defaults to the repository's active run)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "claudex: status takes no positional arguments, got %v\n", fs.Args())
		return 2
	}
	runID, code := resolveRunID(*repo, *run, stderr)
	if code != 0 {
		return code
	}

	report, err := coordinator.Status(*repo, runID)
	if err != nil {
		if errors.Is(err, coordinator.ErrReadRecoveryRequired) {
			fmt.Fprintln(stderr, "claudex: status: the run is mid-transaction; recovery required before it can be read")
		} else {
			fmt.Fprintf(stderr, "claudex: status: %v\n", err)
		}
		return 1
	}
	b, err := report.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "claudex: status: render: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", b); err != nil {
		fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
		return 1
	}
	return 0
}
