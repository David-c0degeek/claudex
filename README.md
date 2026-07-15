# claudex

Pair-programming orchestrator for **Claude Code** and **OpenAI Codex**.

Two AI engineers work as **one team on one plan and one implementation**:
the **LEAD** (you choose it at start) drafts the plan, implements, and
fixes; the **PAIR** critiques the plan, reviews every step's commits, and
verifies the result with fresh context. They converge by agreement — not by
facing off and having a winner picked.

An external, deterministic coordinator (Python stdlib only) owns everything
the models must not own: phase order, edit permissions, response budgets,
convergence detection, diff extraction, and the transcript. Neither model
is ever "the boss" of the other.

## The loop

```
INIT
  → PLAN_DRAFT       lead drafts the plan, grounded in the repo (read-only)
  → PLAN_CRITIQUE    pair critiques it against the repo
  → PLAN_REVISE      lead accepts each finding or rebuts it with evidence
        ↺ until the pair AGREEs (zero blocking/major findings); each response
          budget includes the lead's final revision and a fresh plan audit
        content facts that fit an existing step become persistent
        implementation checks instead of forcing another plan rewrite
  → IMPLEMENT_STEP   lead implements exactly one plan step, commits
  → CHECKPOINT       pair reviews that step's exact diff
  → FIX              lead fixes blocking/major findings, commits (loops)
        ↺ next step, until all steps are agreed
  → TESTS            coordinator runs your test command itself — exit code
                     decides, never agent testimony
  → VERIFY           pair with FRESH context checks every acceptance
                     criterion in the task contract
  → DONE             automatically on verify pass
AWAIT_GUIDANCE       actual choice → `claudex resolve --notes "..."`;
                     exhausted quality budget → `claudex continue`
PAUSED_BUDGET        run economic envelope exhausted → explicit
                     `claudex resume --add-...` override
```

**Stop conditions** (the "good enough" metric — they can never loop
forever):
- convergence = pair verdict `AGREE` with zero blocking/major findings;
- plan findings are limited to architecture, scope, step safety, and
  validation strategy; volatile facts/content details are carried forward as
  implementation checks and reviewed against committed artifacts;
- lead-response budgets: plan revisions (1), checkpoint fixes per step (3),
  test fixes (2), verify fixes (2) — all configurable; every budget permits
  the final lead response before a fresh review. Exhaustion is reported as a
  quality-budget stop, not falsely labeled a disagreement;
- a critique can request human guidance early only for a concrete unresolved
  value choice or repeated evidence-backed disagreement;
- mechanical test gate: your configured command must exit 0;
- final verification runs with a fresh session — no shared history with
  the implementation.

## Two ways to run it

### Headless — `claudex run`

The coordinator drives **both** agents as subprocesses through the whole
loop. Provider text, tool activity, warnings, and lifecycle events are printed
while each turn is running; you no longer wait for a buffered turn to discover
whether the provider is active. You come back for gates (if any) and the merge.

```bash
claudex init                       # scaffold .claudex/task.md + config
claudex task "one paragraph..."    # optional: agent-drafted task contract
claudex run --lead claude          # or --lead codex — the lead is YOUR call
claudex run --open-terminals       # optional Claude + Codex watcher windows
# ...
claudex watch --agent all          # replay + tail normalized provider activity
claudex status [RUN_ID]            # lifecycle, usage, reason, exact next action
claudex resolve --notes "..."      # answer a concrete decision gate
claudex resolve --notes-file decision.md  # multiline/shell-safe alternative
claudex continue                   # allow one response + fresh audit, no guidance
claudex continue --responses 2     # larger extension only when explicitly chosen
claudex resume                     # continue this exact durable run/checkpoint
claudex resume --add-invocations 2 # expand an exhausted run envelope explicitly
claudex restart                    # new execution ID, hash-equivalent checkpoint
claudex restart --fresh-plan       # explicitly discard planning state only
claudex cancel                     # kill the active provider/test process tree
claudex export --output run.zip    # compact redacted recovery bundle
git merge claudex/<run_id>         # DONE prints the exact command
claudex clean
```

### Live — `claudex pair` (your session is the lead)

You work inside an interactive Claude Code (or codex) session as the lead;
claudex runs only the **pair's** turns, enforcing the same state machine,
caps, and artifacts.

