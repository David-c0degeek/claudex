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
- (design close, 2026-07-16) CX's top-3 Go implementation watch-items (both agents AGREE): (1) **Canonical protocol bytes** — one embedded schema source, exact version negotiation, `json.Decoder.UseNumber()`/number semantics, digest stable across whitespace/key-order/platforms; (2) **Crash consistency** — same-directory atomic writes + fsync durability, the git ref/index/state/receipt journal cut-points, idempotent recovery with read-only commands never mutating; (3) **Real OS behaviour** — process-death locks + race-free process-tree ownership tested on native Windows AND Linux; keep inherited-stdio launch separate from the optional PTY telemetry path.
- (01 close, 2026-07-16) **Audit shipped code against every box before claiming a subject done.** txn's AGREE closed only two of ten boxes; a green build on the packages that happened to exist was not evidence the ledger-projection and legacy-migration boxes were done — they were entirely unbuilt. Enumerate the boxes and map each to code+test before ticking the subject.
- (01 close, 2026-07-16) **A durable guard must key on the real storage boundary, not on decoding a file the real path never reads.** The attach loader reads immutable-generation directories, never `<run>/state.json`; proving the strict decoder rejects a legacy `state.json` tested a path bootstrap never takes. The correct fail-closed guard (`legacy.CheckRunDir`) refuses on the *presence* of any unrecognized `state.json` (legacy → typed refusal; anything else → fail closed), because an empty attach store is not evidence the directory is safe to allocate.
- (01 close, 2026-07-16) **Keep comments plan-agnostic from the first commit.** Decision/box IDs (`D0xx`, `NN.M`) accumulated across subjects 00–01 and had to be swept at close (gate §6.11). Rationale belongs in the code as prose and in the ADR by number — not as a cross-reference from shipped source, which rots when the disposable plan is removed.
- (00.3, 2026-07-16) `main` is the lean v2 baseline: `claudex/` = agents,
  artifacts, cli, config, gitops, phases, prompts, schemas, state (+templates).
  It compiles (`python -m compileall claudex` OK) but has **zero tests**
  (`tests/` holds only fixtures). The reliability/observability engine — 12
  modules (budgets, evidence, lifecycle, limits, processes, providers, security,
  terminal, recovery, events, provider_events, planops) and all 10 test modules + 6 fixtures,
  +9.3k lines — exists **only on `reliability-observability-refactor`**, not on
  our base. Any Integration-analysis reference to those modules means "port from
  refactor-branch history," not "already present." §2 test baseline: the suite
  is built fresh (or selectively ported); `unittest discover -s tests` currently
  finds nothing.
