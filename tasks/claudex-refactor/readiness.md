# Claudex Refactor Readiness Record

Date: 2026-07-15  
Platform: Windows 11, PowerShell, Europe/Amsterdam  
Execution branch: `reliability-observability-refactor`

## Repository and baseline

- No `AGENTS.md`, `CLAUDE.md`, project ADR, CI workflow, linter configuration,
  plan-template override, or permanent `tasks/lessons.md` existed at readiness
  time. `README.md`, `refactor.md`, the package metadata, current source, and the
  live plan are authoritative.
- Starting HEAD was `7ab861f08ce5893abe90bf0f0853d37333ed73b1` on `main`.
- The starting workspace contained the uncommitted 0.4 pair-programming rewrite
  across README, package metadata, coordinator/state/schema/prompt/CLI modules,
  templates, and a new `tests/test_orchestration.py`. It was preserved unchanged
  as commit `a23224a` on `reliability-observability-refactor` after its baseline
  verification passed.
- Remote `origin` is `https://github.com/David-c0degeek/claudex.git`; checkpoint
  pushes are feasible.
- `git status --porcelain=v1` exposed `refactor.md`, `tasks/`, and `tests/` as
  untracked while the production helper used `-uno`, confirming the integrity
  defect described by the refactor specification.

## Stack and commands

| Item | Observed |
|---|---|
| Runtime | Python 3.14.6 locally; package contract is Python >=3.10 |
| Packaging | setuptools via `pyproject.toml`; console script `claudex = claudex.cli:main` |
| Dependencies | Runtime is standard-library only |
| Tests | `unittest`; 24 baseline tests passed in 0.584 seconds |
| Build/syntax | `python -m compileall -q claudex` passed |
| Formatting/lint | No configured linter; `git diff --check` passed |
| CLI smoke | `python -m claudex --version` returned 0.4.0 |
| Editable metadata | `pip show claudex` still reported 0.1.0; editable metadata needs refresh during release verification |
| Git | 2.54.0.windows.1 |
| Windows Terminal | 1.24.11321.0; `wt.exe` is available |

The normal suite must remain credential-free. Provider contract tests will use
fake executables and recorded event fixtures. The live-provider module remains
default-skipped and requires an explicit environment opt-in plus native and
coordinator caps.

## Prior-art and integration map

| Concern | Existing seam to extend | Constraint |
|---|---|---|
| Provider processes | `claudex/agents.py::_exec`, `ClaudeAgent`, `CodexAgent`, `AgentResult` | Replace buffered `communicate()` at this seam; preserve typed final results |
| Provider selection | `resolve_claude_bin`, `resolve_codex_bin`, `build_agents` | Explicit path wins; discovered candidates require deterministic version/capability selection |
| Coordination | `claudex/phases.py::Orchestrator` | Coordinator remains the only owner of transitions and invocation admission |
| Human gate | `claudex/schemas.py::requires_human_decision`, `pair_plan_turn` | Explicit boolean plus actionable question is the only provider-requested decision gate |
| Run state | `claudex/state.py::RunState`, `Phase`, atomic `save/load` | Add versioned lifecycle/metrics without a second state store |
| Configuration | `claudex/config.py::Config` and `cli.py::_cfg` | Validate at load; keep conservative migrations for existing JSON |
| Artifacts | `claudex/artifacts.py`, run directory layout, mailbox | Event journal is canonical; mailbox becomes a readable derived summary |
| Git integrity | `claudex/gitops.py` | Include untracked paths and record exact tested/integrated content |
| Prompts/context | `claudex/prompts.py`, plan/review artifact helpers | Bounded evidence manifests replace critique globs and full-history dependence |
| Test gate | `Orchestrator.run_test_gate` | Reuse the streamed/cancellable process lifecycle instead of `subprocess.run` |
| Operator CLI | `claudex/cli.py` parser and command handlers | Add watch/cancel and enrich status/doctor; keep headless operation |
| Tests | `tests/test_orchestration.py` | Retain unit tests; add subprocess, state, Git, Windows, golden, and fault suites |

No existing event or budget abstraction fits the work. New focused modules are
justified for event types/journaling, process lifecycle, and budget/accounting;
they must be called by the existing agents/coordinator/CLI rather than forming a
parallel orchestration system.

## Verified provider capabilities

### Claude Code 2.1.210

Installed help and the official Claude Code CLI/headless documentation confirm:

- `-p --output-format stream-json --verbose --include-partial-messages` emits
  newline-delimited live events. The terminal `result` includes response,
  session, usage, and cost metadata.
- `system/api_retry` exposes retry delay/category, including rate limits.
- `system/init` can expose a capability array; feature detection is preferred
  to version-only branching.
