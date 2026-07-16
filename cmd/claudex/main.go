// Command claudex coordinates two interactive AI agent terminals (Claude Code
// and Codex) pair-programming on one plan and one implementation, converging by
// agreement over a durable file protocol.
//
// This is the greenfield Go implementation. The attach protocol
// (attach/pull/submit/wait/status) and its coordinator arrive with later
// milestones; today the binary exposes only version and help so the module has
// a real, wired entry point rather than a hollow command surface.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/David-c0degeek/claudex/internal/buildinfo"
)

func main() {
	// A signal-cancelled context is threaded through run now so the long-lived
	// commands added later (wait, the mechanical test gate) do not force a
	// signature rewrite.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point: it returns the process exit code and writes
// only to the provided streams. Exit codes: 0 success, 1 operational error
// (e.g. a failed write to stdout), 2 usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	_ = ctx // reserved for the wait/test-gate commands in later milestones

	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		if len(rest) > 0 {
			return usageError(stderr, cmd, rest)
		}
		if _, err := fmt.Fprintln(stdout, buildinfo.Version()); err != nil {
			fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
			return 1
		}
		return 0
	case "help", "-h", "--help":
		if len(rest) > 0 {
			return usageError(stderr, cmd, rest)
		}
		if _, err := fmt.Fprintln(stdout, usage); err != nil {
			fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "claudex: unknown command %q\n\n%s\n", cmd, usage)
		return 2
	}
}

// usageError reports a command invoked with unexpected arguments.
func usageError(stderr io.Writer, cmd string, extra []string) int {
	fmt.Fprintf(stderr, "claudex: %q takes no arguments, got %v\n", cmd, extra)
	return 2
}

const usage = `claudex - pair-programming coordinator for two interactive AI agent terminals

Usage:
  claudex <command>

Commands:
  version   Print version information
  help      Show this help

The attach protocol (attach/pull/submit/wait/status) arrives in a later milestone.`
