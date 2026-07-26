package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/state"
)

// TestPullE2E drives a real pull through the CLI: the lead's PLAN_DRAFT turn is projected with its
// hash-bound evidence packet, mirrored to the session inbox, and is byte-identical on repeat — pull
// mints nothing, so two pulls of the same state agree exactly.
func TestPullE2E(t *testing.T) {
	repo, runID, lead := bootstrapPair(t)
	turnID, rev := currentTurn(t, repo, runID)

	var out, errb bytes.Buffer
	if code := run(context.Background(),
		[]string{"pull", "--repo", repo, "--run", runID, "--session", lead}, &out, &errb); code != 0 {
		t.Fatalf("pull: exit %d, %s", code, errb.String())
	}
	var a struct {
		MessageType           string `json:"message_type"`
		RunID                 string `json:"run_id"`
		SessionID             string `json:"session_id"`
		TurnID                string `json:"turn_id"`
		Role                  string `json:"role"`
		Phase                 string `json:"phase"`
		ExpectedStateRevision uint64 `json:"expected_state_revision"`
		ArtifactMessageType   string `json:"artifact_message_type"`
		Worktree              *string
		Evidence              *struct {
			ManifestRelPath string `json:"manifest_rel_path"`
			RootDigest      string `json:"root_digest"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(out.Bytes(), &a); err != nil {
		t.Fatalf("decode assignment: %v (%s)", err, out.String())
	}
	if a.MessageType != "assignment" || a.RunID != runID || a.SessionID != lead ||
		a.TurnID != turnID || a.ExpectedStateRevision != rev {
		t.Fatalf("assignment identity = %+v (want turn %s at rev %d)", a, turnID, rev)
	}
	if a.Role != "lead" || a.Phase != "PLAN_DRAFT" || a.ArtifactMessageType != "plan" {
		t.Fatalf("assignment contract = %+v", a)
	}
	// PLAN_DRAFT is read-only: the turn is actionable through a hash-bound packet, never a worktree.
	if a.Worktree != nil {
		t.Fatalf("a read-only turn must not carry a worktree: %+v", a.Worktree)
	}
	if a.Evidence == nil || a.Evidence.ManifestRelPath == "" || len(a.Evidence.RootDigest) != 64 {
		t.Fatalf("assignment evidence = %+v", a.Evidence)
	}

	// The bound packet is the one run state named, and it exists on disk.
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve run: %v", err)
	}
	rs, ok, err := state.Open(loc.StateDir, loc.RunLock).Load()
	if err != nil || !ok {
		t.Fatalf("load state: ok=%v err=%v", ok, err)
	}
	if rs.Evidence == nil || rs.Evidence.ManifestRelPath != a.Evidence.ManifestRelPath ||
		rs.Evidence.RootDigest != a.Evidence.RootDigest {
		t.Fatalf("projected evidence %+v disagrees with the state binding %+v", a.Evidence, rs.Evidence)
	}
	if _, serr := os.Stat(filepath.Join(loc.EvidenceDir, a.Evidence.ManifestRelPath)); serr != nil {
		t.Fatalf("the bound packet manifest is not on disk: %v", serr)
	}

	// The session inbox carries the same assignment: it is a mirror, not a second authority.
	inbox, err := os.ReadFile(filepath.Join(loc.SessionDir, lead, "assignment.json"))
	if err != nil {
		t.Fatalf("read session inbox: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(inbox), bytes.TrimSpace(out.Bytes())) {
		t.Fatalf("inbox and stdout disagree:\n inbox %s\nstdout %s", inbox, out.Bytes())
	}

	// Pull mints nothing, so a second pull of unchanged state is byte-identical.
	var out2, errb2 bytes.Buffer
	if code := run(context.Background(),
		[]string{"pull", "--repo", repo, "--run", runID, "--session", lead}, &out2, &errb2); code != 0 {
		t.Fatalf("second pull: exit %d, %s", code, errb2.String())
	}
	if !bytes.Equal(out.Bytes(), out2.Bytes()) {
		t.Fatalf("pull is not deterministic:\n first %s\nsecond %s", out.Bytes(), out2.Bytes())
	}
}

// The pair holds no turn at PLAN_DRAFT. That is an ordinary answer on a healthy run, so it is an
// operational exit with a plain statement of what is true — not a crash, and not success.
func TestPullNotThisSessionsTurn(t *testing.T) {
	repo, runID, lead := bootstrapPair(t)
	pair := otherSession(t, repo, runID, lead)

	var out, errb bytes.Buffer
	code := run(context.Background(),
		[]string{"pull", "--repo", repo, "--run", runID, "--session", pair}, &out, &errb)
	if code != 1 {
		t.Fatalf("pull as the non-owning session = exit %d, want 1 (%s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "belongs to the other session") {
		t.Fatalf("stderr = %q, want a not-your-turn statement", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("a refused pull must emit no assignment, got %s", out.String())
	}
}

func TestPullUsageErrors(t *testing.T) {
	repo, runID, lead := bootstrapPair(t)
	cases := map[string][]string{
		"missing session":   {"pull", "--repo", repo, "--run", runID},
		"malformed session": {"pull", "--repo", repo, "--run", runID, "--session", "not-a-session"},
		// A run id that breaks the safe-name grammar is a usage error; a well-formed id that simply
		// does not bind to an active run is operational, which TestPullUnknownRun covers.
		"malformed run":       {"pull", "--repo", repo, "--run", "../escape", "--session", lead},
		"positional argument": {"pull", "--repo", repo, "--run", runID, "--session", lead, "extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run(context.Background(), args, &out, &errb); code != 2 {
				t.Fatalf("%s = exit %d, want 2 (usage); stderr=%s", name, code, errb.String())
			}
		})
	}
}

// A well-formed run id that does not bind to an active run is an operational failure, not a usage
// error: the caller spelled it correctly, the repository just has no such run.
func TestPullUnknownRun(t *testing.T) {
	repo, _, lead := bootstrapPair(t)
	var out, errb bytes.Buffer
	if code := run(context.Background(),
		[]string{"pull", "--repo", repo, "--run", "run-unknown", "--session", lead}, &out, &errb); code != 1 {
		t.Fatalf("unknown run = exit %d, want 1 (operational); stderr=%s", code, errb.String())
	}
}

// otherSession returns the registered session that is not the given one.
func otherSession(t *testing.T, repo, runID, known string) string {
	t.Helper()
	loc, err := attach.ResolveRun(repo, runID)
	if err != nil {
		t.Fatalf("resolve run: %v", err)
	}
	reg, ok, err := state.OpenRegistry(loc.RegistryDir, loc.RunLock).Load()
	if err != nil || !ok {
		t.Fatalf("load registry: ok=%v err=%v", ok, err)
	}
	for _, slot := range []*state.RoleSlot{reg.Lead, reg.Pair} {
		if slot != nil && slot.CurrentSessionID != known {
			return slot.CurrentSessionID
		}
	}
	t.Fatal("the run has no second registered session")
	return ""
}
