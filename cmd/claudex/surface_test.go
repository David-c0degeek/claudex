package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestExactCommandSurface is the 03.9 invariant: attach is the SOLE run bootstrap, and the shipped
// executable surface is exactly the attach protocol plus the read-only legacy inspector.
//
// It is written as an exact set rather than a "these exist" check on purpose. The subject's whole
// point is that nothing else can create or drive a run — a test that only asserted presence would
// pass just as happily if a stray `run` or `pair` entrypoint were added beside them.
func TestExactCommandSurface(t *testing.T) {
	// Per the D021 amendment, gates/operator are a subject-05 closure dependency and are NOT part of
	// this surface until 05 lands them.
	want := []string{"attach", "help", "inspect-legacy", "pull", "status", "submit", "version", "wait"}

	// Compare against the ACTUAL routed set, enumerated from the dispatch table. Iterating the
	// expected list instead would only ever detect a MISSING command — an extra, unadvertised verb
	// (a stray `run`, a leftover `debug`) would pass unnoticed, and ruling exactly that out is the
	// whole point of this invariant.
	got := make([]string, 0, len(commands))
	for name := range commands {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatchable surface = %v, want exactly %v", got, want)
	}

	// The pre-pivot entrypoints are named explicitly as well. The set comparison above already
	// excludes them; this states the intent, so a future reader sees WHY the set is closed.
	for _, cmd := range []string{"run", "pair", "gates", "operator", "resume", "exec", "start", "drive"} {
		if _, exists := commands[cmd]; exists {
			t.Errorf("command %q must not exist: attach is the sole run bootstrap", cmd)
		}
	}
	// Aliases route to informational commands only; none of them may reach a run-affecting verb.
	for alias, target := range aliases {
		if target != "version" && target != "help" {
			t.Errorf("alias %q routes to %q; aliases are for informational commands only", alias, target)
		}
	}

	// The help text must advertise EXACTLY what is dispatchable — an operator reading it cannot be
	// pointed at a verb that does not exist, nor miss one that does. The advertised list is parsed
	// from the Commands block rather than substring-matched, so prose mentioning a future subject
	// cannot be mistaken for a shipped verb.
	if advertised := helpCommands(t); strings.Join(advertised, ",") != strings.Join(got, ",") {
		t.Fatalf("help advertises %v, want exactly the dispatchable %v", advertised, got)
	}
}

// helpCommands returns the sorted verbs listed in help's Commands block.
func helpCommands(t *testing.T) []string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"help"}, &out, &errb); code != 0 {
		t.Fatalf("help: exit %d, %s", code, errb.String())
	}
	var cmds []string
	inBlock := false
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "Commands:") {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		fields := strings.Fields(line)
		// Continuation lines of a wrapped description are indented past the verb column.
		if len(fields) == 0 || !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "     ") {
			continue
		}
		cmds = append(cmds, fields[0])
	}
	sort.Strings(cmds)
	return cmds
}

// The legacy inspector is READ-ONLY: it renders a redacted view of a pre-pivot state.json and can
// never resume or execute it. The property that matters is that it creates nothing — a pre-pivot
// state is history, not a run this build can pick up — so that is what is asserted, over a real
// legacy document in an otherwise empty directory.
func TestLegacyInspectorIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	writeF(t, legacy, `{"run_id":"r","repo":"/p","lead":"claude","driver":"headless","phase":"init"}`)

	var out, errb bytes.Buffer
	run(context.Background(), []string{"inspect-legacy", legacy}, &out, &errb)

	// Whatever it decided about the document, it must not have created a run: no runtime directory,
	// and nothing beside the file it was pointed at.
	if _, err := os.Stat(filepath.Join(dir, ".claudex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the inspector created a runtime directory (stat err = %v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("the inspector wrote to the directory: %v", entries)
	}
}
