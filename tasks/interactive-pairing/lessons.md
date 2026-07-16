# Lessons — InteractivePairing (run notes)

> Append the moment a slice teaches something; do not wait for the gate.
> Durable lessons migrate to the permanent `tasks/lessons.md` at §7.

- (seed, from the design exchange) The product's own coordination pattern was
  proved live before the plan existed: a role-addressed file mailbox
  (`to-codex.md`/`to-claude.md`) with a `TURN:` sequence and a bounded
  blocking-poll loop is enough for two interactive agent TUIs to converge by
  agreement. The plan productizes exactly this.
- (seed) Git ref update and coordinator state CAS cannot be committed
  atomically; a two-store operation needs a prepared-transaction journal +
  startup reconciliation, not an assumption of atomicity.
- (seed) Interactive attach cannot honestly observe a user's TUI usage; only
  turn/fix counts, artifact bytes, and wall time are enforceable without a
  managed launch + real telemetry channel. Never label a capability "enforced"
  without the mechanism that backs it.
- (00.3, 2026-07-16) `main` is the lean v2 baseline: `claudex/` = agents,
  artifacts, cli, config, gitops, phases, prompts, schemas, state (+templates).
  It compiles (`python -m compileall claudex` OK) but has **zero tests**
  (`tests/` holds only fixtures). The reliability/observability engine — 12
  modules (budgets, evidence, lifecycle, limits, processes, providers, security,
  terminal, recovery, events, provider_events, planops) and all 17 test files,
  +9.3k lines — exists **only on `reliability-observability-refactor`**, not on
  our base. Any Integration-analysis reference to those modules means "port from
  refactor-branch history," not "already present." §2 test baseline: the suite
  is built fresh (or selectively ported); `unittest discover -s tests` currently
  finds nothing.
