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
	"fmt"
	"io"
	"os"

	"github.com/David-c0degeek/claudex/internal/buildinfo"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point: it returns the process exit code and writes
// only to the provided streams.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, buildinfo.Version())
		return 0
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "claudex: unknown command %q\n\n%s\n", args[0], usage)
		return 2
	}
}

const usage = `claudex - pair-programming coordinator for two interactive AI agent terminals

Usage:
  claudex <command>

Commands:
  version   Print version information
  help      Show this help

The attach protocol (attach/pull/submit/wait/status) arrives in a later milestone.`
