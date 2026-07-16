# 00 — Tooling Research And Readiness

## Goal
Research the stack, existing engine, and interactive-terminal constraints
before implementation. Audit which current modules survive the attach pivot vs
are thrown away. Produce a protocol/architecture ADR and confirm the base
branch. Convert findings into concrete plan rules, gates, and references.

## Boxes
> Subject 00 is required. No implementation subject starts until this subject is
> `DONE`, `ABANDONED`, or waived by decision. A failing Build/Test baseline from
> 00.3 blocks implementation subjects unless a §4 row accepts it.

- [ ] **00.1** (agent) Read repo instructions and authoritative docs (`README.md`, `docs/architecture.md`, `docs/decisions.md`, `tasks/lessons.md`, `tasks/claudex-refactor/`); list constraints, existing conventions, and the intended pair-programming direction so later subjects flag drift. Record the vision→code gap explicitly (README describes the loop; code drives subprocesses).
- [ ] **00.2** (agent) Prior-art / reuse audit: map every module under `claudex/` to keep / adapt / retire for the attach model, and **correct the seeded source map** used across subject Integration analyses (the seeds were inferred from README + filenames, not source — CX flagged these during plan review; verify each against the code):
      - `Phase` and `RunState.advance` live in **`state.py`** (not `phases.py`).
      - `phases.py` is a large **subprocess-coupled `Orchestrator`**, NOT a reusable transition table — adapt or retire, do not assume reuse.
      - Convergence helpers live in **`schemas.py`**.
      - `agents.py` holds **provider command/result adapters** (`ClaudeAgent`/`CodexAgent`) — provider flags live here, not "reusable role concepts."
      - `terminal.py` renders/watches; **Windows Terminal launching is `cli.py:open_watch_terminals`** (the real launch surface for subject 07).
      - `artifacts.py:save_json` / `mailbox_append` are **non-atomic** (subject 02 must add atomic writes, not "reuse an immutable artifact dir").
      - `evidence.py` **copies selected files from the live `cfg.repo`** — not a committed-object materializer; subject 04 must change this.
      - `budgets.py` = provider usage/economic accounting; `limits.py` = provider rate-limit **text parsing** — not a general cap engine (subject 05 owns cap policy).
      - `providers.py` probes capabilities; actual provider command flags are largely in `ClaudeAgent`/`CodexAgent`.
      Output a prior-art map naming the real symbols each new subject reuses vs retires. Candidate retirees: `processes.py` provider-invocation path, `agents.py` provider drivers, `providers.py` invocation glue, `cli.py cmd_run` + old `pair` dispatch.
- [ ] **00.3** (agent) Run baseline `python -m pip install -e .`, `python -m compileall claudex`, `python -m unittest discover -s tests`; confirm/correct the §2 Verification-commands table or record exact blockers + reproduction. Note which existing tests assume the subprocess driver (will be retired/rewritten).
- [ ] **00.4** (agent) Research current best practices for the detected stack (Python 3.10+ stdlib-only; `argparse` CLI; `unittest`): durable-state/CAS patterns on a filesystem, atomic file writes (temp→fsync→rename) cross-platform incl. Windows, advisory file locking on Windows vs POSIX, long-poll/bounded-wait ergonomics, and — critically — a **crash-stale lock reclaim** mechanism that is safe without PID-only detection (OS-held advisory lock that dies with the process, or nonce + process-start-time identity, or another proven liveness signal). Pick the mechanism 01.4 will implement. Record only findings that affect this plan.
- [ ] **00.5** (agent) Research the interactive-terminal + provider constraints: how Claude Code and Codex CLIs behave as **interactive** sessions (not `-p`/`exec` headless), whether each can be launched with inherited stdio + pinned cwd + sandbox/tool flags (managed tier), and whether any provides a **usage/cost telemetry side-channel** consumable without breaking TUI semantics (informs D007 / subject 07). Record links, versions, date-sensitive notes. Cross-check `[[codex-cli-binary-quirks]]` (desktop `codex.exe` only; strict schemas).
- [ ] **00.6** (agent) Research applicable skills/MCP/local tools (e.g. `plan-from-template` already in use, `verify`, `cleanup-audit`, `captain-hindsight`); classify each `adopt`/`defer`/`reject` with rationale, trust notes, source, setup cost.
- [ ] **00.7** (agent) Set up only approved local tooling/config. Anything adding credentials, network services, or repo-config permission grants needs a human-owned box mirrored into `manual-actions.md`. Record install/config/provenance; never commit secrets.
- [ ] **00.8** (agent) Write the protocol/architecture **ADR** (into `docs/decisions.md` or a new ADR) capturing D001–D011 and the transport/state/git-transaction contract. Include the **legacy in-flight run compatibility decision (D011)**: pre-pivot active `state.json`/`current` runs are not silently migrated — define the state major-version bump and the fail-closed `resume` behavior (inspect/export allowed; resume refuses with remediation unless a proven converter exists). Bake adopted findings into §2/§6/§7, subject boxes, and lessons. Inventory doc surfaces to update (README rewrite, architecture doc, skill docs). End with an implementation-readiness summary.
- [ ] **00.9** (product-owner) Confirm the base branch (D009): branch from `main`, or continue on `reliability-observability-refactor`? Mirrored in `manual-actions.md`.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice. Date · slice number · box IDs touched · what shipped · how verified · checkpoint commit/push status.
