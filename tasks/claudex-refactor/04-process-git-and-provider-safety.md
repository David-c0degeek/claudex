# 04 — Process, Git, And Provider Safety

## Goal

Make abnormal execution safe and recoverable: cancellation and timeout terminate
provider descendants, rate limits return control, retries retain evidence, Git
gates account for the exact tested tree, provider permissions are capability-
based, and executable/artifact handling fails closed with useful diagnostics.

## Integration analysis

- **Existing code found** — subprocess lifecycle in
  `claudex/agents.py::AgentRunner._exec`; rate-limit retry/sleep behavior and test
  subprocesses in `claudex/phases.py`; worktree/status/checkpoint logic in
  `claudex/gitops.py`; provider mode/allowed-tool command construction in
  `claudex/agents.py`; artifact paths in `claudex/artifacts.py`; executable
  discovery and diagnostics in `claudex/cli.py`; persisted lock/state metadata.
- **Behaviour to preserve** — isolated implementation worktree, read-only review,
  configured mechanical test command, explicit timeout/failure reporting,
  existing supported provider discovery where it passes new capabilities, and
  resumable retry policy.
- **Reuse / extend** — use subject-01 attempt lifecycle and subject-03 state
  transitions; harden existing Git/provider/doctor seams rather than wrapping
  them in separate safety coordinators.
- **Do not duplicate** — no second process registry, worktree implementation,
  executable resolver, or artifact-retention store.
- **Integration point + why** — the subprocess runner owns process groups and
  pipes; phases translate terminal reasons into state; Git operations own tree
  identity; provider builders own permissions; doctor owns preflight capability.
- **Vision fit** — supplies the safety and integrity boundaries required before
  Claudex can wait, retry, mutate a worktree, or advertise autonomous operation.
- **Risks** — platform-specific process APIs, PID reuse, destructive cleanup,
  worktree edge cases, overly broad Claude shell access, secrets in raw logs,
  semantic-version parsing, and breaking headless installs with terminal checks.

## Boxes

- [ ] **04.1** (agent) Start every provider/test command in a controllable
      process group or Windows job-object equivalent; implement idempotent
      `claudex cancel` and timeout escalation that terminate descendants, retain
      partial events, persist the terminal reason, and release locks safely;
      prove it with a fake executable that spawns a long-lived child.
- [ ] **04.2** (agent) Convert provider rate limits into unique immutable attempt
      records and durable `RATE_LIMITED` state with reset metadata and resume
      instructions, then return control promptly by default. Keep autonomous
      waiting opt-in, cancellable, lock-safe, and tested without real sleeping.
- [ ] **04.3** (agent) Remove untracked-file blindness from every cleanliness,
      convergence, checkpoint, test, and integration gate; record the exact
      worktree/tree/patch tested and prove through real temporary Git repositories
      that the proposed integration includes exactly that content.
- [ ] **04.4** (agent) Replace broad provider write permissions with a verified
      capability policy per phase, narrowly restrict Claude shell/tool access,
      preserve Codex sandbox boundaries, disclose that a worktree is not an OS
      sandbox, and add command-construction/policy-denial tests for both adapters.
- [ ] **04.5** (agent) Route mechanical test execution through the shared streamed
      lifecycle so progress, timeout, cancel, descendant cleanup, partial logs,
      and durable failure state behave like provider attempts; demonstrate a
      hung and a noisy test command end to end.
- [ ] **04.6** (agent) Resolve provider executables by explicit configuration
      followed by deterministic semantic version/capability selection—not file
      mtime—and extend `claudex doctor` to report path, version, stream/schema,
      budget, sandbox, session, and nested-agent compatibility with actionable
      failure messages.
- [ ] **04.7** (agent) Implement redaction before display/persistence plus
      configurable age/byte retention that prunes raw attempt data without
      deleting compact summaries, decisions, or recovery state; fault-test
      malformed logs, disk/write failures, retries, pruning, and redaction, and
      record behavior evidence.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.

