// Package legacy reads pre-pivot Python claudex run state read-only. The attach
// coordinator uses a new, incompatible state major version (D011): a pre-pivot
// Python state.json is never reinterpreted as an attach run — it is detected,
// inspectable in a redacted view, and refused for resume/execution.
//
// The old state was dataclasses.asdict(RunState) with fields run_id/repo/lead/
// driver/mode/phase/sessions/steps/events/... and, crucially, NO schema_version.
// The Go attach state always carries schema_version (and revision), so the two
// shapes are distinguishable with certainty.
package legacy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/David-c0degeek/claudex/internal/redact"
)

// ErrNotLegacy means the bytes are not a recognizable pre-pivot Python state.
var ErrNotLegacy = errors.New("not a pre-pivot claudex state")

// Remediation is the operator-facing guidance for a detected legacy run.
const Remediation = "This is a pre-pivot Python claudex run. The attach coordinator uses a new, " +
	"incompatible state major version and will not resume or execute it. Inspect it read-only with " +
	"`claudex inspect-legacy <path>`, then start a fresh attach run for new work."

var terminalPhases = map[string]bool{"done": true, "failed": true, "aborted": true}

// Detect reports whether raw is a pre-pivot Python state. It is conservative: a
// blob carrying schema_version or revision is treated as a (possibly
// future-version) attach state, NOT as legacy, so the attach loader's
// fail-closed schema check owns it.
func Detect(raw []byte) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	if _, ok := m["schema_version"]; ok {
		return false
	}
	if _, ok := m["revision"]; ok {
		return false
	}
	_, hasLead := m["lead"]
	_, hasPhase := m["phase"]
	if !hasLead || !hasPhase {
		return false
	}
	_, hasDriver := m["driver"]
	_, hasSessions := m["sessions"]
	_, hasEvents := m["events"]
	return hasDriver || hasSessions || hasEvents
}

// Report is a redacted, human-readable summary of a legacy run. It is never
// resumable; Remediation states why.
type Report struct {
	RunID         string `json:"run_id"`
	Repo          string `json:"repo"`
	Lead          string `json:"lead"`
	Pair          string `json:"pair"`
	Driver        string `json:"driver"`
	Mode          string `json:"mode"`
	Phase         string `json:"phase"`
	StepIndex     int    `json:"step_index"`
	Steps         int    `json:"steps"`
	MailboxTurn   int    `json:"mailbox_turn"`
	Terminal      bool   `json:"terminal"`
	GateReason    string `json:"gate_reason"`
	GuidanceNotes string `json:"guidance_notes"`
	Error         string `json:"error"`
	Resumable     bool   `json:"resumable"`
	Remediation   string `json:"remediation"`
}

// pyState mirrors the legacy Python dataclass fields the inspector surfaces.
type pyState struct {
	RunID         string            `json:"run_id"`
	Repo          string            `json:"repo"`
	Lead          string            `json:"lead"`
	Driver        string            `json:"driver"`
	Mode          string            `json:"mode"`
	Phase         string            `json:"phase"`
	StepIndex     int               `json:"step_index"`
	Steps         []json.RawMessage `json:"steps"`
	MailboxTurn   int               `json:"mailbox_turn"`
	GateReason    string            `json:"gate_reason"`
	GuidanceNotes string            `json:"guidance_notes"`
	Error         string            `json:"error"`
}

// Inspect parses a legacy state read-only and returns a redacted Report. Every
// free-text field passes through redact so a secret in the legacy bytes never
// reaches the report. The input bytes are never mutated. Returns ErrNotLegacy
// when raw is not a pre-pivot Python state.
func Inspect(raw []byte) (Report, error) {
	if !Detect(raw) {
		return Report{}, ErrNotLegacy
	}
	var p pyState
	if err := json.Unmarshal(raw, &p); err != nil {
		return Report{}, fmt.Errorf("legacy: decode: %w", err)
	}
	return Report{
		RunID:         redact.Text(p.RunID),
		Repo:          redact.Text(p.Repo),
		Lead:          redact.Text(p.Lead),
		Pair:          otherAgent(p.Lead),
		Driver:        redact.Text(p.Driver),
		Mode:          redact.Text(p.Mode),
		Phase:         redact.Text(p.Phase),
		StepIndex:     p.StepIndex,
		Steps:         len(p.Steps),
		MailboxTurn:   p.MailboxTurn,
		Terminal:      terminalPhases[p.Phase],
		GateReason:    redact.Text(p.GateReason),
		GuidanceNotes: redact.Text(p.GuidanceNotes),
		Error:         redact.Text(p.Error),
		Resumable:     false,
		Remediation:   Remediation,
	}, nil
}

// otherAgent returns the pair for a known lead, redacting anything unexpected so
// a crafted lead value cannot smuggle text into the report.
func otherAgent(lead string) string {
	switch lead {
	case "claude":
		return "codex"
	case "codex":
		return "claude"
	default:
		return redact.Text(lead)
	}
}

// String renders the report for the CLI: a stable, sorted key: value block
// followed by the remediation line.
func (r Report) String() string {
	fields := map[string]string{
		"run_id":         r.RunID,
		"repo":           r.Repo,
		"lead":           r.Lead,
		"pair":           r.Pair,
		"driver":         r.Driver,
		"mode":           r.Mode,
		"phase":          r.Phase,
		"step_index":     fmt.Sprintf("%d", r.StepIndex),
		"steps":          fmt.Sprintf("%d", r.Steps),
		"mailbox_turn":   fmt.Sprintf("%d", r.MailboxTurn),
		"terminal":       fmt.Sprintf("%t", r.Terminal),
		"gate_reason":    r.GateReason,
		"guidance_notes": r.GuidanceNotes,
		"error":          r.Error,
		"resumable":      fmt.Sprintf("%t", r.Resumable),
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("legacy pre-pivot claudex run (read-only)\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-15s %s\n", k, fields[k])
	}
	b.WriteString("\n" + Remediation)
	return b.String()
}
