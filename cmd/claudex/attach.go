package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/David-c0degeek/claudex/internal/attach"
	"github.com/David-c0degeek/claudex/internal/gitx"
	"github.com/David-c0degeek/claudex/internal/state"
)

// maxInputFile bounds a --task/--config read so a hostile or accidental huge file cannot
// exhaust memory before the library validates it.
const maxInputFile = 1 << 20

// attachCmd dispatches the four mutually-exclusive attach modes over the attach-protocol
// library: first (create a run), join (fill the pair slot), reattach (read-only session
// resolution), and replace (same-role takeover). It emits the outcome as one line of JSON on
// stdout. Exit codes: 0 success, 1 operational error, 2 usage error.
func attachCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		repo       = fs.String("repo", ".", "repository root")
		run        = fs.String("run", "", "run id (join/reattach/replace)")
		agent      = fs.String("agent", "", "agent: claude|codex")
		role       = fs.String("role", "", "role: lead|pair")
		task       = fs.String("task", "", "path to the task-contract file (first attach)")
		config     = fs.String("config", "", "path to the run-policy/config file (first attach)")
		opID       = fs.String("operation-id", "", "caller-stable idempotency key (op-<32 hex>) for a mutating attach")
		sessionID  = fs.String("session-id", "", "existing session id (reattach)")
		expectGen  = fs.Uint64("expected-generation", 0, "expected current generation (replace)")
		doReattach = fs.Bool("reattach", false, "read-only: resolve whether a session is still current")
		doReplace  = fs.Bool("replace", false, "same-role takeover with an expected generation")
	)
	if err := fs.Parse(args); err != nil {
		return 2 // flag package already wrote the error + usage to stderr
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "claudex: attach takes no positional arguments, got %v\n", fs.Args())
		return 2
	}
	// Track EXPLICITLY set flags so each mode can reject any flag outside its exact allowed
	// set (a silently-ignored irrelevant flag is a usage error, not a different mode).
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	if *doReattach && *doReplace {
		fmt.Fprintln(stderr, "claudex: attach: --reattach and --replace are mutually exclusive")
		return 2
	}

	switch {
	case *doReattach:
		if !enforceFlags(stderr, "--reattach", visited, "reattach", "repo", "run", "session-id", "agent", "role") {
			return 2
		}
		return attachReattach(*repo, *run, *sessionID, *agent, *role, stdout, stderr)
	case *doReplace:
		if !enforceFlags(stderr, "--replace", visited, "replace", "repo", "run", "agent", "role", "expected-generation", "operation-id") {
			return 2
		}
		return attachReplace(*repo, *run, *agent, *role, *expectGen, *opID, stdout, stderr)
	case *run != "":
		if !enforceFlags(stderr, "(join)", visited, "repo", "run", "agent", "role", "operation-id") {
			return 2
		}
		return attachJoin(*repo, *run, *agent, *role, *opID, stdout, stderr)
	default:
		if !enforceFlags(stderr, "(first)", visited, "repo", "agent", "role", "task", "config", "operation-id") {
			return 2
		}
		return attachFirst(ctx, *repo, *agent, *role, *task, *config, *opID, stdout, stderr)
	}
}

// enforceFlags rejects any explicitly-set flag outside the mode's exact allowed set.
func enforceFlags(stderr io.Writer, mode string, visited map[string]bool, allowed ...string) bool {
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	for name := range visited {
		if !ok[name] {
			fmt.Fprintf(stderr, "claudex: attach %s does not accept --%s\n", mode, name)
			return false
		}
	}
	return true
}

// --- shared parsing/output helpers ---

func parseAgent(s string, stderr io.Writer) (state.Agent, bool) {
	switch state.Agent(s) {
	case state.AgentClaude, state.AgentCodex:
		return state.Agent(s), true
	default:
		fmt.Fprintf(stderr, "claudex: attach: --agent must be claude or codex, got %q\n", s)
		return "", false
	}
}

func parseRole(s string, stderr io.Writer) (state.SlotRole, bool) {
	switch state.SlotRole(s) {
	case state.SlotLead, state.SlotPair:
		return state.SlotRole(s), true
	default:
		fmt.Fprintf(stderr, "claudex: attach: --role must be lead or pair, got %q\n", s)
		return "", false
	}
}

// emitJSON writes v as one line of JSON + newline. A marshal or write failure is exit 1.
func emitJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(stderr, "claudex: attach: encode result: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", b); err != nil {
		fmt.Fprintf(stderr, "claudex: write failed: %v\n", err)
		return 1
	}
	return 0
}

