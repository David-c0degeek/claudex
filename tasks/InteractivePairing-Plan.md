# InteractivePairing Plan

> Reshapes claudex from a headless subprocess orchestrator into a coordinator
> for two **human-interactive** agent terminals (Claude Code + Codex) that
> pair-program on one plan and one implementation, converging by agreement.
> Disposable plan: this file and `tasks/interactive-pairing/` are deleted after
> §8 sign-off. Shipped code must not reference them (§6.11).

## Collaboration model

| Field | Value |
|---|---|
| Mode | `coordinated` |
| Primary owner | agent (CC as lead for this plan; CX pairs) |
| Coordinator | Primary owner |
| Resume safety | required |
| Parallel branches | `no` |
| Notes | Plan authored by CC (lead) + CX (pair) over the `.mailbox/` file channel — the very pattern being productized. Base `main`, work branch `interactive-pairing` (D009). Remote `origin` exists — push checkpoints. Human sign-off boxes tracked in `manual-actions.md`. |

Mode rules:
- `coordinated` — one primary worker (the active lead agent) owns the plan, with explicit non-agent boxes for base-branch confirmation, product sign-off, and release. Uses `manual-actions.md`; no parallel branch table required.

### Parallel work tracker

| Subject | Owner | Branch | Dependencies | Conflict-risk files | Status | Handoff notes |
|---|---|---|---|---|---|---|
| n/a | | | | | | |

### Checkpoint gate

Before considering a box done:
- Update the box, subject Progress log, §5 tracker, and any affected §4 decisions / lessons / manual actions.
- Run the relevant §2 Verification command, or record the exact blocker + reproduction command.
- **Behaviour verification:** for a box that changes runtime behaviour, exercise the changed flow end-to-end and record observed-vs-expected output — not a green test exit alone. For this plan the canonical drive is: two attached sessions complete a `pull → submit → wait` turn and the coordinator advances state + records evidence.
- **Doc-currency:** update every dependent doc in the same checkpoint (README, `docs/architecture.md`, `docs/decisions.md`, skill docs), or record `n/a` + reason.
- Commit the coherent checkpoint; push if a remote exists, else record the no-remote note in Notes and treat the local commit as the checkpoint.
- Never rewrite pushed checkpoint history.
- Keep commit messages / branch names / PR titles plan-agnostic (§6.11).

---

## 1. Subject

**In scope.** Pivot claudex's execution model from "coordinator spawns both agents as headless subprocesses" to "coordinator arbitrates two already-running, fully human-interactive agent terminals over a durable file protocol." Deliver: a **processless durable coordinator** (short-lived CLI + locked durable state + CAS transitions + crash-reconciled git/state transaction journal); a **transport protocol** (`pull` / `submit` / `wait --timeout` / `status`) over a role-addressed file mailbox with idempotent receipts; an **attach + phase engine** (register a terminal to a role, run PLAN→IMPLEMENT→CHECKPOINT→TESTS→VERIFY→DONE firing on `submit`, converge on AGREE + zero blocking/major); **coordinator-owned git** (snapshot-and-commit on submit, immutable review evidence, mechanical test gate, merge gating); **human gates + observable-only caps**; **executable agent-side skills** (the `pull→work→submit→wait` loop for Claude and Codex, making BYO autonomous); and a **managed-interactive launcher** (claudex starts each TUI with inherited stdio, pinned cwd, provider sandbox flags — still fully interactive) with individually-labeled enforcement capabilities.

**Out of scope.** Multi-user / cross-machine auth (same-user filesystem assumed — stated, not solved). Network/RPC transport (files only). Rotating leadership or model-vs-model authority. A second headless orchestration engine (the old subprocess driver is retired; a future headless adapter, if ever built, is just another `pull/submit` client and is not part of this plan). Metered token/cost caps in BYO attach (impossible to observe honestly). Sandboxing an untrusted repository (a worktree is an isolation boundary for commits, not an OS sandbox).

### Risks and rollback