```bash
claudex init --with-skill          # installs .claude/skills/claudex-pair
claudex pair start                 # you lead; --lead codex if you are codex
claudex pair plan --file plan.json # pair critiques; revise until AGREE
# implement step 1 in the printed worktree, commit, then:
claudex pair checkpoint --notes "step 1: ..."
# ...steps advance automatically on AGREE...
claudex pair verify                # test gate + fresh verification → DONE
```

The installed skill (`.claude/skills/claudex-pair/SKILL.md`) teaches an
interactive Claude session the full protocol; `codex-lead-prompt.md` is the
mirror for codex-led sessions. Two things the skill insists on: run pair
turns in the background (they invoke the other model and can take many
minutes), and remember the worktree is a **sibling
directory** of your repo (grant access with `--add-dir` or work from a
shell).

## Live events, attempt logs, and the mailbox

Every provider invocation gets an immutable attempt directory:

```
.claudex/runs/<run_id>/attempts/<attempt_id>/
  command.json
  stdout.jsonl
  stderr.log
  events.jsonl
  result.json
  summary.json
```

`stdout.jsonl` and `stderr.log` retain redacted provider streams subject to the
configured raw age/byte limits. `events.jsonl`, `result.json`, and `summary.json`
are compact recovery evidence and are not removed by raw retention. `events.jsonl` is
the provider-neutral, append-only live journal used by the coordinator's console
view and status projection. It contains phase/attempt identity, ordered event
kinds, elapsed time, tool lifecycle, and provider-reported usage/cost when
available. Unknown provider events remain in the raw stream and are surfaced
without breaking final result parsing.

Every completed coordinator turn—plans, critiques, commits, test results,
guidance, DONE—is also summarized by the coordinator in
`.claudex/runs/<run_id>/mailbox.md` using an append-only block format:

```
===== [LEAD] turn 3 | plan | STATUS: REVISE =====
plan round 1: ...\plan-round-1.json
steps (4):
  1. ...
----- end [LEAD] turn 3 -----

===== [PAIR] turn 4 | plan | STATUS: AGREE =====
no findings
----- end [PAIR] turn 4 -----
```

The mailbox is a concise turn ledger, not the live provider transcript. The
initiating terminal and attempt event journal show in-flight work; every mailbox
claim is backed by a typed JSON artifact in the run directory.

`claudex watch [RUN_ID] --agent claude|codex|all` replays those normalized
events and follows new attempts. `--no-follow` is snapshot-friendly; `--raw`
emits stable redacted JSONL; `--color never` is suitable for logs. A final
`[CLAUDEX][STATE]` record explains the lifecycle, reason, and next action.
Watcher exit codes are 0 for normal/paused/completed views, 75 for a rate-limit
stop, 1 for failure, and 130 for cancellation or Ctrl+C. Watch and status reads
never migrate or otherwise rewrite run state. On Windows,
`run --open-terminals` opens two Windows Terminal views. The panes are readers:
closing one never cancels or changes the run. On unsupported/headless systems,
Claudex prints the two equivalent watcher commands.

## Roles

| | LEAD (you pick at start) | PAIR (the other one) |
|---|---|---|
| plan | drafts, revises, rebuts with evidence | critiques against the repo |
| code | implements one step per commit, fixes | reviews each step's exact diff |
| finish | — | fresh-context verification |
| write access | worktree only, implement/fix turns only | never |

The lead is whoever you start the run with — `--lead claude|codex`
(headless) or which session you lead from (live). No rotation, no
winner-picking: ownership is resolved by initiation.

## Enforcement, not prompt discipline

| guarantee | mechanism |
|---|---|
| critiques/reviews can't edit | Claude plan mode with only Read/Glob/Grep; Codex `-s read-only` |
| implementation isolated | dedicated git worktree (sibling dir) on a run branch plus provider permission boundaries |
| reviews see exact code | coordinator extracts `git diff`; tracked and untracked dirty content is refused and exact tree identities are recorded |
| structured findings | Claude `--json-schema`, Codex `--output-schema` (strict mode) |
| provider work is visible | streamed Claude/Codex JSONL normalized into an append-only per-attempt event journal |
| retries preserve evidence | every invocation has a unique immutable attempt directory |
| convergence is checkable | `AGREE` + zero blocking/major, evaluated by the coordinator |
| planning stays planning | plan-level finding categories are schema-constrained; content obligations persist separately as implementation checks |
| inconclusive reviews don't cause churn | tool/evidence failures retry the reviewer fresh without charging a lead response |
| review context stays bounded | fresh plan reviewers receive hashed size-capped manifests, canonical plan/decision/finding ledgers, and selected repo files—never a critique glob |
| no infinite loops | explicit lead-response budgets plus a final fresh-context audit |
| economic admission | durable run caps are checked before every provider call; Claude also receives its native USD/turn caps |
| conservative provider policy | phase capabilities; narrow Claude tools, Codex sandbox/network controls, and provider sub-agents disabled by default |
| guidance stays binding | persistent guidance ledger included in every later agent turn |
| crash safety | state.json written after every round; artifact-presence skip on retry |
| no cross-process races | run-dir lockfile around every state-mutating command |
| verification is independent | verify turn never resumes any session |
| test results are real | coordinator streams `test_command` through the same timeout/cancel lifecycle; exit code and unchanged exact tree decide |

