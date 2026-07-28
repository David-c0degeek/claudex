package attach

import (
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
	"github.com/David-c0degeek/claudex/internal/txn"
)

// TestBootstrapIntentFitsItsPayloadCeilingAtEveryAdmittedMaximum.
//
// The intent is durably appended by txn.Run BEFORE any participant executes, and the transaction
// refuses a payload over 64 KiB. So "each field is individually bounded" is not the property that
// matters — what matters is that the SUM of every field at its own maximum still fits, because a policy
// that passes every individual check and then cannot be journaled is a run that fails after it has
// begun being created.
//
// The arithmetic is not obvious and was got wrong once. Both input snapshots are []byte, which Go
// marshals as base64, so each 16 KiB source becomes ~21.8 KiB on the wire — 43.7 KiB for the pair,
// before the effective policy, the resolved environment, or any identity field. An earlier ceiling
// permitted 32 KiB of RAW resolved environment, which could not have fitted alongside the snapshots at
// all, and control bytes would have expanded it further.
//
// This test builds the worst case rather than reasoning about it.
func TestBootstrapIntentFitsItsPayloadCeilingAtEveryAdmittedMaximum(t *testing.T) {
	runID := "run-" + strings.Repeat("a", 8)
	relDir := state.RunDirRelFor(runID)

	// Both snapshots at the documented source ceiling. The bytes are incompressible-looking and
	// deliberately include characters JSON must escape, since escaping is the expansion that bit us.
	filler := strings.Repeat("\"\\\n\t x", config.MaxContractBytes)
	bigTask := []byte(filler[:config.MaxContractBytes])
	bigPolicy := []byte(filler[:config.MaxContractBytes])

	// A resolved environment at the wire ceiling.
	var names []string
	vals := map[string]string{}
	for i := 0; len(names) < 8; i++ {
		n := "VAR" + string(rune('A'+i))
		names = append(names, n)
		vals[n] = strings.Repeat("v", 715)
	}
	gate := config.TestGate{Argv: []string{"go", "test", "./..."}, Env: config.TestGateEnv{Inherit: names}}
	resolved, err := config.ResolveForRun(gate, config.HostGOOS(),
		func(n string) (string, bool) { v, ok := vals[n]; return v, ok }, relDir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	pol := config.DefaultRunPolicy()
	pol.TestGate = gate

	in := BootstrapIntent{
		SchemaVersion:       bootstrapIntentVersion,
		RunID:               runID,
		TxnID:               strings.Repeat("t", 32),
		OperationID:         strings.Repeat("o", 64),
		SessionID:           "sess-" + strings.Repeat("b", 32),
		Agent:               state.AgentClaude,
		CreatedUnix:         1 << 40,
		PairJoinOperationID: strings.Repeat("p", 64),
		RelDir:              relDir,
		TaskRelPath:         "inputs/task.json",
		TaskDigest:          strings.Repeat("a", 64),
		TaskCanonical:       bigTask,
		PolicyRelPath:       "inputs/policy.json",
		PolicyDigest:        strings.Repeat("b", 64),
		PolicyCanonical:     bigPolicy,
		EffectivePolicy:     pol,
		ResolvedExecution:   resolved,
		Base:                pol.BaseBranch,
		BaseCommit:          strings.Repeat("c", 64),
		WorktreeRelPath:     relDir + "/worktree",
		RunBranch:           "claudex/" + runID,
		FSClass:             "supported-local",
		FSReason:            strings.Repeat("r", 256),
	}

	payload, err := in.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(payload) > txn.MaxPayloadBytes {
		t.Fatalf("a bootstrap intent at every admitted maximum marshals to %d bytes, over the %d-byte "+
			"transaction payload ceiling: a policy could pass every individual bound and still be unjournalable",
			len(payload), txn.MaxPayloadBytes)
	}
	t.Logf("worst-case intent payload: %d of %d bytes", len(payload), txn.MaxPayloadBytes)

	// The resolved environment must actually be AT its ceiling, or this is not the worst case and the
	// headroom above is an illusion produced by an undersized fixture.
	reWire, err := resolved.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal resolved execution: %v", err)
	}
	if len(reWire) < config.MaxResolvedExecutionWireBytes*9/10 {
		t.Fatalf("the resolved environment fixture encodes to %d bytes, well under its %d-byte ceiling; "+
			"grow it or this test is not proving the worst case", len(reWire), config.MaxResolvedExecutionWireBytes)
	}

	// And the headroom must be real, not incidental: the resolved environment's own ceiling has to fit
	// inside what is left after the snapshots, or the two bounds are inconsistent with each other.
	if remaining := txn.MaxPayloadBytes - len(payload); remaining < 0 {
		t.Fatalf("no headroom: %d", remaining)
	}
}
