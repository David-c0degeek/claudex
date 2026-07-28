package attach

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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

			lay := layoutFor(repo)
			_, err := FirstAttach(context.Background(), req)
			if err == nil {
				t.Fatal("a first attach carrying a credential-shaped environment entry succeeded")
			}
			if !strings.Contains(err.Error(), "looks like a credential") {
				t.Fatalf("err = %v, want a credential refusal", err)
			}

			// Nothing durable — asserted as ABSENCE, not as "contains no secret".
			//
			// The requirement is that refusal happens BEFORE the intent is built, so the bootstrap
			// journal and the active-run pointer must not exist at all. An earlier version of this test
			// checked only that no literal token appeared on disk, which would have accepted a journal
			// holding a redacted or malformed residue — and its active-run check passed whenever Load
			// returned an error or an inactive record, so a decode failure read as success.
			assertAbsent(t, lay.bootstrapJournal)
			assertAbsent(t, lay.currentRunDir)
			assertNoRunDirectories(t, filepath.Join(repo, ".claudex", "runs"))
			assertNoSecretOnDisk(t, filepath.Join(repo, ".claudex"))
		})
	}
}

// assertAbsent proves a durable artefact was never created.
//
// Absence is the claim, so anything other than "not there" is a failure — including a read error, which
// an earlier version of this test would have accepted as success.
func assertAbsent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		t.Fatalf("a refused attach created %s", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s: %v", path, err)
	}
}

// assertNoRunDirectories proves no run was allocated. An absent runs directory is the expected shape;
// an unreadable one is a failure rather than a pass.
func assertNoRunDirectories(t *testing.T, runs string) {
	t.Helper()
	entries, err := os.ReadDir(runs)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read %s: %v", runs, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("a refused attach left run directories behind: %v", names)
	}
}

// assertNoSecretOnDisk is the belt to the absence braces above: even if some future artefact is
// legitimately created before the refusal, the credential itself must not be in it.
//
// It searches by CONTENT across the whole tree rather than at the paths this test expects, so a layout
// change cannot make it quietly vacuous — and it tolerates ONLY an absent root. Swallowing walk and read
// errors, as it first did, turns an unreadable tree into a pass, which is the same defect one level up.
func assertNoSecretOnDisk(t *testing.T, root string) {
	t.Helper()
	const secret = "sk-ant-abcdefghijklmnopqrstuvwx"
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return
	} else if err != nil {
		t.Fatalf("stat %s: %v", root, err)
	}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", p, err)
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", p, rerr)
		}
		if strings.Contains(string(b), secret) {
			return fmt.Errorf("the credential was written to %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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

	lay := layoutFor(repo)
	res, err := FirstAttach(context.Background(), req)
	if err != nil {
		t.Fatalf("a disabled-gate policy could not bootstrap: %v", err)
	}
	assertProvisioned(t, g, repo, res.RunID)

	// These are the artefacts the refusal test asserts ABSENT, so a successful attach must create them.
	// Without this, that test could pass over paths nothing ever writes — an absence assertion about a
	// thing that never exists proves nothing at all.
	for _, p := range []string{lay.bootstrapJournal, lay.currentRunDir, filepath.Join(repo, ".claudex", "runs")} {
		if _, serr := os.Stat(p); serr != nil {
			t.Fatalf("a successful attach did not create %s (%v); the refusal test's absence assertions would be vacuous", p, serr)
		}
	}

	// And the persisted policy still compares equal to a fresh parse of its own snapshot — the exact
	// equality the bootstrap intent enforces, asserted against durable bytes rather than in memory.
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
