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

- [ ] **02.1** (agent) Extend attempt results and versioned run state with
      normalized timing, invocation, tool, token-category, and authoritative
      cost/currency fields plus source/quality metadata; add aggregation tests
      that prevent double counting and preserve unknowns.
- [ ] **02.2** (agent) Add validated per-invocation, per-run, and phase policy
      configuration for duration, invocations, rounds, reported input/output
      tokens, reported cost, tool calls where observable, model, effort, and
      nested-agent permission; reject negative, contradictory, or unsafe values
      with actionable CLI errors and migrate legacy config conservatively.
- [ ] **02.3** (agent) Enforce provider-native hard controls when supported—
      including Claude's configured monetary cap—and coordinator admission
      checks before every call; persist `PAUSED_BUDGET` with the exact exhausted
      limit and an explicit resumable override instead of starting more work.
- [ ] **02.4** (agent) Introduce conservative phase profiles, make expensive
      model/maximum-effort combinations prominent and opt-in, and disable nested
      Claude/Codex agents by default using verified provider capabilities;
      fixture-test emitted commands for supported versions.
- [ ] **02.5** (agent) Render current attempt/run usage, reported cost, unknown
      fields, elapsed time, active policy, and remaining budgets from durable
      state in status and completion summaries; ensure logs never label estimates
      as provider-reported facts.
- [ ] **02.6** (agent) Run deterministic scenarios for exact-limit, projected
      overshoot, within-call overshoot, missing usage, cumulative events,
      currency mismatch, config migration, nested-agent opt-in, and budget
      override/resume; record behavior evidence and reconcile events to summaries.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.

