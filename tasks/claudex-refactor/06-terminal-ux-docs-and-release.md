# 06 — Terminal UX, Documentation, And Release

## Goal

Ship the normalized event/status model through clear operator commands and
optional Windows Terminal watcher panes, document the bounded protocol and
safety/cost contracts, complete migrations and compatibility cleanup, and prove
the full acceptance flow before versioned release.

## Integration analysis

- **Existing code found** — `claudex/cli.py` command parser, run/status/restart/
  resolve/doctor handlers and executable discovery; `claudex/artifacts.py` run
  paths/mailbox; `README.md`, `pyproject.toml`, `claudex/__init__.py`, and prompt/
  skill templates provide user, package, and agent-facing documentation/version
  surfaces. Subject 01 supplies the journal/read model; subject 02 supplies
  metrics; subjects 03–04 supply states/actions; subject 05 supplies goldens.
- **Behaviour to preserve** — headless CLI operation, scriptable exit codes,
  current core command names where semantics remain safe, module/console entry
  points, and readable durable artifacts.
- **Reuse / extend** — add subcommands/options to the current CLI and render the
  single normalized journal/status projection; use Windows Terminal only as a
  launcher for watcher commands.
- **Do not duplicate** — watcher panes do not host or scrape interactive provider
  TUIs, own orchestration, or maintain separate usage/state. Closing a pane is
  not cancellation.
- **Integration point + why** — the CLI is the existing operator contract and
  artifacts are its durable data source; terminal launch is an optional view
  adapter above that contract.
- **Vision fit** — delivers the user's requested visible Claude/Codex terminal
  experience while retaining reliable structured coordination and headless use.
- **Risks** — terminal availability/profile differences, noisy partial text,
  ambiguous close/cancel semantics, stale docs/config examples, migration
  surprises, accessibility/color issues, and compatibility code lingering after
  cutover.

## Boxes

- [x] **06.1** (agent) Implement
      `claudex watch [RUN_ID] --agent claude|codex|all` over the normalized journal with replay-then-tail,
      follow/no-follow, readable/raw modes, clear event/state labels, color-safe
      fallback, redaction, stable scriptable exits, and golden tests.
- [x] **06.2** (agent) Upgrade `claudex status [RUN_ID]` to report lifecycle,
      active phase/attempt, model/effort, elapsed time, rounds, tool activity,
      reported usage/cost and unknowns, remaining budgets, rate-limit reset,
      failure/cancel reason, and exact next action from durable state; cover every
      terminal state with goldens.
- [x] **06.3** (agent) Implement `run --open-terminals` as an optional Windows
      Terminal launcher for separate Claude and Codex watcher panes/windows;
      quote paths safely, avoid visible helper consoles, print equivalent watcher
      commands on unsupported/headless systems, and prove panes are views whose
      closure neither cancels nor mutates the run.
- [x] **06.4** (agent) Update README, CLI/config reference, usage examples,
      troubleshooting, provider capability guidance, prompt/skill docs, and
      architecture/decision records to explain the bounded protocol, event
      artifacts, budgets/unknown cost, permissions, worktree-not-sandbox warning,
      watcher UX, pause/rate-limit/resume/restart/fresh-plan/cancel semantics, and
      retention/redaction; promote every durable §4/refactor decision.
- [x] **06.5** (agent) Finalize versioned config/state migration and compatibility
      messages, update package/version and release notes according to repo
      convention, prove rollback/export on representative old states, and avoid
      silently reinterpreting unsafe legacy runs.
- [x] **06.6** (agent) Run a complete slow fake-provider acceptance scenario from
      task snapshot through bounded planning, live dual watchers, implementation,
      test, verification, and completion; record elapsed/calls/usage/status,
      observed-versus-expected terminal output, restart and cancel variants, and
      the exact verified Git tree.
- [x] **06.7** (agent) Remove superseded buffered execution, lossy restart,
      implicit decision gating, foreground-wait, duplicate artifact, and temporary
      compatibility paths once coverage proves cutover; run full §2/§7 gates,
      perform/triage cleanup-audit, reconcile lessons/manual actions, and prepare
      the evidence handoff for human §8 acceptance.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [x] Captain Hindsight review recorded
- [x] Verdict is `CLOSE`

### 1. Keep

