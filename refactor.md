# Claudex Reliability and Observability Refactor

Status: proposed implementation specification

Date: 2026-07-15

Scope: Claudex orchestration, provider execution, recovery, safety, testing, and operator UX

## Executive verdict

Claudex has a sound core idea—a coordinator-owned state machine that gives Claude and Codex bounded roles inside an isolated Git worktree—but the current implementation is not dependable enough for routine use. It hides provider activity until a turn ends, has no effective token or cost envelope, can convert an explicit “no human decision required” response into a blocking gate, and treats restart as a new planning attempt instead of a faithful continuation. Those defects combine into long, expensive runs that appear idle and may finish without advancing the task.

The refactor must make execution observable, bounded, recoverable, and testable before adding more autonomous behavior. A successful result is not “more agent discussion.” It is a short, inspectable protocol that spends a known amount, leaves a durable trail, resumes without losing accepted work, and either produces a verified code change or stops with a precise machine-readable reason.

## Evidence from the incident

The triggering run in `D:\repos\ai-nemo-engineer\.claudex\runs` produced two retained attempts:

- The original attempt completed 12 critiques and 11 revisions, then stopped before implementation.
- Its restart completed one Claude plan draft and one Codex critique, then raised a false human-decision gate.
- Across the retained artifacts, Codex consumed at least 96,473,117 input tokens (87,373,312 cached) and 1,003,097 output tokens. Claude recorded 8,952,998 cached input tokens, 1,056,917 cache-creation input tokens, 372,248 output tokens, and $31.22 of reported cost.
- The restarted plan draft took about 760 seconds and cost about $5.93. Its critique took about 537 seconds, used 2,718,615 input tokens, made 18 MCP calls, and performed 8 web searches.
- The restart also spent roughly 35 minutes sleeping on a rate limit. The whole restart took about 63 minutes to reach the false gate.
- Failed retry logs can be overwritten under the same phase label, so these figures are a lower bound.

This evidence is diagnostic rather than a benchmark: the exact totals depend on provider accounting and cache semantics. The important facts are the repeated context growth, lack of live visibility, unbounded high-effort calls, foreground rate-limit sleep, and failure to preserve the best known plan across restart.

## What should be preserved

The refactor should retain these existing strengths:

- Coordinator-owned phase transitions rather than allowing a provider to mutate orchestration state directly.
- Separate lead, pair, implementer, tester, and verifier responsibilities.
- Typed structured results and explicit schemas.
- A dedicated Git worktree for implementation.
- Read-only reviewer operation and a mechanical test command.
- Immutable task snapshots and append-only human-readable run artifacts.
- A fresh final verifier after implementation.
- A small standard-library-oriented Python codebase.

The redesign should strengthen these boundaries, not replace them with a free-form multi-agent chat.

## Root causes

### 1. Provider output is buffered until completion

`claudex/agents.py` starts providers with piped stdout and stderr and then waits in `communicate()`. Operator logs and mailbox entries are therefore written after the provider exits. A 10-minute reasoning turn is indistinguishable from a deadlocked process.

Both installed providers expose streamable machine formats. Codex supports JSONL from `codex exec --json`; Claude supports streamed JSON with partial messages. Claudex is discarding the exact signal needed for a live UI, progress metrics, cancellation, and budget enforcement.

### 2. The coordinator does not own an economic envelope

Provider results do not carry normalized token, cost, effort, tool-call, or timing information. Configuration does not establish per-invocation or per-run budgets, Claude's provider-side budget option is unused, and status output cannot explain what was spent. The incident also combined an expensive model with `xhigh` reasoning without a prominent warning or confirmation.

A round count is not a budget. One review can be more expensive than ten small implementation steps.

### 3. Human-decision gating is semantically wrong

The result schema can explicitly report `requires_human_decision: false` with no question, but `CritiqueResult.requires_human_decision()` also infers a gate from any blocking or major finding whose kind is `decision`. The incident critique did exactly that: it explicitly declined a human gate, while five ordinary design findings caused the coordinator to stop anyway.

