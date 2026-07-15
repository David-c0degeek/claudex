# ClaudexRefactor — Multi-Slice Implementation Plan

> **Purpose.** Coordinate the reliability and observability refactor specified
> in `refactor.md` across provider execution, orchestration, lifecycle safety,
> tests, and operator UX.
>
> **Disposable.** This plan and `tasks/claudex-refactor/` are deleted or
> archived out of the repo only after §8 sign-off. The agent never deletes the
> plan folder. Shipped code, tests, comments, identifiers, branch names, and
> commit messages must not depend on these files. Commit the plan while it is
> live so another session can resume safely.

> **Terms.** A subject is one numbered file under `tasks/claudex-refactor/`.
> A box is a stable `NN.ordinal` checklist item. A slice is one work
> session recorded in that subject's Progress log. A checkpoint is a durable
> handoff containing updated plan state, verification evidence, and a pushed
> coherent commit (or a recorded no-remote exception).

---

## Collaboration model

| Field | Value |
|---|---|
| Mode | `solo` |
| Primary owner | `agent` |
| Coordinator | same as Primary owner |
| Resume safety | required |
| Parallel branches | `no` |
| Notes | The dirty `main` baseline was inventoried, verified, and preserved unchanged as `a23224a` on `reliability-observability-refactor`. `origin` is available; checkpoint commits will be pushed without history rewrites. |

Mode rules:

- `solo` means one active worker owns the plan at a time. Checkpoint commits and
  pushes remain required so a later session or worker can resume.
- Upgrade to `coordinated` or `parallel` only through a §4 decision that records
  the ownership, branch, dependency, and conflict implications first.
- Human-owned boxes, if added later, must be mirrored into
  `tasks/claudex-refactor/manual-actions.md`.

### Parallel work tracker

| Subject | Owner | Branch | Dependencies | Conflict-risk files | Status | Handoff notes |
|---|---|---|---|---|---|---|
| n/a | | | | | | Mode is `solo` |

### Checkpoint gate

Before considering a box done:

- Update the box, its subject Progress log, §5, and any affected decisions,
  lessons, or manual actions.
- Run the relevant §2 verification command, or record the exact blocker and
  reproduction command.
- For runtime behavior, exercise the changed flow end to end and record
  observed versus expected output. A passing unit command alone does not close
  a behavioral box.
- Review documentation impact. Update affected user and contributor docs in the
  same checkpoint, or record `n/a` with a reason.
- Commit the coherent checkpoint and push it. If no remote exists, record that
  once in Collaboration model Notes and use the local commit as the checkpoint.
- Never rewrite pushed checkpoint history.
- Keep commit messages, branch names, and PR metadata permanent and
  plan-agnostic: no box IDs, decision IDs, plan paths, or slice terminology.

---

## 1. Subject

Refactor Claudex so provider execution is live-observable, economically bounded,
semantically correct, recoverable across pause/restart, safe at process and Git
boundaries, and covered by deterministic subprocess and worktree tests. Deliver
readable `watch`/`status` output and optional Windows Terminal watcher windows.
Retain structured non-interactive provider control and the coordinator-owned
state machine. Out of scope are a general chat UI, keystroke-driven provider
TUIs, unrelated feature work, and promises of exact cost when a provider does
not report authoritative data.

### Risks and rollback

| Risk | Impact | Mitigation / rollback |
|---|---|---|
| Stream schemas change across Claude or Codex versions | Paid runs fail or telemetry is misread | Capability-check before a run; isolate provider decoders; retain raw events; use versioned fixtures; temporarily select the proven compatibility adapter |
| Run-state migration loses resumable work | Existing runs become unrecoverable | Version migrations; atomic replace; retain original state; test representative old state; stop with an export/recovery path when equivalence is unknown |
| Claudex reports incomplete usage as a complete budget picture | Operator makes a false cost assumption | Label values as reported/derived/unknown; enforce native provider caps where available; never synthesize missing cost |
| Timeout/cancel leaves descendant processes alive | Continued spend, file mutation, or lock contention | Process groups/job objects; descendant fixtures; durable cancellation reason; fail closed when tree termination cannot be proven |
| Git gates omit files or test a different tree than integration uses | Unverified or missing work ships | Include untracked files; record exact tree/patch; use real temporary repos and worktrees in integration tests |
| Live logs expose secrets or grow without bound | Credential disclosure or disk exhaustion | Redaction before persistence/display; configurable retention; bounded readable logs; keep compact summaries |
| Refactor overwrites the current dirty workspace | Existing pair-programming changes are lost or conflated | Subject 00 records the exact baseline and overlap; preserve user work; create a safe branch/checkpoint strategy before editing overlapping files |
| New behavior regresses on unsupported terminals/platforms | Headless or non-Windows users cannot run Claudex | Make event journal independent of terminal launch; degrade to printed watcher commands; retain headless tests |

