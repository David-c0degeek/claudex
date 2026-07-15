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

- [ ] **06.1** (agent) Implement
      `claudex watch [RUN_ID] --agent claude|codex|all` over the normalized journal with replay-then-tail,
      follow/no-follow, readable/raw modes, clear event/state labels, color-safe
      fallback, redaction, stable scriptable exits, and golden tests.
- [ ] **06.2** (agent) Upgrade `claudex status [RUN_ID]` to report lifecycle,
      active phase/attempt, model/effort, elapsed time, rounds, tool activity,
      reported usage/cost and unknowns, remaining budgets, rate-limit reset,
      failure/cancel reason, and exact next action from durable state; cover every
      terminal state with goldens.
- [ ] **06.3** (agent) Implement `run --open-terminals` as an optional Windows
      Terminal launcher for separate Claude and Codex watcher panes/windows;
      quote paths safely, avoid visible helper consoles, print equivalent watcher
      commands on unsupported/headless systems, and prove panes are views whose
      closure neither cancels nor mutates the run.
- [ ] **06.4** (agent) Update README, CLI/config reference, usage examples,
      troubleshooting, provider capability guidance, prompt/skill docs, and
      architecture/decision records to explain the bounded protocol, event
      artifacts, budgets/unknown cost, permissions, worktree-not-sandbox warning,
      watcher UX, pause/rate-limit/resume/restart/fresh-plan/cancel semantics, and
      retention/redaction; promote every durable §4/refactor decision.
- [ ] **06.5** (agent) Finalize versioned config/state migration and compatibility
      messages, update package/version and release notes according to repo
      convention, prove rollback/export on representative old states, and avoid
      silently reinterpreting unsafe legacy runs.
- [ ] **06.6** (agent) Run a complete slow fake-provider acceptance scenario from
      task snapshot through bounded planning, live dual watchers, implementation,
      test, verification, and completion; record elapsed/calls/usage/status,
      observed-versus-expected terminal output, restart and cancel variants, and
      the exact verified Git tree.
- [ ] **06.7** (agent) Remove superseded buffered execution, lossy restart,
      implicit decision gating, foreground-wait, duplicate artifact, and temporary
      compatibility paths once coverage proves cutover; run full §2/§7 gates,
      perform/triage cleanup-audit, reconcile lessons/manual actions, and prepare
      the evidence handoff for human §8 acceptance.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.
