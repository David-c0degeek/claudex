package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out, errBuf bytes.Buffer
		if code := run([]string{arg}, &out, &errBuf); code != 0 {
			t.Fatalf("run(%q) exit = %d, want 0 (stderr: %s)", arg, code, errBuf.String())
		}
		if !strings.HasPrefix(out.String(), "claudex ") {
			t.Fatalf("run(%q) stdout = %q, want prefix %q", arg, out.String(), "claudex ")
		}
	}
}

func TestRunHelp(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"help"}, &out, &errBuf); code != 0 {
		t.Fatalf("run(help) exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("run(help) stdout missing usage: %q", out.String())
	}
}

func TestRunNoArgs(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(nil, &out, &errBuf); code != 2 {
		t.Fatalf("run(nil) exit = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("run(nil) wrote to stdout: %q", out.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"frobnicate"}, &out, &errBuf); code != 2 {
		t.Fatalf("run(unknown) exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown command") {
		t.Fatalf("run(unknown) stderr = %q, want mention of unknown command", errBuf.String())
	}
}