A Git worktree is an isolation boundary for commits, not an OS security sandbox.
Claude tool restrictions and the Codex sandbox reduce provider capabilities, but
they do not make an untrusted repository safe to execute. Review the repository
and the configured mechanical test command before running Claudex.

Session continuity without contamination: planning/revision reviewers and the
final verifier are fresh and consume coordinator-built evidence. Only
`lead_impl` and `pair_review` may resume inside the unchanged worktree; resume
pins the original cwd, so those lineages never cross.

The default planning ceiling is one draft, one critique, one hash-guarded
section-replacement revision, and at most one conditional fresh audit. A human
gate exists only when structured output contains both explicit
`requires_human_decision: true` and a concrete non-empty question; finding
labels cannot infer a gate. Inconsistent combinations are retryable provider
protocol failures.

Run state also has a durable macro lifecycle (`running`, `paused`,
`paused_budget`, `rate_limited`, `cancelled`, `failed_retryable`,
`failed_terminal`, or `completed`). Every lifecycle entry records the phase,
attempt, reason, timestamp, and exact resume instruction. `claudex resume`
continues the same run ID. `restart` creates a new execution ID only after a
hash-equivalence check of the copied canonical checkpoint—including decisions,
findings, checks, budgets, worktree/base identity, and safe sessions. State
migrations and restart retain an original-state backup; only `--fresh-plan`
deliberately removes planning artifacts.

Provider rate limits create an immutable failed attempt and durable
`rate_limited` lifecycle with an approximate reset time, then return to the
shell (exit 75) by default. Run `claudex resume` when ready. Set
`wait_on_limits` only when autonomous waiting is intentional; Claudex releases
the run lock while waiting and `claudex cancel` remains effective. Cancellation
is idempotent, terminates the complete active process tree, keeps partial
evidence, and exits a driving command with 130.

## Report mode

Start the task contract's goal with `[REPORT]` (or `--mode report`) when
the deliverable IS analysis: the report draft becomes the single step —
lead writes and commits it in the worktree, the pair critiques it through
the same checkpoint loop, fresh verification checks the contract. No
plan-about-the-work layer.

## Configuration

`.claudex/config.json` (written by `claudex init`, overridable per-run by
flags):

```jsonc
{
  "config_schema_version": 2,
  "lead": "claude",             // who holds the pen by default
  "mode": "auto",               // auto | change | report
  "max_plan_rounds": 1,       // one revision + one conditional fresh audit
  "max_checkpoint_rounds": 3, // fix budget per step
  "max_test_rounds": 2,       // fixes after test failures
  "max_verify_rounds": 2,     // fixes after verification failures
  "test_command": "",           // mechanical gate, run by the coordinator
  "agent_timeout": 3600,
  "max_invocation_cost_usd": 3.0, // native Claude cap; Codex admission only
  "max_invocation_turns": 12,
  "max_invocation_output_tokens": 50000,
  "max_invocation_tool_calls": 50,
  "max_run_invocations": 20,
  "max_run_input_tokens": 5000000,
  "max_run_output_tokens": 250000,
  "max_run_cost_usd": 12.0,
  "max_run_tool_calls": 250,
  "max_run_wall_seconds": 10800,
  "planning_effort": "high",
  "implementation_effort": "high",
  "verification_effort": "high",
  "claude_planning_model": "", // empty falls back to claude_model
  "codex_planning_model": "",  // implementation/verification variants exist
  "max_evidence_bytes": 262144,
  "max_evidence_file_bytes": 98304,
  "max_evidence_requests": 8,
  "disable_nested_agents": true,
  "allow_expensive_profiles": false,
  "wait_on_limits": false,      // opt in to lock-free autonomous limit waits
  "raw_retention_days": 14,     // raw stdout/stderr/last-message only
  "raw_retention_bytes": 104857600,
  "claude_model": "",           // pin models if you want reproducibility
  "codex_model": ""
}
```