| Risk | Impact | Mitigation / rollback |
|---|---|---|
| Git ref update and state CAS cannot commit atomically; crash between them corrupts run | High — orphaned commit or lost turn | Prepared-transaction journal written before ref move; startup reconciliation replays or rolls back the incomplete transaction; review evidence is the committed tree, never the live worktree (D006, subject 01/04) |
| BYO attach cannot detect out-of-worktree writes; false sense of enforcement | Med — user trusts a boundary that doesn't exist | Label BYO capability `protocol-only`; individually-labeled managed capabilities; docs state the threat model plainly (D005/D007, subject 05/07) |
| Retiring headless `run` deletes working (if wrong-shaped) code | Med — regression / lost salvage | 00 reuse audit decides survivors before deletion; git retains history; deletions land as their own reviewable checkpoints |
| Two humans submit/resolve concurrently against stale generations | Med — wrong turn/gate answered | Unique immutable `turn_id`/`gate_id` + `state_revision`, accept-once under lock; stale/conflicting rejected with current status (D004, subject 01/03) |
| Managed launch promises telemetry it can't deliver | Med — dishonest caps | Research metering channel in 00; label `sandboxed` vs `telemetry-observed` separately; never a blanket "enforced" (D007, subject 07) |
| Interactive agent commits or edits after snapshot | Med — review sees wrong tree | Skill instructs agents never to `git commit`; post-snapshot edits are uncommitted protocol violations that block the next lead submit and are surfaced, never absorbed (subject 04/06) |
| Pre-pivot in-flight headless run reinterpreted under attach semantics | High — silent state corruption of an existing run | State major-version bump; `resume` fails closed with remediation; inspect/export only; converter is opt-in and proven (D011, subject 00.8/01.7) |
| Coordinator commit leaves the checked-out index describing old HEAD → worktree always looks dirty → the next-submit block trips forever | High — MVP deadlocks after the first commit | 04.2 synchronizes the real index to the new tree (safe mixed reset, no file overwrite) and re-verifies clean; the index step is in the journal crash-cut matrix (subject 04) |
| Reviewer evidence sourced from the live worktree instead of the committed object | High — pair reviews mutable, wrong, or racing content | `evidence.py` materializes files from the committed Git object + hashes the packet; never copies from live `cfg.repo`; skills point the pair at the packet, never the lead worktree (subject 04/06) |
| Two simultaneous first-attaches race before the run dir exists | Med — split-brain run allocation | Repository-level nonce-based allocation/current-pointer lock+CAS, not only a run-dir lock; release only the acquired lock instance (subject 01.4) |
| A crashed short-lived CLI leaves a lock that no PID-only check can safely reclaim → repo bricked | Med — no run can ever start again | 00.4 chooses + 01.4 implements a proven liveness/reclaim mechanism (OS-held advisory lock, or nonce + process-start identity), with crash/reclaim tests; "no PID-only detection" must not mean "never reclaimable" (subject 01.4) |

---

## 2. Authoritative inputs

| Source | Contribution |
|---|---|
| `main` branch (base, confirmed) | Merge base / target this plan branches from and merges back into; work happens on `interactive-pairing` (D009) |
| Current `reliability-observability-refactor` branch | Existing engine modules audited for reuse (`state.py`, `schemas.py`, `gitops.py`, `budgets.py`/`limits.py`, `phases.py`, `lifecycle.py`, `security.py`, `evidence.py`) |
| `README.md` | Documents the intended pair-programming loop, convergence semantics, artifact/mailbox formats — the vision the code must now actually realize |
| `docs/architecture.md`, `docs/decisions.md` | Prior architecture + durable rationale to update, not fork |
| `.mailbox/` proof-of-concept (this session) | Working reference implementation of the attach transport (`to-codex.md`/`to-claude.md`, TURN sequencing, blocking-poll loop) |
| `tasks/claudex-refactor/lessons.md` + `tasks/lessons.md` | Prior-run lessons (streaming safety, usage-at-boundary, lifecycle vs phase, evidence manifests, resume-vs-restart, cancellation, tree identity) — reuse where they still apply |
| CC↔CX design exchange (mailbox TURN 3–4) | The converged architecture — source of the §4 decision seeds |

