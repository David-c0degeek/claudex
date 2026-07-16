package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestInspectLegacyPrintsRedactedRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	secret := "token=sk-ant-abcdefghijklmnopqrstuvwx"
	body := `{"run_id":"r1","repo":"/p","lead":"claude","driver":"headless","phase":"await_guidance","guidance_notes":"` + secret + `","steps":[],"events":[]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var out, errBuf bytes.Buffer
	if code := run(context.Background(), []string{"inspect-legacy", path}, &out, &errBuf); code != 0 {
		t.Fatalf("inspect-legacy exit = %d (stderr: %s)", code, errBuf.String())
	}
	s := out.String()
	if strings.Contains(s, "sk-ant-") {
		t.Fatalf("secret leaked into inspect-legacy output:\n%s", s)
	}
	if !strings.Contains(s, "resumable") || !strings.Contains(s, "will not resume") {
		t.Fatalf("inspect-legacy output missing the refusal:\n%s", s)
	}

	// The read-only inspector must not touch the file.
	after, _ := os.ReadFile(path)
	if string(after) != body {
		t.Fatalf("inspect-legacy modified the input file")
	}
}

func TestInspectLegacyRejectsNonLegacyAndArity(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(context.Background(), []string{"inspect-legacy"}, &out, &errBuf); code != 2 {
		t.Fatalf("inspect-legacy with no path exit = %d, want 2", code)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "attach.json")
	os.WriteFile(path, []byte(`{"schema_version":1,"revision":1,"run_id":"r"}`), 0o600)
	out.Reset()
	errBuf.Reset()
	if code := run(context.Background(), []string{"inspect-legacy", path}, &out, &errBuf); code != 1 {
		t.Fatalf("inspect-legacy on an attach state exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "not a pre-pivot") {
		t.Fatalf("stderr = %q, want not-legacy diagnostic", errBuf.String())
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