The effective run envelope is frozen into `state.json` when a run starts, so
later config edits cannot silently enlarge or shrink it. Claudex admits a call
only while every durable cap has room. A call can still report an overshoot
after it finishes; that terminal result is charged once and the next call is
blocked as `PAUSED_BUDGET`. Resume requires naming the added capacity, for
example `claudex resume --add-input-tokens 100000`. Non-USD costs are retained
without conversion and require an explicit `--acknowledge-currency CODE` before
work continues.

Token and cost labels are deliberately literal: status shows provider-reported
categories and USD cost, plus counts of attempts whose usage, tools, or cost
were not reported. Claudex does not scrape prices or relabel estimates as
provider facts. Maximum-cost effort values (`xhigh` or `max`) require
`allow_expensive_profiles`; nested agents require
`--allow-nested-agents`.

Each provider can select a model per `planning`, `implementation`, and
`verification` profile (for example `claude_verification_model` or the matching
`--claude-verification-model` flag); an empty phase value falls back to the
provider's global model. The run-start summary prints every effective
model/effort profile and marks maximum-effort opt-ins prominently.

Binary discovery: `CLAUDEX_CLAUDE_BIN` / `CLAUDEX_CODEX_BIN` env vars win and
fail closed if that explicit binary lacks required capabilities. Otherwise
Claudex probes PATH and installed desktop candidates, selects the highest
compatible semantic version deterministically, and reports its stream, schema,
budget, sandbox, session, and nested-agent matrix in `claudex doctor`.

Config files use schema 2. Loading schema 1 retains `config.v1.bak.json` and
migrates only the exact historical broad Claude-tool default; a custom broad
shell grant stops for review. Run state uses schema 6 and keeps
`state.vN.bak.json` on migration. Future schemas fail closed. Before manual
rollback or support work, `claudex export [RUN_ID] --output run.zip` captures
state, decisions, manifests, events, results, and summaries while excluding raw
stdout/stderr/last-message streams, rollback backups, control files, and
symlinks. Text is redacted again at the export boundary.

## Operator recovery

- `rate_limited` (exit 75): check the reset in `status`, then `resume`; no
  default command sleeps in the foreground.
- `paused_budget`: add only the named capacity with `resume --add-...`.
- decision gate: answer the exact question with `resolve`; use `continue` only
  for a quality-response budget.
- retryable failure: inspect the immutable attempt summary, then `retry`.
- active or idle cancellation: `cancel`; partial evidence and the worktree stay.
- replacement identity: `restart`; use `--fresh-plan` only to discard planning
  state before implementation exists.
- migration uncertainty: preserve the generated `.bak.json` and export the run
  before changing files manually.

The architecture and durable rationale are documented in
[`docs/architecture.md`](docs/architecture.md) and
[`docs/decisions.md`](docs/decisions.md).

## Install

```bash
pip install -e .
claudex doctor        # checks git + both CLIs
```

Requires Python 3.10+, git, `claude` CLI, `codex` CLI. No third-party
Python dependencies.

The paid compatibility smoke is deliberately excluded from normal tests. It
runs capability preflight before any invocation and refuses safely while either
provider lacks a native spend cap (current Codex reports
`budget=coordinator-only`). If both providers can enforce the contract, it also
disables nested/tool work and applies a $0.10 / 120-second coordinator envelope:

```powershell
$env:CLAUDEX_RUN_LIVE_SMOKE="1"
$env:CLAUDEX_LIVE_SMOKE_ACK="I_ACCEPT_CAPPED_PROVIDER_COSTS"
python -m unittest tests.test_live_smoke -v
```

## Design decisions

- **Pair, not face-off.** One plan, co-owned. The pair's AGREE means "I
  co-own this plan and its implementation", not "you win".
- **Lead by initiation.** Whoever you start with holds the pen. This
  resolves ownership without rotation schemes or model-vs-model authority.
- **Agreement with teeth.** AGREE is only accepted with zero blocking/major
  findings. Complete revision/fix cycles have explicit budgets. A real choice
  becomes persistent human guidance; simple budget exhaustion can continue
  without inventing a decision.
- **The coordinator is code, not a model.** Phase transitions, caps,
  diffs, test results, and convergence are computed deterministically.
  Models argue; the state machine decides what happens next.
- **Evidence beats testimony.** Rebuttals require file-level evidence;
  the verifier treats all prior claims as unverified; the test gate is a
  subprocess exit code.