An inferred finding category must never override an explicit protocol field. A gate is valid only when the provider explicitly requests it and supplies a concrete, non-empty question that the user can answer.

### 4. Restart loses high-value state

The restart state preserves binding guidance, mode, and lead role, but discards the latest plan, critique, checks, and accepted findings. The provider must then reconstruct work from prose and old artifacts, causing plan regression and duplicated investigation.

Restart must mean “continue from the latest durable checkpoint.” Starting a clean run is a separate operation.

### 5. Review context grows without control

Plan sessions are resumed, each revision re-emits the complete plan, and reviewers are pointed at a growing glob of prior critiques while being asked to re-verify the repository and external facts. In the incident, later critiques grew from roughly 2.4 million to roughly 15 million Codex input tokens.

The coordinator should assemble a bounded evidence packet: task snapshot, current plan, unresolved findings, decision ledger, relevant diff, and selected verification output. Reviewers should be fresh by default and should inspect deltas plus unresolved risks. Nested provider agents should be disabled unless a task explicitly opts in, because they multiply context and spend outside the coordinator's accounting model.

### 6. Rate limits and cancellation are not durable states

Rate limits currently cause the foreground process to sleep while holding the run lock. Defaults allow up to 12 waits, each potentially lasting hours. The user cannot reliably cancel a process tree, distinguish a paused run from a hung run, or resume without losing an attempt log.

Rate limiting should persist a `RATE_LIMITED` state and exit promptly. Autonomous waiting may exist as an explicit opt-in mode, never the default. `cancel` must terminate the full provider process tree and persist `CANCELLED`.

### 7. Git and shell isolation have gaps

Git checks use `git status --porcelain -uno`, so untracked files are invisible to convergence and checkpoint logic. A test can use an untracked file that is never included in the checkpoint or final merge.

Claude write mode also allows unrestricted shell commands under an edit-acceptance mode. A worktree controls the intended working directory; it does not create an operating-system security boundary. Provider permissions must be capability-based and as narrow as the task permits.

### 8. Tests validate orchestration branches, not provider reality

The current unit suite is fast, but most provider calls are mocked. It lacks subprocess stream fixtures, malformed-event handling, usage accounting, process-tree cancellation, foreground rate-limit behavior, restart-baseline preservation, untracked-file detection, real temporary worktrees, Windows executable resolution, and terminal-viewer tests. One existing restart test explicitly approves discarding plan progress, and no test covers the false-gate combination from the incident.

## Refactor goals

1. Show useful provider activity within one second of receiving it, without waiting for the turn to finish.
2. Record raw provider events and a normalized event stream for every attempt.
3. Enforce explicit time, token, cost, tool-call, and round limits at the boundaries Claudex can control.
4. Stop only for a human decision when a provider explicitly requests one with a concrete question.
5. Resume from the latest valid durable checkpoint without reconstructing or discarding accepted work.
6. Replace foreground rate-limit sleeping with durable pause/resume states.
7. Make cancellation terminate provider descendants on Windows and supported Unix platforms.
8. Detect tracked, modified, deleted, and untracked work consistently at all Git gates.
9. Prove the provider adapters and orchestrator with deterministic fake CLIs and real temporary Git repositories.
10. Make the normal plan-to-implementation path short enough to understand from the run timeline.

## Non-goals

- Building a general chat UI or an IDE.
- Driving interactive Claude or Codex TUIs by keystrokes or scraping screen text.
- Hiding provider differences behind an abstraction so generic that important capabilities are lost.
- Estimating provider cost when the provider supplies no trustworthy pricing or usage data; unknown values must remain explicitly unknown.
- Preserving compatibility with undocumented run-state internals at the expense of a safe migration path.
- Optimizing for maximal autonomous deliberation. The default optimizes for a bounded, inspectable delivery loop.