// readInput reads a required --task/--config file, bounded and regular-file-checked.
func readInput(flagName, path string, stderr io.Writer) ([]byte, bool) {
	if path == "" {
		fmt.Fprintf(stderr, "claudex: attach (first) requires %s\n", flagName)
		return nil, false
	}
	if fi, err := os.Lstat(path); err != nil {
		fmt.Fprintf(stderr, "claudex: attach: %s: %v\n", flagName, err)
		return nil, false
	} else if !fi.Mode().IsRegular() {
		fmt.Fprintf(stderr, "claudex: attach: %s must be a regular file\n", flagName)
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "claudex: attach: %s: %v\n", flagName, err)
		return nil, false
	}
	defer f.Close()
	// Re-check regularity on the OPENED handle: a symlink/FIFO/device swapped in between the
	// Lstat and the Open cannot slip through (the size cap alone would not stop a blocking FIFO).
	if hi, err := f.Stat(); err != nil {
		fmt.Fprintf(stderr, "claudex: attach: %s: %v\n", flagName, err)
		return nil, false
	} else if !hi.Mode().IsRegular() {
		fmt.Fprintf(stderr, "claudex: attach: %s must be a regular file\n", flagName)
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxInputFile+1))
	if err != nil {
		fmt.Fprintf(stderr, "claudex: attach: %s: %v\n", flagName, err)
		return nil, false
	}
	if len(b) > maxInputFile {
		fmt.Fprintf(stderr, "claudex: attach: %s exceeds %d bytes\n", flagName, maxInputFile)
		return nil, false
	}
	return b, true
}

// --- first attach ---

func attachFirst(ctx context.Context, repo, agentS, roleS, task, config, opID string, stdout, stderr io.Writer) int {
	ag, ok := parseAgent(agentS, stderr)
	if !ok {
		return 2
	}
	// First attach always chooses lead; --role is optional but if given must be lead.
	if roleS != "" {
		if r, ok := parseRole(roleS, stderr); !ok {
			return 2
		} else if r != state.SlotLead {
			fmt.Fprintln(stderr, "claudex: attach (first) is always the lead; --role pair is a join (pass --run)")
			return 2
		}
	}
	if !state.IsOperationID(opID) {
		fmt.Fprintln(stderr, "claudex: attach (first) requires --operation-id op-<32 lower-hex>")
		return 2
	}
	taskBytes, ok := readInput("--task", task, stderr)
	if !ok {
		return 2
	}
	configBytes, ok := readInput("--config", config, stderr)
	if !ok {
		return 2
	}

	g, err := gitx.New()
	if err != nil {
		fmt.Fprintf(stderr, "claudex: attach: git: %v\n", err)
		return 1
	}
	defer g.Close()
	base, pre, wt := attach.NewGitSeams(g)
	res, err := attach.FirstAttach(ctx, attach.FirstAttachRequest{
		RepoDir: repo, Agent: ag, OperationID: opID,
		TaskCanonical: taskBytes, PolicyCanonical: configBytes,
		CreatedUnix: time.Now().Unix(), RNG: rand.Reader,
		Base: base, Preflight: pre, Worktree: wt,
	})
	if err != nil {
		fmt.Fprintf(stderr, "claudex: attach (first): %v\n", err)
		return 1
	}

	// The join command is complete and stable in the library result (the pair join op id is
	// frozen unguessable in the bootstrap intent, and the repo locator is canonical), so a
	// same-first-op replay advertises a byte-identical join command. Emit it verbatim.
	return emitJSON(stdout, stderr, map[string]any{
		"mode":         "first",
		"run_id":       res.RunID,
		"session_id":   res.SessionID,
		"role":         res.Role,
		"agent":        res.Agent,
		"join_command": res.JoinArgv,
	})
}

// --- join ---

func attachJoin(repo, run, agentS, roleS, opID string, stdout, stderr io.Writer) int {
	ag, ok := parseAgent(agentS, stderr)
	if !ok {
		return 2
	}
	r, ok := parseRole(roleS, stderr)
	if !ok {
		return 2
	}
	if r != state.SlotPair {
		fmt.Fprintln(stderr, "claudex: attach --run is a pair join; pass --role pair")
		return 2
	}
	if !state.IsRunID(run) {
		fmt.Fprintln(stderr, "claudex: attach (join) requires a canonical --run id")
		return 2
	}
	if !state.IsOperationID(opID) {
		fmt.Fprintln(stderr, "claudex: attach (join) requires --operation-id op-<32 lower-hex>")
		return 2
	}
	res, err := attach.JoinAttach(attach.JoinAttachRequest{
		RepoDir: repo, RunID: run, OperationID: opID,
		Agent: ag, Role: state.SlotPair, Now: time.Now().Unix(), RNG: rand.Reader,
	})
	if err != nil {
		fmt.Fprintf(stderr, "claudex: attach (join): %v\n", err)
		return 1
	}
	return emitJSON(stdout, stderr, map[string]any{
		"mode":          "join",
		"run_id":        res.RunID,
		"session_id":    res.SessionID,
		"role":          res.Role,
		"agent":         res.Agent,
		"first_turn_id": res.FirstTurnID,
	})
}