---

## 2. Authoritative inputs

| Source | Contribution |
|---|---|
| `main` branch at HEAD `7ab861f` plus the current uncommitted workspace | Merge target and actual starting implementation, including the in-progress pair-programming rewrite that must be preserved |
| `refactor.md` | Durable problem statement, fixed design decisions, target invariants, delivery sequence, and acceptance criteria |
| `claudex/agents.py`, `phases.py`, `schemas.py`, `state.py`, `config.py`, `cli.py`, `gitops.py`, `artifacts.py`, and `prompts.py` | Current provider, coordinator, state, policy, Git, artifact, and prompt integration seams |
| `tests/` | Existing regression conventions and current mocked orchestration coverage |
| Retained incident artifacts under `D:\repos\ai-nemo-engineer\.claudex\runs` | Reproducible evidence for context growth, cost, false gating, overwritten attempts, rate-limit sleep, and restart loss; read-only diagnostic input |
| Installed Claude and Codex CLI help plus official provider documentation | Current streamed-output, schema, budget, permission, and capability contracts; re-verify in subject 00 because these are version-sensitive |
| `D:\repos\c0degeek-ai\skills\plan-from-template` | Required planning structure, readiness gate, checkpoint discipline, and Hindsight review protocol |

### Verification commands

| Purpose | Command | Notes |
|---|---|---|
| Build | `python -m compileall -q claudex` | Fast syntax/import byte-compilation gate; subject 00 confirms packaging expectations |
| Test | `python -m unittest discover -s tests -v` | Existing project test command; later subjects add deterministic fake-provider and worktree coverage |
| Lint/format | `git diff --check` | No configured Python linter was found during plan creation; subject 00 must confirm or correct this row |
| CLI smoke | `python -m claudex --version` | Confirms module entry point remains usable |
| Fake-provider behavior gate | `python -m unittest tests.test_integration -v` | Must run a slow streamed provider, watcher, budget stop, restart, rate-limit, cancel, and worktree scenarios without paid providers |
| Opt-in live-provider smoke | `python -m unittest tests.test_live_providers -v` | Default-skipped unless an explicit environment opt-in is present; must have strict native and coordinator spend caps |

---

## 3. Subject file index

| # | File | Subject | Depends on |
|---|---|---|---|
| 00 | `tasks/claudex-refactor/00-tooling-research-and-readiness.md` | Tooling research and readiness | — |
| 01 | `tasks/claudex-refactor/01-streaming-and-observability.md` | Provider streaming, event journal, and live visibility | 00 |
| 02 | `tasks/claudex-refactor/02-usage-and-budget-controls.md` | Usage accounting, cost policy, and hard admission controls | 01 |
| 03 | `tasks/claudex-refactor/03-orchestration-and-recovery.md` | Gate semantics, bounded convergence, lifecycle, and non-lossy restart | 00, 01 |
| 04 | `tasks/claudex-refactor/04-process-git-and-provider-safety.md` | Cancellation, rate-limit behavior, Git integrity, provider policy, and artifact safety | 01, 03 |
| 05 | `tasks/claudex-refactor/05-integration-and-fault-testing.md` | Deterministic provider, process, state, and worktree integration suite | 01, 02, 03, 04 |
| 06 | `tasks/claudex-refactor/06-terminal-ux-docs-and-release.md` | Watch/status/open-terminal UX, migration docs, and release acceptance | 02, 03, 04, 05 |