## Target architecture

```text
Claude/Codex process
        |
        | streamed stdout/stderr
        v
provider decoder -----> raw attempt JSONL/log
        |
        | AgentEvent
        v
event journal -----> live watchers / terminal windows
        |                    |
        |                    +--> phase, text, tools, elapsed, usage, cost
        v
budget + lifecycle controller
        |
        v
provider-specific result parser
        |
        v
coordinator transition + durable checkpoint
```

### Normalized events

Each adapter should emit an `AgentEvent` with at least:

- `run_id`, `attempt_id`, `agent`, `phase`, and monotonic sequence number.
- Wall-clock timestamp and elapsed duration.
- Event kind such as `started`, `text_delta`, `tool_started`, `tool_finished`, `usage`, `warning`, `rate_limited`, `completed`, or `failed`.
- Human-readable summary safe for live display.
- Provider event type and a reference to the raw event.
- Normalized usage fields when reported: input, cached input, cache creation, output, reasoning, and total tokens.
- Reported cost and currency when authoritative.
- Tool name/status and optional redacted arguments.

Raw provider output remains available for diagnosis, while coordinator code consumes only normalized events and typed final results. Unknown event types are journaled and surfaced as warnings, not silently discarded or allowed to crash the reader.

### Attempt artifacts

Every invocation gets a unique attempt identifier and immutable files, for example:

```text
runs/<run-id>/attempts/<attempt-id>/
  command.json
  stdout.jsonl
  stderr.log
  events.jsonl
  result.json
  summary.json
```

Secrets, authorization headers, environment values, and configured sensitive argument fields must be redacted before persistence. Retention is configurable by age and maximum bytes. The compact run summary and decision ledger remain durable even when raw event logs are pruned.

## Visible execution and terminal UX

The primary interface remains the Claudex coordinator. It gains:

- `claudex watch [RUN_ID] --agent claude|codex|all` for a readable live event stream.
- `claudex status [RUN_ID]` with state, active phase, model, effort, elapsed time, rounds, tools, usage, reported cost, and remaining budgets.
- `claudex run ... --open-terminals` to launch dedicated Claude and Codex watcher panes/windows when Windows Terminal is available.
- A clear fallback that prints watcher commands when terminal launching is unavailable or disabled.

The terminal windows are views of Claudex's normalized journal. Providers still run as structured non-interactive subprocesses. Closing a watcher does not cancel a run; `claudex cancel` does.

Output should favor high-signal lines: provider text deltas, tool start/finish, warnings, usage updates, and state transitions. Raw JSON is an explicit diagnostic mode.

## Budget and policy model

Budgets are first-class run configuration with validated non-negative values:

- Per invocation: maximum duration, maximum reported output tokens, and maximum tool calls where observable.
- Per run: maximum provider invocations, input tokens, output tokens, reported cost, and elapsed wall time.
- Per phase: model and reasoning-effort profile, with a conservative default for planning/review and explicit escalation for difficult implementation or verification.
- Provider controls: pass Claude's native monetary limit when configured; enforce coordinator-side “do not start the next invocation” guards for all providers.

Claudex cannot always stop a provider at the exact token boundary, so it must distinguish hard provider-side limits from coordinator-side admission limits. Before starting an invocation, it calculates whether the configured remaining envelope permits it. If not, the run transitions to `PAUSED_BUDGET` with the exact limit and a resumable override command.

High-cost combinations—such as premium models with maximum reasoning effort—must be visible in the start summary and require an explicit policy opt-in, not an incidental inherited default. Nested agent capabilities are disabled by default for both providers and counted explicitly when enabled.

## Orchestration protocol

The default delivery loop becomes:

1. One bounded plan draft.
2. One fresh critique against a bounded evidence packet.
3. One revision that resolves accepted findings and updates the decision ledger.
4. At most one short audit when unresolved blocking findings remain.
5. Implementation in the isolated worktree.
6. Checkpoint, mechanical tests, and a fresh verification pass.

