package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/David-c0degeek/claudex/internal/coordinator"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/transport"
)

// pullCmd prints the session's assignment as one line of canonical JSON and mirrors it into that
// session's inbox. It reads the run over the coordinator's lock-free coherent snapshot (no run lock
// held), mints nothing, and mutates no run state: the turn is whichever the preceding transition
// issued, and a read-only turn's evidence packet is RE-VERIFIED, never synthesized.
//
// Exit codes: 0 success, 1 operational (no turn for this session, recovery required, a packet that
// no longer verifies, I/O), 2 usage error. "Not your turn" is an ordinary answer on a healthy run —
// a session polls and waits — so it is an operational exit with a plain message, not a crash.
func pullCmd(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "repository root")
	run := fs.String("run", "", "run id (defaults to the repository's active run)")
	session := fs.String("session", "", "the session id pulling its assignment")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "claudex: pull takes no positional arguments, got %v\n", fs.Args())
		return 2
	}
	if !state.IsSessionID(*session) {
		fmt.Fprintln(stderr, "claudex: pull requires --session sess-<32 lower-hex>")
		return 2
	}
	runID, code := resolveRunID(*repo, *run, stderr)
	if code != 0 {
		return code
	}

	a, err := coordinator.Pull(*repo, runID, *session)
	if err != nil {
		fmt.Fprintf(stderr, "claudex: pull: %s\n", pullMessage(err))
		return 1
	}
	b, err := a.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "claudex: pull: render: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", b); err != nil {
		fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
		return 1
	}
	return 0
}

// pullMessage turns the expected refusals into plain statements of what is true, so a polling
// session can tell "nothing for me yet" from "this run needs attention".
func pullMessage(err error) string {
	switch {
	case errors.Is(err, coordinator.ErrNotThisSessionsTurn):
		return "the active turn belongs to the other session; wait for yours"
	case errors.Is(err, transport.ErrNoActiveTurn):
		return "the run has no active turn to pull"
	case errors.Is(err, coordinator.ErrSessionSuperseded):
		return "this session has been superseded by a replacement; reattach"
	case errors.Is(err, transport.ErrFreshSessionRequired):
		return "the verification turn requires a fresh session; replace this one first"
	case errors.Is(err, coordinator.ErrReadRecoveryRequired):
		return "the run is mid-transaction; recovery required before it can be read"
	case errors.Is(err, coordinator.ErrEvidence):
		return fmt.Sprintf("the turn's review evidence no longer verifies: %v", err)
	default:
		return err.Error()
	}
}
