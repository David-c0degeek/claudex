// Package legacy reads pre-pivot Python claudex run state read-only. The attach
// coordinator uses a new, incompatible state major version (D011): a pre-pivot
// Python run directory is never reinterpreted or resumed as an attach run — it
// is detected, inspectable in a redacted view, and refused at bootstrap.
//
// The old state is dataclasses.asdict(RunState) from
// reliability-observability-refactor:claudex/state.py (state_schema_version 6):
// run_id/repo/lead/driver/mode/phase/lifecycle/sessions/steps/events/… and,
// crucially, NO schema_version (its own key is state_schema_version). The Go
// attach state always carries schema_version (and revision), so the two shapes
// are distinguishable with certainty.
package legacy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// LegacyStateFile is the legacy run's state filename.
const LegacyStateFile = "state.json"

var terminalPhases = map[string]bool{"done": true, "failed": true, "aborted": true}

// LegacyRunError is a typed refusal for a run directory that holds pre-pivot
// state; it carries the offending path and the operator remediation so a
// bootstrap/resume path can fail closed before any allocation or mutation.
type LegacyRunError struct {
	StatePath string
}

func (e *LegacyRunError) Error() string {
	return fmt.Sprintf("%s is a pre-pivot Python claudex run: %s", e.StatePath, Remediation)
}

// UnknownStateError signals a directory that holds an existing state.json which
// is neither a recognized pre-pivot legacy state nor part of the attach store.
// Attach state lives in immutable generation directories, never in
// <run>/state.json, so any state.json here is unexplained — malformed, from an
// unrelated tool, or a future/unknown schema — and the directory must not be
// allocated over. Fail closed rather than trust it.
type UnknownStateError struct {
	StatePath string
}

func (e *UnknownStateError) Error() string {
	return fmt.Sprintf("%s holds an unrecognized state.json (not a legacy run, not an attach generation); "+
		"refusing to allocate over it — inspect, upgrade, or move it aside before starting a run here", e.StatePath)
}

// CheckRunDir inspects dir for an existing state.json WITHOUT mutating anything,
// so a bootstrap/resume path can fail closed before any allocation. Because
// attach state lives in immutable generation directories (never in
// <dir>/state.json), any existing state.json is unexplained and blocks:
//
//   - missing state.json           -> nil (safe to allocate)
//   - a detected pivot Python run  -> *LegacyRunError (with Remediation)
//   - any other existing state.json -> *UnknownStateError (fail closed)
//   - an unreadable state.json     -> the read error
func CheckRunDir(dir string) error {
	p := filepath.Join(dir, LegacyStateFile)
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if Detect(raw) {
		return &LegacyRunError{StatePath: p}
	}
	return &UnknownStateError{StatePath: p}
}

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
	RunID            string `json:"run_id"`
	PredecessorRunID string `json:"predecessor_run_id"`
	Repo             string `json:"repo"`
	Lead             string `json:"lead"`
	Pair             string `json:"pair"`
	Driver           string `json:"driver"`
	Mode             string `json:"mode"`
	Phase            string `json:"phase"`
	Lifecycle        string `json:"lifecycle"`
	GateKind         string `json:"gate_kind"`
	StepIndex        int    `json:"step_index"`
	Steps            int    `json:"steps"`
	MailboxTurn      int    `json:"mailbox_turn"`
	Terminal         bool   `json:"terminal"`
	GateReason       string `json:"gate_reason"`
	Error            string `json:"error"`
	Resumable        bool   `json:"resumable"`
	Remediation      string `json:"remediation"`
}

// pyState mirrors the pivot-schema (state_schema_version 6) dataclass fields the
// inspector surfaces.
type pyState struct {
	RunID            string            `json:"run_id"`
	PredecessorRunID string            `json:"predecessor_run_id"`
	Repo             string            `json:"repo"`
	Lead             string            `json:"lead"`
	Driver           string            `json:"driver"`
	Mode             string            `json:"mode"`
	Phase            string            `json:"phase"`
	Lifecycle        string            `json:"lifecycle"`
	GateKind         string            `json:"gate_kind"`
	StepIndex        int               `json:"step_index"`
	Steps            []json.RawMessage `json:"steps"`
	MailboxTurn      int               `json:"mailbox_turn"`
	GateReason       string            `json:"gate_reason"`
	Error            string            `json:"error"`
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
		RunID:            redact.Text(p.RunID),
		PredecessorRunID: redact.Text(p.PredecessorRunID),
		Repo:             redact.Text(p.Repo),
		Lead:             redact.Text(p.Lead),
		Pair:             otherAgent(p.Lead),
		Driver:           redact.Text(p.Driver),
		Mode:             redact.Text(p.Mode),
		Phase:            redact.Text(p.Phase),
		Lifecycle:        redact.Text(p.Lifecycle),
		GateKind:         redact.Text(p.GateKind),
		StepIndex:        p.StepIndex,
		Steps:            len(p.Steps),
		MailboxTurn:      p.MailboxTurn,
		Terminal:         terminalPhases[p.Phase],
		GateReason:       redact.Text(p.GateReason),
		Error:            redact.Text(p.Error),
		Resumable:        false,
		Remediation:      Remediation,
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
		"run_id":       r.RunID,
		"predecessor":  r.PredecessorRunID,
		"repo":         r.Repo,
		"lead":         r.Lead,
		"pair":         r.Pair,
		"driver":       r.Driver,
		"mode":         r.Mode,
		"phase":        r.Phase,
		"lifecycle":    r.Lifecycle,
		"gate_kind":    r.GateKind,
		"step_index":   fmt.Sprintf("%d", r.StepIndex),
		"steps":        fmt.Sprintf("%d", r.Steps),
		"mailbox_turn": fmt.Sprintf("%d", r.MailboxTurn),
		"terminal":     fmt.Sprintf("%t", r.Terminal),
		"gate_reason":  r.GateReason,
		"error":        r.Error,
		"resumable":    fmt.Sprintf("%t", r.Resumable),
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("legacy pre-pivot claudex run (read-only)\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-13s %s\n", k, fields[k])
	}
	b.WriteString("\n" + Remediation)
	return b.String()
}
