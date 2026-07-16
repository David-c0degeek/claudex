package legacy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = "testdata/legacy_state.json"

// The planted secret in the fixture's guidance_notes; the redacted report must
// never contain it.
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
	// An attach state (has schema_version + revision) is not legacy: the attach
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
	if rep.Phase != "await_guidance" || rep.Terminal {
		t.Fatalf("await_guidance must be non-terminal: %+v", rep)
	}
	if rep.Steps != 2 || rep.StepIndex != 1 || rep.MailboxTurn != 7 {
		t.Fatalf("unexpected counters: %+v", rep)
	}
	if rep.Resumable {
		t.Fatalf("a legacy run must never report resumable")
	}

	// The planted secret must be gone from every rendered surface.
	if !strings.Contains(rep.GuidanceNotes, "[REDACTED]") {
		t.Fatalf("guidance_notes not redacted: %q", rep.GuidanceNotes)
	}
	rendered := rep.String()
	if strings.Contains(rendered, plantedSecret) || strings.Contains(rep.GuidanceNotes, plantedSecret) {
		t.Fatalf("secret leaked into the report:\n%s", rendered)
	}
	if !strings.Contains(rendered, Remediation) {
		t.Fatalf("rendered report is missing the remediation guidance")
	}
}

func TestInspectIsReadOnly(t *testing.T) {
	// Inspect must not mutate its input, and the on-disk fixture must be byte
	// identical afterwards (legacy .bak/state bytes are preserved, D017 retention
	// supersedes the old single .bak).
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

// Guard against the fixture drifting out of the package directory.
func TestFixtureExists(t *testing.T) {
	if _, err := os.Stat(filepath.FromSlash(fixture)); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
}