### Verification commands

| Purpose | Command | Notes |
|---|---|---|
| Build / install | `python -m pip install -e .` | Confirm in 00.3 |
| Compile-check | `python -m compileall claudex` | Fast structural check |
| Test | `python -m unittest discover -s tests` | Existing suite is stdlib `unittest`; confirm exact invocation in 00.3 |
| Lint/format | TBD | `n/a` unless 00.3 finds ruff/black config |
| Plan-specific gate | Two-session attach e2e (subject 06) | Scenario-driven fake TUIs drive `pull/submit/wait`; asserts state advance + evidence |

---

## 3. Subject file index

| # | File | Subject | Depends on |
|---|---|---|---|
| 00 | `tasks/interactive-pairing/00-tooling-research-and-readiness.md` | Tooling research, reuse audit, protocol ADR, base-branch decision | — |
| 01 | `tasks/interactive-pairing/01-durable-state-core.md` | Durable state, CAS transitions, git/state transaction journal, crash reconciliation | 00 |
| 02 | `tasks/interactive-pairing/02-transport-protocol.md` | `pull`/`submit`/`wait`/`status` CLI, role-addressed mailbox, atomic artifacts, idempotent receipts | 01 |
| 03 | `tasks/interactive-pairing/03-attach-and-phase-engine.md` | `attach`/registration, assignment issuance, phase graph, convergence | 02 |
| 04 | `tasks/interactive-pairing/04-git-transaction-and-review-evidence.md` | Coordinator snapshot/commit, immutable review evidence, test + merge gate | 03 |
| 05 | `tasks/interactive-pairing/05-human-gates-and-caps.md` | Human gates (`resolve`/`continue`), observable-only caps, operator lifecycle, status/recovery UX | 04 |
| 06 | `tasks/interactive-pairing/06-agent-skills-and-byo-integration.md` | Executable Claude/Codex skill clients + BYO end-to-end, concurrency, crash/fault tests (**MVP milestone**) | 05 |
| 07 | `tasks/interactive-pairing/07-managed-launcher-and-release.md` | Managed-interactive launcher, enforcement capability matrix, docs/release | 06 |

---

## 4. Decision log

