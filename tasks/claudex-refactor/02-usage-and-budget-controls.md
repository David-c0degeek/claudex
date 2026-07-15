# 02 — Usage And Budget Controls

## Goal

Turn streamed provider telemetry into durable per-attempt and per-run accounting,
validated policy, provider-native limits, and coordinator admission controls.
Operators must see reported spend and remaining envelopes, while missing or
provider-specific data remains explicitly unknown.

## Integration analysis

- **Existing code found** — `claudex/agents.py::AgentResult` currently carries
  outcome but not normalized telemetry; `claudex/config.py` owns run defaults;
  `claudex/state.py` persists run state; `claudex/cli.py` renders status and start
  options; provider command builders select model/effort; `phases.py` owns
  invocation admission and round limits.
- **Behaviour to preserve** — existing configuration and CLI invocations remain
  usable through documented defaults/migration; round limits remain a secondary
  convergence guard; provider-reported values are preserved exactly.
- **Reuse / extend** — aggregate the subject-01 event contract into attempt and
  run summaries; validate budgets through the existing config/CLI boundary;
  enforce before the existing provider invocation seam.
- **Do not duplicate** — no separate billing database or provider-price scraper.
  Raw event usage, normalized summary, state, status, and final report form one
  traceable pipeline.
- **Integration point + why** — `phases.py` is the final coordinator-controlled
  point before spend begins; provider command builders are the correct seam for
  native caps and multi-agent/effort policy.
- **Vision fit** — makes the `refactor.md` economic envelope enforceable without
  pretending Claudex can know unreported or future provider pricing.
- **Risks** — double-counting cumulative provider messages, mixing cached/input
  semantics, currency ambiguity, overshoot within an active call, invalid legacy
  configuration, and cost controls that do not match provider capabilities.

## Boxes

- [x] **02.1** (agent) Extend attempt results and versioned run state with
      normalized timing, invocation, tool, token-category, and authoritative
      cost/currency fields plus source/quality metadata; add aggregation tests
      that prevent double counting and preserve unknowns.
- [x] **02.2** (agent) Add validated per-invocation, per-run, and phase policy
      configuration for duration, invocations, rounds, reported input/output
      tokens, reported cost, tool calls where observable, model, effort, and
      nested-agent permission; reject negative, contradictory, or unsafe values
      with actionable CLI errors and migrate legacy config conservatively.
- [x] **02.3** (agent) Enforce provider-native hard controls when supported—
      including Claude's configured monetary cap—and coordinator admission
      checks before every call; persist `PAUSED_BUDGET` with the exact exhausted
      limit and an explicit resumable override instead of starting more work.
- [x] **02.4** (agent) Introduce conservative phase profiles, make expensive
      model/maximum-effort combinations prominent and opt-in, and disable nested
      Claude/Codex agents by default using verified provider capabilities;
      fixture-test emitted commands for supported versions.
- [x] **02.5** (agent) Render current attempt/run usage, reported cost, unknown
      fields, elapsed time, active policy, and remaining budgets from durable
      state in status and completion summaries; ensure logs never label estimates
      as provider-reported facts.
- [x] **02.6** (agent) Run deterministic scenarios for exact-limit, projected
      overshoot, within-call overshoot, missing usage, cumulative events,
      currency mismatch, config migration, nested-agent opt-in, and budget
      override/resume; record behavior evidence and reconcile events to summaries.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [x] Captain Hindsight review recorded
- [x] Verdict is `CLOSE`

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.

- 2026-07-15 · slice 1 · 02.1–02.4 · added an idempotent provider-terminal
  accounting pipeline, versioned fail-closed run state, frozen run/phase
  policies, native Claude USD/turn caps, phase-native model/effort selection,
  default Claude Agent/Task and Codex `multi_agent` denial, and durable
  pre-call `PAUSED_BUDGET` admission; verified the installed Claude 2.1.210
  help exposes `--max-budget-usd`, `--max-turns`, `--effort`, and
  `--disallowedTools`, while Codex 0.144.4 exposes JSON/config/feature switches
  and lists `multi_agent` stable · checkpoint is the subject-close commit.
- 2026-07-15 · slice 2 · 02.5–02.6 · added explicit run-envelope overrides,
  non-converting currency acknowledgement, active/last attempt and normalized
  category reporting, start/completion summaries, config/CLI/README reference,
  and deterministic exact-limit, projected/within-call overshoot, missing data,
  cumulative-category, migration, provider-command, nested-agent, foreign
  currency, wall-clock, phase-model, and resume scenarios · all tests,
  compileall, CLI help, and diff-check pass; no live provider spend was used.

### Captain Hindsight — closing review

1. **Keep:** Admission and invocation counting happen durably before provider
   launch, while exactly one terminal result is the idempotent charge point.
   Provider-native differences stay in command builders; raw values, normalized
   non-overlapping token categories, authoritative currency, and unknown
   quality remain distinguishable.
2. **Fix before closing:** The first pass modeled only run totals, provider
   duration, effort, and global models. Cross-checking `refactor.md` exposed
   missing per-invocation output/tool ceilings, run wall time, and per-phase
   models. Those were added with frozen state, explicit override paths, start
   visibility, and regression scenarios before closure. The first currency
   shape also overloaded `cost_usd`; it now retains a general reported amount
   plus authoritative currency and derives USD totals only for USD reports.
3. **Record:** D004 records terminal-only accounting and conservative unknown
   handling. Added the cached/reasoning subset normalization and policy-freeze
   lessons to `lessons.md`; both should inform the permanent budgeting ADR in
   06.4.
4. **Risk:** Providers cannot always hard-stop at output/tool boundaries, and
   Codex does not report authoritative monetary cost here. Claudex therefore
   charges within-call overshoot, blocks the next call, labels missing cost
   unknown, and relies on invocation/time/token admission plus Claude's native
   monetary cap. Subject 05 will exercise the same behavior through reusable
   black-box fake executables.
5. **Verdict:** CLOSE.
