// Command claudex coordinates two interactive AI agent terminals (Claude Code
// and Codex) pair-programming on one plan and one implementation, converging by
// agreement over a durable file protocol.
//
// This is the greenfield Go implementation. The attach protocol —
// attach/pull/submit/wait/status — is wired over the coordinator, alongside the
// read-only inspector for pre-pivot Python runs. Human gates and the operator
// surface arrive with subject 05.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/David-c0degeek/claudex/internal/buildinfo"
	"github.com/David-c0degeek/claudex/internal/legacy"
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
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	cmd, rest := args[0], args[1:]
	if canonical, ok := aliases[cmd]; ok {
		cmd = canonical
	}
	h, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(stderr, "claudex: unknown command %q\n\n%s\n", cmd, usage)
		return 2
	}
	return h(ctx, rest, stdout, stderr)
}

// handler is one dispatchable command.
type handler func(ctx context.Context, args []string, stdout, stderr io.Writer) int

// commands is the COMPLETE dispatch table — the single source of what this binary can do. It is a
// table rather than a switch so the shipped command surface is enumerable: the 03.9 invariant is
// that attach is the sole run bootstrap and nothing else exists, and that can only be asserted
// against the actual routed set. A switch can be extended without any test noticing.
var commands = map[string]handler{
	"version": versionCmd,
	"help":    helpCmd,
	"attach":  attachCmd,
	"pull":    pullCmd,
	"submit":  submitCmd,
	"status":  statusCmd,
	"wait":    waitCmd,
	"inspect-legacy": func(_ context.Context, args []string, stdout, stderr io.Writer) int {
		return inspectLegacy(args, stdout, stderr)
	},
}

// aliases are the conventional flag spellings of the two informational commands. They route to the
// same handlers and are deliberately NOT part of the command surface.
var aliases = map[string]string{
	"--version": "version", "-v": "version",
	"-h": "help", "--help": "help",
}

func versionCmd(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return usageError(stderr, "version", args)
	}
	if _, err := fmt.Fprintln(stdout, buildinfo.Version()); err != nil {
		fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
		return 1
	}
	return 0
}

func helpCmd(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return usageError(stderr, "help", args)
	}
	if _, err := fmt.Fprintln(stdout, usage); err != nil {
		fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
		return 1
	}
	return 0
}

// usageError reports a command invoked with unexpected arguments.
func usageError(stderr io.Writer, cmd string, extra []string) int {
	fmt.Fprintf(stderr, "claudex: %q takes no arguments, got %v\n", cmd, extra)
	return 2
}

// inspectLegacy reads a pre-pivot Python run read-only and prints a redacted
// summary plus the refuse-to-resume remediation. The path may be a run
// directory (its state.json is located via the same CheckRunDir guard the
// attach bootstrap uses) or the state.json file directly. It never mutates
// anything. Exit codes: 0 printed, 1 read/render error, 2 usage.
func inspectLegacy(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "claudex: inspect-legacy takes exactly one path, got %v\n", args)
		return 2
	}
	path := args[0]
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		var lre *legacy.LegacyRunError
		switch err := legacy.CheckRunDir(path); {
		case errors.As(err, &lre):
			path = lre.StatePath // it is a legacy run; inspect its state.json
		case err != nil:
			fmt.Fprintf(stderr, "claudex: inspect-legacy: %v\n", err)
			return 1
		default:
			fmt.Fprintf(stderr, "claudex: inspect-legacy: no pre-pivot state.json in %s\n", path)
			return 1
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "claudex: inspect-legacy: %v\n", err)
		return 1
	}
	rep, err := legacy.Inspect(raw)
	if err != nil {
		fmt.Fprintf(stderr, "claudex: inspect-legacy: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, rep.String()); err != nil {
		fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
		return 1
	}
	return 0
}

const usage = `claudex - pair-programming coordinator for two interactive AI agent terminals

Usage:
  claudex <command>

Commands:
  version          Print version information
  help             Show this help
  attach           Bootstrap or join a run, or reattach/replace a session
  pull             Print the caller's assignment and mirror it to their inbox
  submit           Submit an artifact for the caller's turn
  status           Print the run's status projection
  wait             Long-poll for the next event relevant to a session
  inspect-legacy   Print a redacted, read-only view of a pre-pivot Python run
                   (state.json); it is never resumed as an attach run

Human gates and the operator surface arrive with subject 05.`
