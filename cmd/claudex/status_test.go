package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/genstore"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// TestStatusE2E drives a real run to PLAN_DRAFT and asserts the status projection over the
// coordinator's lock-free coherent snapshot: schema-valid JSON with the live phase and owner.
func TestStatusE2E(t *testing.T) {
	repo, runID, _ := bootstrapPair(t)

	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"status", "--repo", repo, "--run", runID}, &out, &errb); code != 0 {
		t.Fatalf("status: exit %d, %s", code, errb.String())
	}
	var s struct {
		MessageType string  `json:"message_type"`
		RunID       string  `json:"run_id"`
		Phase       string  `json:"phase"`
		Lifecycle   string  `json:"lifecycle"`
		WhoseTurn   *string `json:"whose_turn"`
		TurnID      *string `json:"turn_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatalf("decode status %q: %v", out.String(), err)
	}
	if s.MessageType != "status" || s.RunID != runID || s.Phase != "PLAN_DRAFT" || s.Lifecycle != "running" {
		t.Fatalf("status header wrong: %+v", s)
	}
	if s.WhoseTurn == nil || s.TurnID == nil {
		t.Fatalf("PLAN_DRAFT status should name a live owner: %+v", s)
	}

	// Discovery: with no --run, status finds the active run.
	out.Reset()
	errb.Reset()
	if code := run(context.Background(), []string{"status", "--repo", repo}, &out, &errb); code != 0 {
		t.Fatalf("status (discovered): exit %d, %s", code, errb.String())
	}
}

// TestStatusRecoveryRequired proves the coherent read fails closed (operational exit 1) on a
// nonterminal aggregate transaction — here, a planted pending commit-txn journal.
func TestStatusRecoveryRequired(t *testing.T) {
	repo, runID, _ := bootstrapPair(t)
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Plant a nonterminal (pending) commit-txn journal record — the aggregate read must refuse.
	plantPendingCommitTxn(t, loc)

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"status", "--repo", repo, "--run", runID}, &out, &errb)
	if code != 1 {
		t.Fatalf("status with a pending commit txn: exit %d, want 1; stderr=%q", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("recovery-required status still wrote a projection: %q", out.String())
	}
}

// TestStatusUsageErrors covers the status command's usage-error dispatch.
func TestStatusUsageErrors(t *testing.T) {
	cases := [][]string{
		{"status", "extra"},           // positional
		{"status", "--nope"},          // unknown flag
		{"status", "--run", "bad id"}, // malformed run id
	}
	for i, args := range cases {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Fatalf("case %d: exit %d, want 2; stderr=%q", i, code, errb.String())
		}
	}
}

// plantPendingCommitTxn leaves a NONTERMINAL commit-txn journal record for the run (a prepared
// transaction whose first step halts on Apply), so a lock-free reader classifies the run as
// mid-transaction. It uses the public txn journal API + the attach commit-txn intent kind,
// matching how the coordinator's driver prepares one.
func plantPendingCommitTxn(t *testing.T, loc attach.RunLocation) {
	t.Helper()
	g, ok, err := genstore.Acquire(loc.RunLock)
	if err != nil || !ok {
		t.Fatalf("acquire run lock: ok=%v err=%v", ok, err)
	}
	defer g.Release()
	j := txn.Open(loc.CommitTxnDir, loc.RunLock)
	plan := txn.Plan{
		Intent: txn.Intent{
			Version:               txn.IntentVersion,
			Kind:                  attach.CommitTxnIntentKind,
			TxnID:                 "ctxn-" + repeat("a", 32),
			ExpectedStateRevision: 1,
			Payload:               []byte(fmt.Sprintf(`{"run_id":%q}`, loc.RunID)),
		},
		Steps: []txn.Step{{
			Name:           "halt",
			Status:         func() (txn.StepStatus, error) { return txn.StatusNotApplied, nil },
			Apply:          func() error { return errors.New("planted halt") },
			ConfirmDurable: func() error { return nil },
		}},
	}
	// Run drives the prepare (a nonterminal record) then halts on the Apply error, leaving the
	// journal head nonterminal — exactly the pending-transaction shape the read must refuse.
	if _, rerr := j.Run(g, plan); rerr == nil {
		t.Fatal("planted commit-txn unexpectedly completed")
	}
}