- Keep the single normalized journal/durable-state read model, with terminal
  windows as optional view processes and the coordinator as the sole control
  plane. Replay/tail visibility, final state records, raw JSONL, redaction, and
  exit codes are observable contracts rather than terminal implementation
  details.
- Keep status projection, explicit next actions, frozen terminal elapsed time,
  schema-versioned migration with rollback backup, and compact re-redacted
  export. The README, templates, architecture, decisions, and changelog describe
  the same shipped semantics.
- Keep the slow fake-provider and real-worktree evidence. It observed text while
  the provider was still blocked, then both Claude/Codex views, completed in
  4.886s with 7 calls, 77 input tokens, 49 output tokens, a completed status,
  and verified commit equal to the worktree HEAD. Separate black-box variants
  cover restart, rate return/resume, budget stop, and descendant cancellation.

### 2. Fix before closing

- Fixed journal tailing that initially rescanned every historical event on each
  poll; watchers now keep a cursor per immutable attempt journal.
- Fixed observer/control separation: watch and status apply migrations only in
  memory, while active control loads persist the migration and original backup.
  A misplaced control-path flag and its CI test's accidental dependency on
  locally installed providers were both caught and regression-covered.
- Fixed Windows atomic-replace sharing failures under concurrent watchers with a
  bounded retry; fixed completed elapsed time growing forever; added a final
  watcher lifecycle record and rate-limit exit 75.
- Fixed recovery export inclusion of exact legacy backups and symlinks, and
  re-redact all exported text. Removed the now-unused full-journal replay helper.
- Cleanup audit triage: `abort` remains only in the release compatibility note;
  `guidance_notes` remains only as an old-state migration input and regression;
  bounded `capture_output` calls are capability/Git/OS probes or test helpers,
  not provider/mechanical execution. No fix-now finding remains.

### 3. Record

- Promoted the view/control, migration, export, budget, process, provider, Git,
  orchestration, and offline-test decisions into `docs/decisions.md` and
  `docs/architecture.md`.
- Reconciled the temporary learning log into `tasks/lessons.md`, including the
  observer-side migration and Windows file-sharing lessons found during this
  subject.

### 4. Risk

- The paid live-provider smoke was not invoked: current Codex reports
  coordinator-only budget enforcement, so the required native-plus-coordinator
  spend envelope correctly refuses before spend. Provider schema drift remains
  a compatibility risk guarded by doctor/preflight, raw evidence, and adapters.
- Windows Terminal launch behavior is command/quoting tested without opening GUI
  windows in automation. Missing/headless terminals degrade to printed watcher
  commands; user acceptance can still assess local profile aesthetics.

### 5. Verdict

`CLOSE`. Final evidence: `compileall`, `git diff --check`, version 0.5.0, wheel
build, doctor, 127 offline tests in 64.716s (one intentional live-smoke skip),
12 explicit integration tests, and CI run 29459011913 across Windows/Ubuntu and
Python 3.10/3.13 all passed.

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.

- 2026-07-16 · 1 · 06.1–06.7 · Implemented replay/tail watchers, rich status,
  optional dual Windows Terminal views, migration/export/release surfaces,
  documentation/ADRs, and slow dual-watcher acceptance. Hindsight caught and
  fixed O(N²) journal replay, observer-triggered state migration, Windows atomic
  replace sharing failures, terminal elapsed-time drift, missing watcher state/
  rate exit, and unsafe export inclusion. Full Windows gate: compileall plus 125
  tests in 67.963s, OK with one default live-smoke skip; diff-check/version
  passed. Documentation: README, prompt/skill templates, architecture,
  decisions, changelog. Lessons recorded. Implementation checkpoint awaits
  pushed CI before boxes close.
- 2026-07-16 · 2 · 06.1–06.7 · Captain Hindsight and cleanup audit fixed the
  observer/control migration boundary, isolated its CI regression, removed the
  superseded replay helper, and expanded actual status/slow-acceptance evidence
  across every lifecycle. Code checkpoints `517142f`, `9a3a2c2`, `580a110`, and
  `17d240a` were pushed. Final local gate: 127 tests in 64.716s, OK (one safe
  live skip); final CI 29459011913: all four Windows/Ubuntu Python 3.10/3.13 jobs
  passed. Docs/ADRs/lessons reconciled; no manual action; closure checkpoint is
  this pushed plan-state commit.
