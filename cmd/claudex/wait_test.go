package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/state"
)

// pairSessionID reads the durable registry and returns the pair slot's current session id.
func pairSessionID(t *testing.T, repo, runID string) string {
	t.Helper()
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reg, ok, err := state.OpenRegistry(loc.RegistryDir, loc.RunLock).Load()
	if err != nil || !ok || reg.Pair == nil {
		t.Fatalf("load registry: ok=%v err=%v pair=%v", ok, err, reg.Pair)
	}
	return reg.Pair.CurrentSessionID
}

// TestWaitE2E drives a real run to PLAN_DRAFT and long-polls as the lead, who owns the turn: the
// command prints a schema-valid wait_event of kind "assignment" over the coordinator's coherent
// aggregate snapshot.
func TestWaitE2E(t *testing.T) {
	repo, runID, lead := bootstrapPair(t)

	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"wait", "--repo", repo, "--run", runID, "--session", lead, "--since", "0", "--timeout", "2s"},
		&out, &errb)
	if code != 0 {
		t.Fatalf("wait: exit %d, %s", code, errb.String())
	}
	var ev struct {
		MessageType string  `json:"message_type"`
		Kind        string  `json:"kind"`
		Revision    uint64  `json:"revision"`
		Phase       string  `json:"phase"`
		TurnID      *string `json:"turn_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &ev); err != nil {
		t.Fatalf("decode wait %q: %v", out.String(), err)
	}
	if ev.MessageType != "wait_event" || ev.Kind != "assignment" || ev.Phase != "PLAN_DRAFT" || ev.TurnID == nil {
		t.Fatalf("wait event wrong: %+v", ev)
	}
}

// TestWaitUnchangedOnTimeout: the pair does not own the lead's PLAN_DRAFT turn, so a short wait
// returns a valid `unchanged` event and exit 0. The pair session is discovered from the run state.
func TestWaitUnchangedOnTimeout(t *testing.T) {
	repo, runID, _ := bootstrapPair(t)
	pair := pairSessionID(t, repo, runID)

	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"wait", "--repo", repo, "--run", runID, "--session", pair, "--since", "2", "--timeout", "150ms"},
		&out, &errb)
	if code != 0 {
		t.Fatalf("wait: exit %d, %s", code, errb.String())
	}
	var ev struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(out.Bytes(), &ev); err != nil {
		t.Fatalf("decode wait %q: %v", out.String(), err)
	}
	if ev.Kind != "unchanged" {
		t.Fatalf("kind = %q, want unchanged", ev.Kind)
	}
}

// TestWaitRecoveryRequired: a persistent nonterminal aggregate surfaces as operational exit 1 with
// no event written (the same recovery surface as status).
func TestWaitRecoveryRequired(t *testing.T) {
	repo, runID, lead := bootstrapPair(t)
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	plantPendingCommitTxn(t, loc)

	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"wait", "--repo", repo, "--run", runID, "--session", lead, "--since", "0", "--timeout", "150ms"},
		&out, &errb)
	if code != 1 {
		t.Fatalf("wait over a pending commit txn: exit %d, want 1; stderr=%q", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("recovery-required wait still wrote an event: %q", out.String())
	}
}

// TestWaitUsageBeforeDiscovery: session grammar and the full timeout range are validated BEFORE
// run discovery/I/O, so a malformed session or out-of-range timeout is usage exit 2 even in a repo
// with NO active run (where discovery would otherwise fail operationally at exit 1).
func TestWaitUsageBeforeDiscovery(t *testing.T) {
	repo := gitRepo(t) // git init only — no attach, so no active run
	validSession := "sess-" + strings.Repeat("a", 32)
	cases := []struct {
		name string
		args []string
	}{
		{"empty session", []string{"wait", "--repo", repo}},
		{"malformed session", []string{"wait", "--repo", repo, "--session", "bad id"}},
		{"zero timeout, valid session", []string{"wait", "--repo", repo, "--session", validSession, "--timeout", "0"}},
		{"over-max timeout, valid session", []string{"wait", "--repo", repo, "--session", validSession, "--timeout", "2h"}},
	}
	for _, tc := range cases {
		var out, errb bytes.Buffer
		if code := run(context.Background(), tc.args, &out, &errb); code != 2 {
			t.Fatalf("%s: exit %d, want 2 (usage precedence over no-active-run); stderr=%q", tc.name, code, errb.String())
		}
	}
}

// TestWaitUsageErrors covers the wait command's usage-error dispatch.
func TestWaitUsageErrors(t *testing.T) {
	repo, runID, lead := bootstrapPair(t)
	cases := [][]string{
		{"wait", "--repo", repo, "--run", runID},                              // missing --session
		{"wait", "--repo", repo, "--run", runID, "--session", lead, "extra"},  // positional
		{"wait", "--repo", repo, "--run", runID, "--session", lead, "--nope"}, // unknown flag
		{"wait", "--repo", repo, "--run", "bad id", "--session", lead},        // malformed run id
	}
	for i, args := range cases {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Fatalf("case %d: exit %d, want 2; stderr=%q", i, code, errb.String())
		}
	}
}