| ID | Date | Title | Decision | Rationale | Refs (box IDs / files / ADR#) |
|---|---|---|---|---|---|
| D001 | 2026-07-16 | Attach over subprocess driving | Coordinator arbitrates two already-running human-interactive TUIs over a file protocol; it does not spawn agents as headless children | The product is human-in-both-terminals pair programming; the subprocess driver is the wrong shape (confirmed: `processes.py` Popen pipes; `pair` still spawns the pair headlessly) | `processes.py`, `agents.py`, `providers.py`; ADR TBD in 00 |
| D002 | 2026-07-16 | Processless durable coordinator | Short-lived CLI + durable state + per-mutation lock; every transition is an atomic CAS on `(run_id, state_revision, turn_id)`; ledger is a projection of accepted artifacts; `wait` holds no lock | A daemon adds lifecycle/recovery failure modes without adding correctness | subject 01; `state.py` |
| D003 | 2026-07-16 | One orchestration protocol | Drop the public headless `run` driver; a future headless adapter is just another `pull/submit` client, not a second engine | Keeping it doubles the driver/state/recovery/test matrix and preserves what we're replacing | subject 00 (retirement plan), `cli.py cmd_run` |
| D004 | 2026-07-16 | Identity, not leases | Unique immutable `turn_id`/assignment ID + `gate_id`, each with `state_revision`, accepted once under lock; duplicate identical submit → prior receipt; stale/conflicting → rejected with current status | Lease expiry creates ambiguity during long human/model turns; tokens guard misrouting, not security (shared FS) | subject 01/03 |
| D005 | 2026-07-16 | Two tiers, honestly labeled | (1) BYO attach = cooperative, `protocol-only`; (2) managed-interactive launch = inherited stdio + pinned cwd + provider sandbox flags, capabilities labeled individually | Interactive BYO cannot observe out-of-worktree writes or usage; managed recovers *some* enforcement but not all | subject 05/07 |
| D006 | 2026-07-16 | Coordinator-owned git commit | Agents edit but never commit; `submit` builds the snapshot in a temp index, commits from that tree, CAS-updates the run branch from expected old HEAD, journals a prepared transaction, reconciles on restart; review uses the committed tree only | Keeps git truly coordinator-owned + authoritative tree identity; git-ref and state-CAS cannot be atomic, so a journal + reconciliation is required | subject 01/04; `gitops.py` |
| D007 | 2026-07-16 | Observable-only caps | Attach enforces turn/fix counts, artifact bytes, wall time; token/cost/tool-call metering requires managed launch with a provider telemetry side-channel (or PTY/ConPTY proxy); never accept agent self-report as enforcement | Attach cannot observe a user's existing TUI usage | subject 05/07 |
| D008 | 2026-07-16 | MVP = 00–06 | BYO-attach is the MVP tier, milestone at subject 06 (skills are executable protocol clients, not docs); managed launch + release is 07 | Without the skill clients, BYO is a human manually shuttling JSON, not autonomous | §3, subject 06 |
| D009 | 2026-07-16 | Base branch | RESOLVED: fresh branch `interactive-pairing` from `main`. The prior `reliability-observability-refactor` WIP is stashed/parked (not carried forward); its committed engine modules are audited for reuse from git history in subject 00 | Human chose a clean base over continuing the throwaway-driver branch; reuse is by deliberate audit, not by inheriting uncommitted WIP | 00.1/00.3 + `manual-actions.md` |
| D010 | 2026-07-16 | Verifier context independence | VERIFY **blocks until a new same-role session generation attaches** (single locked behavior — no silent proceed-with-downgrade). The incumbent pair generation is invalid for VERIFY. BYO reports `fresh-session-declared` (session generation enforced; model-context freshness is not); managed reports `fresh-process` only when it truly launches one. Allowing a downgrade requires a later explicit decision/gate | A long-lived attached pair terminal is not context-fresh; the README's fresh-verifier promise cannot silently carry over to attach, and a nondeterministic "refuse-or-label" outcome is not a spec | subject 03.8, 06 skills, 07 labels |
| D011 | 2026-07-16 | Legacy in-flight run compatibility | Pre-pivot active `state.json`/`current` runs are NOT silently migrated into attach semantics. Bump the state major version; behavior is: inspect/export allowed, `resume` fails closed with remediation, unless a proven converter is written | 01.7-style field migration could otherwise reinterpret an in-flight headless run as an attach run and corrupt it | 00.8, 01.7, §1 risks |

---

## 5. Master progress tracker

| Done | # | File | Status | Owner summary | Human actions mirrored? |
|---|---|---|---|---|---|
| [ ] | 00 | `tasks/interactive-pairing/00-tooling-research-and-readiness.md` | TODO | agent: 8; product-owner: 1 | yes |
| [ ] | 01 | `tasks/interactive-pairing/01-durable-state-core.md` | TODO | agent: 7 | n/a |
| [ ] | 02 | `tasks/interactive-pairing/02-transport-protocol.md` | TODO | agent: 6 | n/a |
| [ ] | 03 | `tasks/interactive-pairing/03-attach-and-phase-engine.md` | TODO | agent: 9 | n/a |
| [ ] | 04 | `tasks/interactive-pairing/04-git-transaction-and-review-evidence.md` | TODO | agent: 6 | n/a |
| [ ] | 05 | `tasks/interactive-pairing/05-human-gates-and-caps.md` | TODO | agent: 7 | n/a |
| [ ] | 06 | `tasks/interactive-pairing/06-agent-skills-and-byo-integration.md` | TODO | agent: 7 | n/a |
| [ ] | 07 | `tasks/interactive-pairing/07-managed-launcher-and-release.md` | TODO | agent: 6; release-engineer: 2 | yes |

---

## 6. Cross-cutting principles

1. KISS · YAGNI · CLEAN · SOLID · DRY (in that order when conflicting).
2. ~~*(C#)* One class per file.~~ (not applicable — Python)
3. ~~*(C#)* Nullable reference types enabled.~~ (not applicable — Python)
4. ~~*(.NET)* No new build warnings.~~ (not applicable — Python)
5. **Keep code modular and locally understandable.** Small cohesive modules with explicit boundaries; split files before they become dumping grounds; split functions when branching/nesting/mixed responsibility hurts scanning.
6. **Cyclomatic complexity stays low.** Prefer guard clauses, extracted decision helpers, table-driven cases over deep nesting. Double-digit complexity is design pressure to simplify.
7. **Spec at the contract level, not the SDK level.** State testable contracts on observable behaviour (state transitions, artifacts, git tree identity), not internal call shapes.
8. **Coverage % is a smell-detector, not a goal.** Each test pins observable behaviour; if you can't name the bug it prevents, delete it.
9. **Every plan box has an owner and a stable ID** from the §5 enum.
10. **Lessons land in `tasks/interactive-pairing/lessons.md` as they happen**, migrating durable ones to `tasks/lessons.md` at §7.
11. **Code and public metadata are plan-agnostic.** No plan/box/decision refs in comments, identifiers, commits, branches, PRs. Design rationale goes in an ADR, not a plan ref.
12. **Captain Hindsight review before subject close.**
13. **Tooling research before implementation** (subject 00) unless waived by a §4 decision.
14. **All plans are resume-safe** — a ticked box is backed by a durable checkpoint (plan update + verification/blocker + commit + push, no-remote exception).
15. **Parallelism is opt-in** — this plan is `coordinated`, not `parallel`.
16. **Documentation ships with the change** — README, `docs/architecture.md`, `docs/decisions.md`, skill docs updated in the same checkpoint or `n/a` + reason.
17. **Reuse and integrate before adding** — each implementation subject records an `## Integration analysis` naming concrete files/symbols before any box; new parallel code only when no existing surface fits. Wire or delete new public surface.
18. **No transition without CAS.** Every state mutation is an atomic compare-and-swap against the expected `state_revision`; torn or unguarded writes are blockers. Artifacts are written temp → fsync → rename before the state/ledger advances.
19. **Immutable evidence, never the live worktree.** Reviews, diffs, and convergence checks read committed/immutable artifacts identified by hash. The mutable worktree is never a review input; post-snapshot edits are surfaced as protocol violations, never silently absorbed.
20. **Honesty labels — never label a capability enforced without a mechanism.** BYO attach is `protocol-only`; managed capabilities are labeled individually (`sandboxed`, `telemetry-observed`, …). Agent self-report is never enforcement. Every cap/label in `status` names the mechanism that backs it.
21. **Two-store operations are journaled + reconciled.** Any operation spanning git and coordinator state writes a prepared-transaction record first and is replayed or rolled back on startup; the code never assumes the two stores committed atomically.
22. *(plan-specific principles above are 18–21; keep numbering stable)*

---

## 7. Gate review (run last; tick everything)

- [ ] All §5 subjects done (or explicitly `ABANDONED` with a §4 row)
- [ ] Subject 00 completed, or explicitly waived/abandoned with a §4 row
- [ ] Build/install + compile-check commands from §2 pass
- [ ] Test command from §2 passes; the two-session attach e2e (subject 06) passes
- [ ] Behaviour verification done for every runtime-behaviour box — the changed flow driven end-to-end with observed-vs-expected output recorded (canonical: two attached sessions complete a real turn)
- [ ] Remaining §2 rows (lint/format, plan-specific gates) pass or recorded `n/a`
- [ ] §1 Risks-and-rollback table reviewed; rollback steps still accurate for what shipped
- [ ] §6 principles reviewed; CAS/immutable-evidence/honesty-label/journal rules hold in shipped code
- [ ] Every implementation subject recorded an `## Integration analysis`; no unjustified duplication
- [ ] New public surface wired to a non-test caller, or disclosed library-only/unwired
- [ ] Every non-abandoned subject has a Captain Hindsight verdict `CLOSE`
- [ ] Every ticked box has a Progress-log entry + pushed checkpoint commit (or no-remote note)
- [ ] Durable architecture decisions (D001–D009 + protocol ADR) promoted to `docs/decisions.md`/ADR
- [ ] Documentation-impact review done — README + architecture/decision docs + skill docs match shipped behaviour, or `n/a` + reason
- [ ] Current branch pushed (or no-remote note); `git status --short` clean except deferred/ignored
- [ ] Shipped code/tests/comments/identifiers plan-agnostic — grep excluding `tasks/` for box IDs (`\b\d\d\.\d+\b`), decision IDs (`\bD\d{3}\b`), `tasks/interactive-pairing/`, `InteractivePairing-Plan.md`, `\bslices?\b`; zero hits after triage
- [ ] Commit messages plan-agnostic — `git log <base>..HEAD`
- [ ] Branch names / PR titles plan-agnostic if used
- [ ] `manual-actions.md` — every human-owned box resolved or explicitly deferred
- [ ] Retirement of the old headless driver (`run`, `processes.py` Popen path, `agents.py`) is complete or explicitly deferred with a §4 row; no dead half-removed engine left
- [ ] Managed-launch capabilities are individually labeled in `status`; no blanket "enforced" bit shipped
- [ ] `cleanup-audit` teardown review run before sign-off, or waived by a §4 row; findings triaged (fix-now / new plan / won't-fix)
- [ ] `tasks/interactive-pairing/lessons.md` reconciled; lasting lessons migrated to `tasks/lessons.md`
- [ ] Plan handed to reviewer for §8 sign-off

---

## 8. Acceptance / sign-off

| Date | Reviewer | Result | Notes |
|---|---|---|---|
| | | | |

---

## Appendix: Captain Hindsight Prompt

```text
You are now Captain Hindsight.

Review the completed subject, phase, box, or major plan section with hindsight.
Assume the work is already done, then identify what is clearer now than it was
before the work started.

Check specifically for:
- Scope drift or missed requirements.
- Spec deviations that need a Decision-log row (and whether durable enough to promote to an ADR / docs/decisions.md).
- Lessons that should be recorded before context is lost.
- Tests that pin implementation details instead of observable behavior.
- Complexity, duplication, brittle design, or awkward naming that should be fixed now.
- Behaviour closed on a green build alone: a box marked done without the changed flow exercised end-to-end (two attached sessions completing a real turn).
- Reuse missed or vision drift: new parallel code where existing engine modules (state/schemas/gitops/budgets/lifecycle) should have been extended.
- A capability or cap labeled "enforced" without a real mechanism backing it (honesty-label violation).
- A git+state operation that assumed atomicity instead of journaling + reconciliation.
- Human-owned actions that need to be mirrored or resolved.
- Docs that drifted from shipped behaviour (README, docs/architecture.md, docs/decisions.md, skill docs).
- Plan references that leaked into shipped code, tests, comments, identifiers, commit messages, branch names, PR titles/descriptions.

Before returning a verdict, confirm the claimed verification against the
artefacts: rerun the relevant verification command or cite its recorded output.

Return exactly these sections:
1. Keep: what was correct and should remain.
2. Fix before closing: concrete issues, missing tests, spec drift, plan hygiene, or design problems — cite the file, test, or commit for each item.
3. Record: decisions or lessons that must be added to the plan files.
4. Risk: anything still uncertain after verification.
5. Verdict: CLOSE or DO NOT CLOSE.

If the verdict is DO NOT CLOSE, list the smallest concrete actions needed before
the work can be closed.
```
