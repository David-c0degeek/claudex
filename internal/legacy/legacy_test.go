package legacy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is an exact dataclasses.asdict(RunState(...)) shape from
// reliability-observability-refactor:claudex/state.py at commit 1b6ceea
// (state_schema_version 6), with a secret planted in the free-text gate_reason.
const fixture = "testdata/legacy_state.json"

const plantedSecret = "sk-ant-abcdefghijklmnopqrstuvwx"

func readFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func TestDetectPrePivotState(t *testing.T) {
	if !Detect(readFixture(t)) {
		t.Fatalf("fixture should be detected as a pre-pivot Python state")
	}
	// An attach state (schema_version + revision) is not legacy: the attach
	// loader's fail-closed schema check owns it, not this package.
	attach := []byte(`{"schema_version":1,"revision":3,"run_id":"r","phase":"init"}`)
	if Detect(attach) {
		t.Fatalf("an attach state must not be detected as legacy")
	}
	future := []byte(`{"schema_version":999,"revision":1,"run_id":"r"}`)
	if Detect(future) {
		t.Fatalf("a future-version attach state must not be detected as legacy")
	}
	if Detect([]byte(`not json`)) || Detect([]byte(`{"unrelated":1}`)) {
		t.Fatalf("non-claudex blobs must not be detected as legacy")
	}
}

func TestInspectExportsRedactedReport(t *testing.T) {
	raw := readFixture(t)
	rep, err := Inspect(raw)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}

	if rep.RunID != "20260714-abc123" || rep.Lead != "claude" || rep.Pair != "codex" {
		t.Fatalf("unexpected identity fields: %+v", rep)
	}
	if rep.PredecessorRunID != "20260713-def456" {
		t.Fatalf("predecessor not surfaced: %+v", rep)
	}
	if rep.Phase != "await_guidance" || rep.Terminal {
		t.Fatalf("await_guidance must be non-terminal: %+v", rep)
	}
	if rep.Lifecycle != "paused_budget" || rep.GateKind != "budget" {
		t.Fatalf("unexpected lifecycle/gate fields: %+v", rep)
	}
	if rep.Steps != 2 || rep.StepIndex != 1 || rep.MailboxTurn != 7 {
		t.Fatalf("unexpected counters: %+v", rep)
	}
	if rep.Resumable {
		t.Fatalf("a legacy run must never report resumable")
	}

	// The planted secret must be gone from every rendered surface.
	if !strings.Contains(rep.GateReason, "[REDACTED]") {
		t.Fatalf("gate_reason not redacted: %q", rep.GateReason)
	}
	rendered := rep.String()
	if strings.Contains(rendered, plantedSecret) || strings.Contains(rep.GateReason, plantedSecret) {
		t.Fatalf("secret leaked into the report:\n%s", rendered)
	}
	if !strings.Contains(rendered, Remediation) {
		t.Fatalf("rendered report is missing the remediation guidance")
	}
}

func TestInspectIsReadOnly(t *testing.T) {
	before := readFixture(t)
	input := append([]byte(nil), before...)
	if _, err := Inspect(input); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !bytes.Equal(input, before) {
		t.Fatalf("Inspect mutated its input slice")
	}
	after := readFixture(t)
	if !bytes.Equal(after, before) {
		t.Fatalf("the fixture on disk changed after a read-only inspect")
	}
}

func TestInspectRejectsNonLegacy(t *testing.T) {
	attach := []byte(`{"schema_version":1,"revision":1,"run_id":"r","phase":"init"}`)
	if _, err := Inspect(attach); !errors.Is(err, ErrNotLegacy) {
		t.Fatalf("inspect attach state err = %v, want ErrNotLegacy", err)
	}
}

// CheckRunDir is the actual bootstrap/resume refusal seam: a legacy run
// directory (state.json + state.v1.bak.json + current) must be refused with a
// typed error carrying Remediation, and nothing on disk may change.
func TestCheckRunDirRefusesLegacyAndPreservesBytes(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	bakPath := filepath.Join(dir, "state.v1.bak.json")
	curPath := filepath.Join(dir, "current")

	stateBytes := readFixture(t)
	bakBytes := append([]byte(nil), stateBytes...) // an existing legacy backup
	curBytes := []byte("20260714-abc123\n")
	mustWrite(t, statePath, stateBytes)
	mustWrite(t, bakPath, bakBytes)
	mustWrite(t, curPath, curBytes)

	err := CheckRunDir(dir)
	var lre *LegacyRunError
	if !errors.As(err, &lre) {
		t.Fatalf("CheckRunDir err = %v, want *LegacyRunError", err)
	}
	if lre.StatePath != statePath {
		t.Fatalf("LegacyRunError.StatePath = %q, want %q", lre.StatePath, statePath)
	}
	if !strings.Contains(lre.Error(), Remediation) {
		t.Fatalf("refusal missing remediation: %v", lre)
	}

	// .bak and pointer preservation: the read-only guard changed nothing.
	assertBytes(t, statePath, stateBytes)
	assertBytes(t, bakPath, bakBytes)
	assertBytes(t, curPath, curBytes)
}

func TestCheckRunDirIgnoresMissingAndAttach(t *testing.T) {
	empty := t.TempDir()
	if err := CheckRunDir(empty); err != nil {
		t.Fatalf("empty dir CheckRunDir = %v, want nil", err)
	}
	attach := t.TempDir()
	mustWrite(t, filepath.Join(attach, "state.json"), []byte(`{"schema_version":1,"revision":1,"run_id":"r"}`))
	if err := CheckRunDir(attach); err != nil {
		t.Fatalf("attach state.json CheckRunDir = %v, want nil (not legacy)", err)
	}
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed after a read-only check", path)
	}
}