---

## 4. Decision log

Append-only. Record every deviation from `refactor.md` or this plan, every
collaboration-mode change, and every retired box. Cite affected box IDs/files.
Durable architecture decisions must be promoted to the project's permanent
architecture decision record before §7; this temporary log is not sufficient.
Retired boxes remain visible as checked, struck-through `ABANDONED` entries.

| ID | Date | Title | Decision | Rationale | Refs (box IDs / files / ADR#) |
|---|---|---|---|---|---|
| D001 | 2026-07-15 | Preserve dirty starting baseline | Commit the already-passing 0.4 pair-programming rewrite unchanged on the refactor branch before implementation. | It prevents the user's pre-existing work from being overwritten or silently mixed into refactor checkpoints. | 00.2; `a23224a` |
| D002 | 2026-07-15 | Keep CLI provider boundary | Consume Claude stream-json and Codex exec JSONL through provider adapters; do not add Agent SDK runtime dependencies. | Both installed CLIs expose stream, schema, session, usage, and permission contracts; this retains the stdlib-only product and structured non-interactive control boundary. Promote to permanent architecture docs in 06.4. | 00.4, 00.5, 01.1–01.4, `refactor.md` |
| D003 | 2026-07-15 | Offline suite is authoritative | Deterministic fake executables and temporary Git repos are the default correctness gate; live providers are an explicit, capped compatibility smoke only. | Correctness must not require credentials, spend, rate-limit availability, or nondeterministic model behavior. Promote to permanent test docs in 06.4. | 00.5, 00.8, 05.1–05.6 |
| D004 | 2026-07-15 | Terminal-only authoritative accounting | Charge each attempt idempotently from its provider terminal result; streamed cumulative usage is display-only. Preserve non-USD amounts separately, never convert or estimate missing cost, and require explicit acknowledgement/override before continuing when a reported currency cannot be reconciled with the USD run cap. | Intermediate usage semantics differ by provider and may be cumulative; local price inference would create false precision and double counting. Promote to the budgeting ADR in 06.4. | 02.1–02.6; `claudex/budgets.py` |
| D005 | 2026-07-15 | Explicit gates and bounded fresh planning | A human gate requires explicit true plus a non-empty question; finding labels never infer intent. Planning reviewers/revisions are fresh and consume immutable size-capped manifests; revisions are hash-guarded canonical section replacements. | This removes the exact incident false gate and prevents resumed conversation/history growth from multiplying context or corrupting an accepted baseline. Promote to orchestration/evidence ADRs in 06.4. | 03.1–03.3; `schemas.py`, `evidence.py`, `planops.py` |
| D006 | 2026-07-15 | Resume identity; restart checkpoint | `resume` continues the same durable run. `restart` changes execution identity only after copying and hash-verifying the latest canonical checkpoint; discarding planning state requires `--fresh-plan`. | Identity replacement is not permission to discard accepted work, decisions, budgets, worktree identity, or safe sessions. Promote to recovery ADR in 06.4. | 03.4–03.6; `lifecycle.py`, `recovery.py` |

---

## 5. Master progress tracker

A subject is `DONE` only when every box is checked and its Captain Hindsight
verdict is `CLOSE`. Every completed box needs a Progress-log record and durable
checkpoint. Owners are `agent`, `release-engineer`, `product-owner`,
`tech-lead`, or `domain-sme`; any new role requires a §4 decision.

| Done | # | File | Status | Owner summary | Human actions mirrored? |
|---|---|---|---|---|---|
| [x] | 00 | `tasks/claudex-refactor/00-tooling-research-and-readiness.md` | DONE | agent: 8 | n/a |
| [x] | 01 | `tasks/claudex-refactor/01-streaming-and-observability.md` | DONE | agent: 6 | n/a |
| [x] | 02 | `tasks/claudex-refactor/02-usage-and-budget-controls.md` | DONE | agent: 6 | n/a |
| [x] | 03 | `tasks/claudex-refactor/03-orchestration-and-recovery.md` | DONE | agent: 6 | n/a |
| [ ] | 04 | `tasks/claudex-refactor/04-process-git-and-provider-safety.md` | TODO | agent: 7 | n/a |
| [ ] | 05 | `tasks/claudex-refactor/05-integration-and-fault-testing.md` | TODO | agent: 6 | n/a |
| [ ] | 06 | `tasks/claudex-refactor/06-terminal-ux-docs-and-release.md` | TODO | agent: 7 | n/a |

---

## 6. Cross-cutting principles

Violations are blockers, not review nits. Keep numbering stable.

1. KISS · YAGNI · CLEAN · SOLID · DRY, in that order when they conflict.
2. ~~*(C#)* One class per file.~~ Not applicable to this Python project.
3. ~~*(C#)* Nullable reference types enabled.~~ Not applicable.
4. ~~*(.NET)* No new build warnings.~~ Not applicable; Python gates are in §2.
5. **Keep code modular and locally understandable.** Provider decoding, event journaling, lifecycle control, budgeting, and presentation have explicit seams; avoid new catch-all coordinator modules.
6. **Cyclomatic complexity stays low.** Prefer guard clauses, typed transitions, tables, and focused strategies over nested phase/flag branches.
7. **Specify observable contracts, not incidental SDK calls.** Tests pin events, state, artifacts, and operator behavior rather than a particular subprocess helper implementation.
8. **Coverage percentage is a smell detector, not a target.** Every new test states the regression or contract it protects.
9. **Every box has an owner and stable ID.** Never renumber; abandon through §4 instead of deletion.
10. **Record lessons when learned** in `tasks/claudex-refactor/lessons.md`; migrate durable lessons to `tasks/lessons.md` at §7.
11. **Shipped code and public metadata are plan-agnostic.** No task paths, box IDs, decision IDs, or slice language survives outside disposable plan artifacts.
12. **Run Captain Hindsight before closing every subject.** `DO NOT CLOSE` reopens the minimum required work.
13. **Tooling research precedes implementation.** Subject 00 establishes the real baseline, provider contracts, official sources, and approved tools.
14. **All progress is resume-safe.** A checked box has plan state, verification evidence, and a durable checkpoint; pre-existing dirty work is preserved explicitly.
15. **Parallelism is opt-in.** This plan remains solo unless §4 records the ownership and branch transition before concurrent work starts.
16. **Documentation ships with behavior.** Update README, CLI/config reference, architecture/decision docs, troubleshooting, examples, and prompt/skill docs when their contracts change.
17. **Reuse and integrate before adding.** Fill each subject's Integration analysis first. New public surfaces must have real non-test callers or explicit library-only documentation.
18. **No expensive work is invisible.** Persist and expose provider events, phase, elapsed time, tool activity, and available usage while an invocation is running.
19. **Budgets are coordinator invariants.** A new invocation never starts when its configured envelope is exhausted; provider-native caps are used when available; unknown usage stays labeled unknown.
20. **Provider adapters preserve differences at the edge.** Raw events and provider capabilities stay provider-specific; the coordinator consumes a small versioned `AgentEvent` and typed result contract.
21. **Human gates are explicit and actionable.** Findings never synthesize a gate. Protocol inconsistencies are bounded provider failures, not questions dumped onto the user.
22. **Pause, rate limit, cancel, failure, resume, and restart are durable transitions.** No default path hides multi-minute sleeps, loses the canonical checkpoint, or overwrites an attempt.
23. **The verified tree is the integrated tree.** Untracked files, partial output, migrations, process descendants, and platform-specific executable selection are covered by deterministic tests.

---

## 7. Gate review (run last; tick everything)

Run only when §5 is fully resolved. This is the engineering gate; §8 is the
separate user/reviewer acceptance.

- [ ] All §5 subjects are `DONE` or explicitly `ABANDONED` through §4
- [ ] Subject 00 completed, or was explicitly waived/abandoned through §4
- [ ] Build command from §2 passes with zero errors
- [ ] Test command from §2 passes; any repo-enforced coverage gate passes
- [ ] Runtime boxes have end-to-end observed-versus-expected evidence, not only a green unit command
- [ ] Remaining §2 lint, smoke, and plan-specific gates pass or are explicitly `n/a`
- [ ] §1 risks and rollback paths were rechecked against what actually shipped
- [ ] All §6 principles hold
- [ ] Every implementation subject contains concrete Integration analysis and no unjustified parallel implementation
- [ ] Every new public surface is wired to a non-test caller or explicitly documented as library-only/unwired
- [ ] Every non-abandoned subject has a Captain Hindsight verdict of `CLOSE`
- [ ] Every checked box has a Progress-log entry and pushed checkpoint (or recorded no-remote exception)
- [ ] Durable architecture decisions were promoted to the project's permanent decision/ADR record
- [ ] Documentation-impact review was completed for every shipped box
- [ ] The integration branch is pushed or covered by the no-remote exception; `git status --short` has no unexplained changes
- [ ] If mode became `parallel`, there are no stale owners, unmerged branches, or unresolved handoffs
- [ ] Shipped files outside `tasks/` contain no plan-only IDs, plan paths, or accidental slice terminology after false-positive triage
- [ ] `git log 7ab861f..HEAD` commit messages contain no plan-only IDs, paths, or slice terminology after false-positive triage
- [ ] Branch and PR metadata are plan-agnostic if used
- [ ] `tasks/claudex-refactor/manual-actions.md` contains no unresolved human-owned action
- [ ] Run the cleanup-audit teardown review, or waive it through §4; triage every finding into fix-now, a new plan, or won't-fix with reason
- [ ] Reconcile `tasks/claudex-refactor/lessons.md` and migrate lasting lessons to `tasks/lessons.md`
- [ ] The exact false-gate incident fixture does not gate when the explicit field is false and the question is absent
- [ ] A slow fake provider produces watcher-visible text/tool events before it exits and without stdout/stderr deadlock
- [ ] Rate limiting returns promptly into durable `RATE_LIMITED` state; no default path performs a long foreground sleep
- [ ] Usage totals and reported cost reconcile from attempt events into status; missing fields stay visibly unknown
- [ ] Budget exhaustion blocks the next invocation and resumes only through an explicit recorded override
- [ ] Restart preserves a hash-equivalent canonical checkpoint unless `--fresh-plan` is explicit
- [ ] Cancel/timeout terminate fake-provider descendants and retain unique partial attempt artifacts on Windows and supported CI platforms
- [ ] Real temporary Git/worktree tests prove untracked-file handling and verified-tree identity
- [ ] Watch/status golden tests cover running, paused, rate-limited, failed, cancelled, and completed states
- [ ] Any live-provider smoke used for release was opt-in and bounded by native plus coordinator spend caps
- [ ] Plan handed to reviewer for §8 sign-off

---

## 8. Acceptance / sign-off

Filled by the user or reviewer after §7 passes. Sign-off is acceptance, not a
spec amendment.

| Date | Reviewer | Result | Notes |
|---|---|---|---|
| | | | |

---

## Appendix: Captain Hindsight Prompt

This prompt is embedded so the plan remains self-standing.

```text
You are now Captain Hindsight.

Review the completed subject, phase, box, or major plan section with hindsight.
Assume the work is already done, then identify what is clearer now than it was
before the work started.

Check specifically for:
- Scope drift or missed requirements.
- Spec deviations that need a Decision-log row (and whether the decision is
  durable enough to promote to the project's ADR / decision log).
- Lessons that should be recorded before context is lost.
- Tests that pin implementation details instead of observable behavior.
- Complexity, duplication, brittle design, or awkward naming that should be fixed now.
- Behaviour closed on a green build alone: a box marked done on a passing build/test exit code without the changed flow exercised end-to-end (observed-vs-expected output).
- Reuse missed or vision drift: new parallel code where existing code should have been extended, or a change that drifts from the project vision/ADRs (cross-check the subject's Integration analysis).
- Human-owned actions that need to be mirrored or resolved.
- Docs that drifted from shipped behaviour — user- and developer-facing alike (README, usage/HOWTO guides, CHANGELOG, API/command/config reference, architecture/onboarding docs, troubleshooting, examples, wiki, prompt/skill docs).
- Plan references that leaked into shipped code, tests, comments, identifiers,
  commit messages, branch names, PR titles, or PR descriptions.

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
