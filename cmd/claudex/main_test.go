package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// failWriter fails after allowing n successful writes, to exercise the
// write-error path.
type failWriter struct {
	ok int
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.ok <= 0 {
		return 0, errors.New("write failed")
	}
	w.ok--
	return len(p), nil
}

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out, errBuf bytes.Buffer
		if code := run(context.Background(), []string{arg}, &out, &errBuf); code != 0 {
			t.Fatalf("run(%q) exit = %d, want 0 (stderr: %s)", arg, code, errBuf.String())
		}
		if !strings.HasPrefix(out.String(), "claudex ") {
			t.Fatalf("run(%q) stdout = %q, want prefix %q", arg, out.String(), "claudex ")
		}
	}
}

func TestRunHelp(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(context.Background(), []string{"help"}, &out, &errBuf); code != 0 {
		t.Fatalf("run(help) exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("run(help) stdout missing usage: %q", out.String())
	}
}

func TestRunNoArgs(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(context.Background(), nil, &out, &errBuf); code != 2 {
		t.Fatalf("run(nil) exit = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("run(nil) wrote to stdout: %q", out.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(context.Background(), []string{"frobnicate"}, &out, &errBuf); code != 2 {
		t.Fatalf("run(unknown) exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown command") {
		t.Fatalf("run(unknown) stderr = %q, want mention of unknown command", errBuf.String())
	}
}

func TestRunRejectsExtraArgs(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(context.Background(), []string{"version", "junk"}, &out, &errBuf); code != 2 {
		t.Fatalf("run(version junk) exit = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("run(version junk) unexpectedly wrote version: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "takes no arguments") {
		t.Fatalf("run(version junk) stderr = %q, want arity error", errBuf.String())
	}
}

func TestRunWriteErrorIsOperationalFailure(t *testing.T) {
	var errBuf bytes.Buffer
	if code := run(context.Background(), []string{"version"}, &failWriter{ok: 0}, &errBuf); code != 1 {
		t.Fatalf("run(version) with failing stdout exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "write failed") {
		t.Fatalf("stderr = %q, want write-failure diagnostic", errBuf.String())
	}
}
