# 07 — Managed Launcher And Release

## Goal
The enforcement-upgrade tier and the ship. A managed-interactive launcher starts
each provider TUI with inherited stdio (fully human-interactive), a cwd pinned to
the coordinator worktree, and provider sandbox/tool flags — recovering
launch-time enforcement that BYO attach cannot. Enforcement capabilities are
labeled **individually** (`sandboxed`, `telemetry-observed`, …), never a blanket
"enforced". Then retire the old headless driver, rewrite the docs, and cut the
release.

## Integration analysis
> Fill/confirm against the 00.2 corrected source map and the 00.5 telemetry research before ticking any box.
- **Existing code found** — the real launch surface is **`cli.py:open_watch_terminals`** (Windows Terminal launcher), NOT `terminal.py` (which renders/watches). `claudex/providers.py` probes capabilities + `doctor`; actual provider command flags live in `ClaudeAgent`/`CodexAgent` (`agents.py`). `claudex/processes.py` provider-invocation path is retired here.
- **Behaviour to preserve** — `doctor` capability discovery/reporting; provider flag controls (Claude tool restrictions, Codex sandbox/network); Windows Terminal launch best-effort semantics.
- **Reuse / extend** — base managed launch on `cli.py:open_watch_terminals` + provider discovery (`providers.py`) + the flag definitions in `agents.py`; launch an *interactive* provider session (inherited stdio) rather than a read-only watcher; the retired headless-invocation code in `processes.py`/`agents.py`/`cli.py cmd_run` is deleted here (residual to 03.9's disable, per D003).
- **Do not duplicate** — no second launcher or capability-discovery path.
- **Integration point + why** — `cli.py:open_watch_terminals` + `providers.py`/`agents.py` already own launching + capability facts; extend them for managed-interactive launch.
- **Vision fit** — realizes D005 (managed tier) + D007 (per-capability labels; telemetry only if a real channel exists).
- **Risks** — inherited stdio does NOT yield usage telemetry by itself; over-claiming metered caps; command-argument unit tests cannot prove interactivity (need a manual smoke); cross-platform (Windows ConPTY vs POSIX PTY) launch differences; leaving half-removed headless code.

## Boxes
- [ ] **07.1** (agent) Managed launch: `claudex` starts each provider TUI with inherited stdin/stdout (fully interactive), cwd pinned to the coordinator worktree, provider sandbox/tool flags applied at launch, and the run/session/role identity passed into the session so it can `pull`/`submit` without a manual `attach`. Define provider exit/reconnect behavior (a closed TUI is a session that can be replaced per 03.2, not a bricked run). Test: launched session is interactive + pinned; identity + flags present.
- [ ] **07.2** (agent) Per-capability labeling: `status`/`doctor` report each enforcement capability separately (`sandboxed`, `cwd-pinned`, `telemetry-observed`, …) with the mechanism that backs it; no blanket "enforced" bit; BYO stays `protocol-only`. Test: labels match actual mechanisms.
- [ ] **07.3** (agent) Usage metering — only if 00.5 found a real telemetry channel (provider side-channel or PTY/ConPTY proxy preserving TUI semantics). If none, ship `telemetry-observed: false` and label usage caps `requires-telemetry`/`unavailable` (managed-with-inherited-stdio alone is still unmetered — NOT `requires-managed`); record the decision. Test: metered caps enforced only when the channel exists.
- [ ] **07.4** (agent) Delete the residual headless driver: remove `cli.py cmd_run`, the `processes.py` provider-invocation path, `agents.py` subprocess driving, and dead `providers.py` invocation glue (03.9 already disabled the public entrypoints); keep the streamed timeout/cancel primitive used by the test gate; update/retire tests that assumed the old driver. No half-removed engine.
- [ ] **07.5** (agent) Docs/release: rewrite `README.md` (attach model, two tiers, honesty labels), update `docs/architecture.md` + `docs/decisions.md` (promote D001–D011 + protocol ADR), refresh CHANGELOG; `doctor` reflects the new matrix. Behaviour-verify the documented commands.
- [ ] **07.6** (agent) Managed-launch capability matrix: document + test which capabilities each tier truly provides (BYO `protocol-only`; managed `sandboxed`/`cwd-pinned`; `telemetry-observed` only if 07.3's channel exists), so no doc or `status` output over-claims. Test: the matrix matches shipped labels.
- [ ] **07.7** (release-engineer) Manual interactive smoke on the supported-platform matrix: define the supported platforms (Windows / POSIX) and which the release environment actually has; launch a **real** Claude Code and Codex interactive TUI via managed launch and confirm interactivity + a completed turn on **each available** platform (command-argument unit tests cannot prove this). A platform the release environment lacks is recorded as an explicit support deferral, not an uncloseable gate. Mirrored in `manual-actions.md`.
- [ ] **07.8** (release-engineer) Release cut/tag/version bump per repo convention; mirrored in `manual-actions.md`.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