Additional planning rounds require an explicit configured override. Planning and review sessions are fresh by default; an implementation session may persist when continuity has measurable value.

Plan updates should be section replacements or structured deltas, not mandatory full-plan re-emission on every turn. The coordinator owns the canonical assembled plan and rejects malformed deltas without losing the last valid version.

Reviewer context is assembled by the coordinator and capped. It includes only:

- The immutable task and binding guidance.
- The current canonical plan or implementation diff.
- The compact decision ledger.
- Unresolved accepted findings.
- Selected repository facts and verification output.
- A manifest of omitted artifacts that can be requested through a bounded mechanism.

External research is performed once per changing fact and cached with source and timestamp. A reviewer should not repeatedly browse the same question simply because a new round started.

## Human decision contract

A human gate is valid only when all of these are true:

1. `requires_human_decision` is explicitly `true`.
2. `human_decision_question` is a non-empty, actionable question.
3. The result explains why safe progress cannot continue under existing task authority.

Finding severity and kind influence acceptance and convergence but never synthesize a human gate. An inconsistent result (`true` with no question, or `false` with a question) is a provider-protocol validation failure with a bounded retry, not a user decision.

Budget increases, security-boundary expansion, and destructive external actions remain coordinator-owned gates even if a provider forgets to request them.

## Durable lifecycle and restart

Run state should distinguish at least:

- `RUNNING`
- `PAUSED`
- `PAUSED_BUDGET`
- `RATE_LIMITED`
- `CANCELLED`
- `FAILED_RETRYABLE`
- `FAILED_TERMINAL`
- `COMPLETED`

Every state transition is appended with timestamp, reason, phase, attempt, and resume instruction. A rate limit persists reset metadata and exits promptly by default. An opt-in daemon mode may wait, but must release or safely renew the run lease and remain cancellable.

`restart` creates a replacement execution identity while copying the complete latest validated checkpoint: canonical plan, accepted and unresolved findings, decision ledger, worktree/base commit, checks, binding guidance, budgets, and provider session metadata that is safe to reuse. `resume` continues the same run. `restart --fresh-plan` deliberately discards planning state and records that destructive choice.

State migrations are versioned, tested, and fail closed with a recovery message. The previous state file is retained until the migrated state has been atomically committed.

## Process, Git, and provider safety

- Start provider processes in their own process group/job object and terminate the full tree on cancel or timeout.
- Stream stdout and stderr concurrently to avoid pipe deadlocks.
- Preserve partial output and the terminal reason after timeout, cancellation, malformed data, or rate limiting.
- Use unique attempt paths so retries cannot overwrite the evidence needed to diagnose the preceding failure.
- Treat untracked files as work at every cleanliness, checkpoint, test, and merge gate.
- Record the exact tree or patch that tests exercised and verify it is the tree proposed for integration.
- Give provider write sessions only the filesystem and command capabilities required for the phase. A worktree is not presented as a sandbox.
- Resolve executables by explicit configuration or semantic capability/version checks, not file modification time.
- `claudex doctor` reports provider path, version, required stream/schema features, sandbox capabilities, and actionable incompatibilities.

## Testing strategy

The test foundation must not depend on live paid providers.

### Deterministic provider fixtures

Create fake Claude and Codex executables that emit timed fixtures covering:

- Partial text, tool start/finish, usage, and final structured result.
- Interleaved stdout and stderr.
- Unknown and malformed events.
- Rate-limit metadata and non-zero exits.
- Hung children and spawned descendants.
- Partial output followed by timeout or cancellation.
- Provider version/capability variations.

Tests assert both the normalized event journal and typed result.

### Required regression and integration tests

