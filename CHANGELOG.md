# Changelog

## 0.5.0 — 2026-07-16

- Stream Claude, Codex, and coordinator test activity into normalized,
  replayable per-attempt journals with immutable result summaries.
- Enforce bounded planning, exact human-decision gates, durable budgets,
  reported-versus-unknown usage, non-lossy resume/restart, and explicit
  rate-limit/cancel lifecycles.
- Kill complete provider/test process trees, include untracked files in exact
  tested/reviewed identities, negotiate provider capabilities semantically,
  narrow phase permissions, and redact/prune raw artifacts safely.
- Add `watch`, richer `status`, `cancel`, compact `export`, optional Windows
  Terminal watcher windows, versioned config/state migrations, deterministic
  fake-provider acceptance/fault suites, and Windows/Ubuntu CI.

### Compatibility

- Config schema 1 migrates to schema 2 with `config.v1.bak.json`. Only the exact
  historical broad Claude tool default is narrowed automatically; custom broad
  shell grants fail closed for operator review.
- Run state migrates atomically to schema 6 and retains `state.vN.bak.json`.
  Unsafe legacy planning protocol runs still require an explicit new run.
- `abort` is replaced by idempotent `cancel`; use `claudex cancel` for active or
  idle runs.