// --- reattach (read-only) ---

func attachReattach(repo, run, sessionID, agentS, roleS string, stdout, stderr io.Writer) int {
	ag, ok := parseAgent(agentS, stderr)
	if !ok {
		return 2
	}
	r, ok := parseRole(roleS, stderr)
	if !ok {
		return 2
	}
	if !state.IsRunID(run) {
		fmt.Fprintln(stderr, "claudex: attach --reattach requires a canonical --run id")
		return 2
	}
	if !state.IsSessionID(sessionID) {
		fmt.Fprintln(stderr, "claudex: attach --reattach requires an existing --session-id")
		return 2
	}
	res, err := attach.Reattach(attach.ReattachRequest{
		RepoDir: repo, RunID: run, SessionID: sessionID, Agent: ag, Role: r,
	})
	if err != nil {
		fmt.Fprintf(stderr, "claudex: attach (reattach): %v\n", err)
		return 1
	}
	return emitJSON(stdout, stderr, map[string]any{
		"mode":               "reattach",
		"status":             res.Status,
		"run_id":             res.RunID,
		"session_id":         res.SessionID,
		"role":               res.Role,
		"agent":              res.Agent,
		"current_generation": res.CurrentGeneration,
	})
}

// --- replace (same-role takeover) ---

func attachReplace(repo, run, agentS, roleS string, expectGen uint64, opID string, stdout, stderr io.Writer) int {
	ag, ok := parseAgent(agentS, stderr)
	if !ok {
		return 2
	}
	r, ok := parseRole(roleS, stderr)
	if !ok {
		return 2
	}
	if !state.IsRunID(run) {
		fmt.Fprintln(stderr, "claudex: attach --replace requires a canonical --run id")
		return 2
	}
	if expectGen == 0 {
		fmt.Fprintln(stderr, "claudex: attach --replace requires --expected-generation > 0")
		return 2
	}
	if !state.IsOperationID(opID) {
		fmt.Fprintln(stderr, "claudex: attach --replace requires --operation-id op-<32 lower-hex>")
		return 2
	}
	res, rerr := attach.ReplaceAttach(attach.ReplaceRequest{
		RepoDir: repo, RunID: run, Role: r, Agent: ag,
		ExpectedGeneration: expectGen, OperationID: opID, RNG: rand.Reader,
	})
	if rerr != nil && !errors.Is(rerr, attach.ErrReplaceOutcomeUnknown) {
		fmt.Fprintf(stderr, "claudex: attach (replace): %v\n", rerr)
		return 1
	}
	out, warnings, postEmitExit := classifyReplaceResult(res, rerr)
	for _, w := range warnings {
		fmt.Fprintf(stderr, "claudex: attach (replace) WARNING: %s\n", w)
	}
	if code := emitJSON(stdout, stderr, out); code != 0 {
		return code // a write/encode failure dominates
	}
	return postEmitExit
}

// classifyReplaceResult maps a replace outcome to its emitted JSON, the distinct stderr
// warnings, and the post-emit exit code. ErrReplaceOutcomeUnknown is recovery-required
// (exit 1 after emitting the candidate), per D6's 0-success/1-operational contract; a
// proven-committed CommitWarning is an authoritative success-with-warning (exit 0). Pure, so
// the exit/output contract is unit-testable without a real replace.
func classifyReplaceResult(res attach.ReplaceResult, rerr error) (out map[string]any, warnings []string, postEmitExit int) {
	out = map[string]any{
		"mode":             "replace",
		"run_id":           res.RunID,
		"session_id":       res.SessionID,
		"role":             res.Role,
		"agent":            res.Agent,
		"generation":       res.Generation,
		"verifier_turn_id": res.VerifierTurnID,
	}
	if errors.Is(rerr, attach.ErrReplaceOutcomeUnknown) {
		out["outcome_unknown"] = true
		warnings = append(warnings, fmt.Sprintf("outcome unknown, session is a recovery candidate: %v", rerr))
		postEmitExit = 1
	}
	if res.CommitWarning != nil {
		out["commit_warning"] = res.CommitWarning.Error()
		warnings = append(warnings, res.CommitWarning.Error())
	}
	return out, warnings, postEmitExit
}