- The exact incident case: explicit `requires_human_decision: false`, no question, and major `decision` findings must not gate.
- An event emitted by a slow fake provider becomes visible to a watcher before provider completion.
- Rate limiting persists state and returns control without sleeping by default.
- Retries retain distinct attempt logs and failed session metadata.
- Budget exhaustion prevents the next provider invocation and preserves resumability.
- Restart preserves the canonical plan, decisions, findings, checks, budgets, and worktree identity.
- A real temporary Git repository/worktree validates checkpoint and merge behavior.
- Untracked files fail the appropriate gate and are included when explicitly accepted.
- Timeout and cancel terminate process descendants and preserve partial evidence.
- Windows executable selection uses configured path/capability/version deterministically.
- Golden watcher/status output covers active, rate-limited, budget-paused, failed, and completed runs.
- An opt-in live-provider smoke test has a strict native/provider-side spend cap and is excluded from the default suite.

## Acceptance criteria

The refactor is ready to replace the current workflow when all of the following are demonstrated:

1. A slow fake-provider run displays text/tool events live and completes without pipe deadlock.
2. Status reports active phase, elapsed time, model/effort, attempts, usage, reported cost, and remaining budgets from durable state.
3. The incident critique fixture proceeds without a human gate.
4. Default planning cannot exceed one draft, one critique, one revision, and one conditional audit.
5. Rate limiting returns control promptly with a durable, resumable state; no default path sleeps for minutes.
6. Cancel kills the complete fake-provider process tree on Windows and supported CI platforms.
7. Restart preserves a hash-equivalent canonical checkpoint unless `--fresh-plan` is explicit.
8. Tracked and untracked changes are accounted for in tests, checkpoints, and integration.
9. Fake-provider, orchestration, migration, Git-worktree, and terminal-view tests pass in the normal test suite.
10. A budget-capped live smoke run can complete the short delivery protocol with no hidden provider invocation.
11. README and CLI help explain the protocol, costs, watcher windows, pause/resume/cancel behavior, and the distinction between a worktree and a security sandbox.

## Delivery sequence

Implementation should proceed in dependency order:

1. Lock down tool/provider contracts and create deterministic fake executables.
2. Add normalized events, streaming subprocess execution, immutable attempt logs, and watchers.
3. Add usage accounting, validated budgets, phase profiles, and admission guards.
4. Correct gate semantics, bounded reviewer context, convergence, lifecycle states, and restart behavior.
5. Harden process cancellation, Git integrity, provider permissions, executable resolution, retention, and redaction.
6. Complete integration, migration, fault-injection, and optional live-provider tests.
7. Ship the watcher/open-terminal UX, operator documentation, and a conservative versioned migration.

The detailed execution plan is maintained separately under `tasks/` using the c0degeek-ai plan template. This specification is the durable statement of the problem, invariants, and acceptance boundary; the plan is disposable coordination scaffolding.

## Rollback and compatibility

- Introduce versioned state/event schemas and retain the last pre-migration state file.
- Keep the existing non-streaming result parser behind a temporary compatibility adapter only while fixtures prove provider parity; remove it before declaring the refactor complete.
- Gate the new terminal launcher independently from the event journal so headless environments remain supported.
- If a provider changes its stream schema, fail capability checks before starting a paid run and preserve the last compatible adapter.
- Do not silently reinterpret old rate-limit or restart states. Migrate when semantics are provably equivalent; otherwise stop with a documented recovery/export path.

## Decisions fixed by this specification

- Live terminal windows are journal watchers, not interactive provider automation.
- Structured non-interactive provider execution remains the control boundary.
- Rate limits pause and return control by default.
- Human gates require an explicit boolean and an actionable question.
- Planning/review sessions are fresh and bounded by default; implementation continuity is opt-in by phase.
- Nested provider agents are disabled by default.
- Restart is non-lossy; discarding planning state requires an explicit fresh-plan operation.
- Unknown cost remains unknown and visible; it is never replaced by a reassuring estimate.

Any change to these decisions should update this document, include the new evidence, and add or revise the corresponding regression test.
