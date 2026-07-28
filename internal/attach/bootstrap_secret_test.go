package attach

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/config"
	"github.com/David-c0degeek/claudex/internal/state"
)

// policyWithSecretEnv builds a run policy whose test gate carries a credential-shaped environment
// entry. Everything else about it is ordinary and valid, so the refusal is attributable to the secret
// and to nothing else.
func policyWithSecretEnv(t *testing.T, disabled bool) []byte {
	t.Helper()
	gate := map[string]any{
		"env": map[string]any{
			"inherit": []string{},
			"set":     []any{map[string]any{"name": "TOKEN", "value": "sk-ant-abcdefghijklmnopqrstuvwx"}},
		},
	}
	if disabled {
		gate["disabled"] = true
	} else {
		gate["argv"] = []string{"go", "test", "./..."}
	}
	b, err := json.Marshal(map[string]any{
		"schema_version": config.RunPolicyVersion,
		"base_branch":    "main",
		"test_gate":      gate,
	})
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	return b
}

// TestFirstAttachRefusesACredentialAndLeavesNothingBehind.
//
// This is the assertion the refusal is FOR. txn.Run durably appends the whole bootstrap intent before
// any participant executes, so a credential that survives to intent construction is already written
// down — a later guard can object, but only about something that is now on disk. Checking that the call
// returns an error therefore proves the wrong half; what matters is that nothing was created.
//
// The disabled case is here because it was the hole: resolution used to skip the environment path
// entirely when the gate was disabled, so the credential reached the journal precisely BECAUSE nothing
// was going to use it.
func TestFirstAttachRefusesACredentialAndLeavesNothingBehind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
	}{
		{"an enabled gate", false},
		{"a disabled gate", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, g, _ := realGitRepo(t)
			req := realRequest(t, repo, g, opID("a"), rand.Reader)
			req.PolicyCanonical = policyWithSecretEnv(t, tc.disabled)

			_, err := FirstAttach(context.Background(), req)
			if err == nil {
				t.Fatal("a first attach carrying a credential-shaped environment entry succeeded")
			}
			if !strings.Contains(err.Error(), "looks like a credential") {
				t.Fatalf("err = %v, want a credential refusal", err)
			}

			// Nothing durable. The whole point of refusing before the intent is built is that there is
			// no journal holding the secret and no run to clean up.
			claudex := filepath.Join(repo, ".claudex")
			if entries, rerr := os.ReadDir(claudex); rerr == nil {
				for _, e := range entries {
					if e.Name() == "runs" {
						runs, _ := os.ReadDir(filepath.Join(claudex, "runs"))
						if len(runs) != 0 {
							t.Fatalf("a refused attach left %d run directories behind", len(runs))
						}
					}
				}
			}
			// And no active run was recorded.
			lay := layoutFor(repo)
			if cur, ok, cerr := state.OpenCurrentRun(lay.currentRunDir, lay.repoLock).Load(); cerr == nil && ok && cur.Active {
				t.Fatalf("a refused attach left an active run: %+v", cur)
			}
			// The secret must not appear anywhere under .claudex, whatever the layout happens to be.
			assertNoSecretOnDisk(t, claudex)
		})
	}
}

// assertNoSecretOnDisk walks the tool's whole directory looking for the credential. It searches by
// CONTENT rather than by the paths this test expects to exist, so a future layout change cannot make
// the check silently vacuous.
func assertNoSecretOnDisk(t *testing.T, root string) {
	t.Helper()
	const secret = "sk-ant-abcdefghijklmnopqrstuvwx"
	found := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // absent is the expected case
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(b), secret) {
			found++
			t.Errorf("the credential was written to %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if found > 0 {
		t.Fatalf("the credential reached disk in %d file(s)", found)
	}
}

// TestFirstAttachAcceptsADisabledGate.
//
// The shape with no argv is the one that exposed a latent round-trip defect: ParseRunPolicy left the
// vector nil, MarshalJSON wrote it as `[]`, and the persisted effective policy therefore decoded to an
// empty non-nil slice — which reflect.DeepEqual does not consider equal to nil. BootstrapIntent
// compares exactly that, so a perfectly ordinary disabled policy could not bootstrap at all.
//
// A package-level round-trip assertion would not have caught it, and did not: the test that claimed to
// cover this compared a parse against another parse of the same bytes, using a helper that treats nil
// and empty as equal. This drives the real attach, which is where the invariant actually lives.
func TestFirstAttachAcceptsADisabledGate(t *testing.T) {
	repo, g, _ := realGitRepo(t)
	req := realRequest(t, repo, g, opID("a"), rand.Reader)
	b, err := json.Marshal(map[string]any{
		"schema_version": config.RunPolicyVersion,
		"base_branch":    "main",
		// No argv key at all, which is the whole point: the absent collection is what differed.
		"test_gate": map[string]any{"disabled": true},
	})
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	req.PolicyCanonical = b

	res, err := FirstAttach(context.Background(), req)
	if err != nil {
		t.Fatalf("a disabled-gate policy could not bootstrap: %v", err)
	}
	assertProvisioned(t, g, repo, res.RunID)

	// And the persisted policy still compares equal to a fresh parse of its own snapshot — the exact
	// equality the bootstrap intent enforces, asserted against durable bytes rather than in memory.
	lay := layoutFor(repo)
	runDir := lay.runDir(state.RunDirRelFor(res.RunID))
	rs, ok, lerr := state.Open(filepath.Join(runDir, "state"), lay.repoLock).Load()
	if lerr != nil || !ok {
		t.Fatalf("load run state: ok=%v err=%v", ok, lerr)
	}
	reparsed, perr := config.ParseRunPolicy(b)
	if perr != nil {
		t.Fatalf("re-parse the policy source: %v", perr)
	}
	if !reflect.DeepEqual(rs.EffectivePolicy, reparsed) {
		t.Fatalf("the persisted effective policy is not DeepEqual to a fresh parse of its source: %+v vs %+v",
			rs.EffectivePolicy, reparsed)
	}
}
