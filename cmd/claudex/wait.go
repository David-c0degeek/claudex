package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/David-c0degeek/claudex/internal/coordinator"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// waitCmd long-polls the run for the next event relevant to a session and prints it as one line of
// canonical JSON. It reads through the coordinator's aggregate authority (no run lock held), so a
// no-event timeout returns a valid `unchanged` event. Exit codes: 0 success (including a no-event
// timeout), 1 operational (recovery-required / no active run / run switched / client-ahead / I/O /
// cancelled), 2 usage error.
func waitCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "repository root")
	run := fs.String("run", "", "run id (defaults to the repository's active run)")
	session := fs.String("session", "", "session id waiting for events (required)")
	since := fs.Uint64("since", 0, "the last state revision the caller has already seen")
	timeout := fs.Duration("timeout", 30*time.Second, "maximum time to block for an event")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "claudex: wait takes no positional arguments, got %v\n", fs.Args())
		return 2
	}
	if *session == "" {
		fmt.Fprintln(stderr, "claudex: wait requires --session")
		return 2
	}
	runID, code := resolveRunID(*repo, *run, stderr)
	if code != 0 {
		return code
	}

	ev, err := coordinator.Wait(ctx, *repo, runID, *session, *since, *timeout)
	if err != nil {
		return waitError(err, stderr)
	}
	b, err := ev.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "claudex: wait: render: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", b); err != nil {
		fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
		return 1
	}
	return 0
}

// waitError maps a wait failure to its message and exit code. An invalid timeout is a usage error;
// everything else is operational. Recovery-required and run-gone get their own stable phrasings so a
// caller can act without parsing free text.
func waitError(err error, stderr io.Writer) int {
	switch {
	case errors.Is(err, transport.ErrInvalidTimeout):
		fmt.Fprintf(stderr, "claudex: wait: %v\n", err)
		return 2
	case errors.Is(err, coordinator.ErrReadRecoveryRequired):
		fmt.Fprintln(stderr, "claudex: wait: the run is mid-transaction; recovery required before it can be read")
		return 1
	case errors.Is(err, coordinator.ErrRunGone):
		fmt.Fprintln(stderr, "claudex: wait: the requested run is no longer the repository's active run")
		return 1
	default:
		fmt.Fprintf(stderr, "claudex: wait: %v\n", err)
		return 1
	}
}