- `--json-schema` preserves validated final structured output.
- `--max-budget-usd` and `--max-turns` provide native print-mode caps.
- `--tools`, `--allowedTools`, `--disallowedTools`, `--strict-mcp-config`,
  `--setting-sources`, `--safe-mode`, and permission modes provide narrower
  execution policy. `allowedTools` alone is not an availability restriction;
  the refactor must remove/deny Agent/Task and unneeded tools explicitly.

Primary sources:

- https://code.claude.com/docs/en/cli-usage
- https://code.claude.com/docs/en/headless
- https://code.claude.com/docs/en/agent-sdk/python

### Codex CLI 0.144.4 on PATH; 0.144.2 desktop candidate

Installed help and the current official Codex manual confirm:

- `codex exec --json` makes stdout a JSONL event stream with
  `thread.started`, `turn.*`, `item.*`, errors, and terminal usage.
- `--output-schema` and `-o/--output-last-message` retain structured final
  output while events stream.
- `--sandbox read-only|workspace-write`, explicit approval configuration,
  `--ignore-user-config`, `--ignore-rules`, `--strict-config`, and feature
  overrides allow deterministic automation policy.
- `multi_agent` is a stable enabled feature in the installed CLI and can be
  disabled per invocation with `--disable multi_agent`.
- Higher reasoning effort increases response time and token usage. Claudex must
  set/show its phase policy instead of silently inheriting `xhigh`.
- There is no documented native per-invocation monetary cap comparable to
  Claude's flag, so Claudex can enforce only admission/time limits and reported
  usage around an active Codex turn.
- Current resolver behavior selects the older 0.144.2 desktop binary by file
  mtime even though PATH provides 0.144.4, proving the resolver defect.

Primary source: current Codex manual fetched by the official OpenAI docs helper
from https://developers.openai.com/codex/codex-manual.md, especially its
Non-interactive mode, sandboxing, configuration, and multi-agent sections.

## Primary implementation guidance

- Python documents that pipe consumers can deadlock if one pipe fills. Streaming
  therefore requires concurrent stdout/stderr drains and explicit process wait/
  timeout coordination, not sequential reads. Source:
  https://docs.python.org/3/library/subprocess.html
- Windows documents Job Objects as the unit for controlling and terminating a
  process group, with child processes joining by default and
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` available. Use a small stdlib `ctypes`
  wrapper with a tested `taskkill /T` fallback when assignment is unavailable.
  Source: https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects
- Git porcelain status is stable for scripts and shows untracked files by
  default; `-uno` explicitly hides them. Use `--porcelain=v1 -z --untracked-files=all`
  where path-safe parsing is needed. Source: https://git-scm.com/docs/git-status
- Journals use one JSON object per line, monotonic per-attempt sequence numbers,
  flush after every event, atomic compact summaries/state, and tolerant readers
  that ignore incomplete final lines while surfacing corruption.

## Tool and skill disposition

| Tool/workflow | Disposition | Reason |
|---|---|---|
| c0degeek-ai `plan-from-template` | adopt | Governs this resume-safe plan and its readiness/Hindsight gates |
| OpenAI docs skill/manual helper | adopt for readiness | Supplies current official Codex behavior without provider spend |
| Python stdlib `unittest`, `threading`, `queue`, `ctypes`, `tempfile` | adopt | Satisfies runtime and test requirements without new dependencies |
| Real Git CLI and temporary repositories/worktrees | adopt | Tests the actual integration boundary |
| Windows Terminal `wt.exe` | adopt as optional view adapter | Installed, but journal/watch remain the headless source of truth |
| `pytest` | defer | Installed locally but not a repository contract and unnecessary for this suite |
| `coverage` | reject for this plan | Not installed; plan tests observable regressions rather than chasing a percentage |
| Claude Agent SDK dependency | reject | The supported CLI already supplies the required stream/schema/cap controls; adding an SDK would violate the small stdlib runtime goal |
| MCP/plugin installation | reject | No implementation need; would add startup, trust, credential, and hidden tool-call variability |

No new tooling, credentials, services, or permission grants were installed.

## Documentation inventory

The implementation must update, in the same checkpoint as affected behavior:

- `README.md` for protocol, watch/open-terminal workflow, budgets, lifecycle,
  provider policy, troubleshooting, and the worktree-not-sandbox warning.
- CLI help in `claudex/cli.py` and configuration comments/reference examples.
- `claudex/templates/SKILL.md` and `codex-lead-prompt.md` for live pairing.
- A permanent architecture/decision record created under `docs/` because the
  repository currently lacks one.
- Package/version metadata and release notes at cutover.

## Readiness conclusion

Implementation is ready. The baseline passes, the dirty starting work is safely
checkpointed, no external install or human action is required, provider
contracts are verified from installed help and primary docs, the new module
boundaries are justified, and the plan's commands/gates now name deterministic
offline tests. Provider schema uncertainty is contained by raw journaling,
tolerant adapters, capability checks, and versioned fixtures.

