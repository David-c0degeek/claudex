# Claudex architecture

Claudex is a deterministic coordinator around the installed Claude Code and
Codex CLIs. Structured non-interactive provider execution is the control
boundary; terminal windows are views over durable events, not interactive
automation that Claudex scrapes.

## Control flow

```text
task snapshot
  -> bounded fresh plan / critique / optional revision + audit
  -> one implementation commit at a time
  -> exact checkpoint diff review
  -> streamed mechanical test on an unchanged tree
  -> fresh verification
  -> completed branch + exact merge instruction
```

The coordinator owns phases, macro lifecycle, response budgets, provider
admission, worktree identity, and artifacts. Providers return typed results;
they do not choose phase transitions. A human gate exists only for explicit
`requires_human_decision: true` plus a concrete question.

Before minting a new run identity, Claudex performs a read-only repository
preflight: substantive task, Git repository, ignored and untracked runtime
paths, a clean exact base, and non-billable provider capability discovery. A
failed preflight cannot create state, move the current pointer, or launch
terminal views.

## Process and event boundary

Every provider or mechanical-test call has a unique attempt directory. The
process runner starts a Unix process group or named Windows job, drains stdout
and stderr concurrently, persists redacted raw lines, normalizes provider events
into append-only JSONL, and retains a compact result and summary. Timeout and
`cancel` terminate descendants and preserve partial evidence.

`watch` replays and tails the normalized journal. `status` derives operator
metrics and the next action from durable state. Windows Terminal launchers only
start watcher commands, so pane closure has no control-plane effect.

## Cost, context, and recovery

Invocation policy is frozen at run start. Admission checks calls, reported
tokens/cost, observable tools, and wall time before every provider call. Terminal
provider results are charged once; unavailable usage and cost remain visibly
unknown. Rate limits persist reset metadata and return exit 75 by default.

Planning reviewers are fresh and receive immutable size-capped evidence
manifests. Implementation/review sessions may resume only within the unchanged
worktree lineage. `resume` keeps run identity; `restart` copies and verifies a
hash-equivalent checkpoint before retiring the predecessor. `--fresh-plan` is a
separate explicit reset and is rejected after an implementation worktree exists.

## Integrity and security boundaries

Git status includes tracked and untracked files. Tests, checkpoint reviews, and
verification record HEAD, tree, status, patch hashes, untracked content hashes,
and a combined exact identity. A passing test that changes the tree fails.

Claude receives a narrow tool set per phase. Codex receives read-only or
workspace-write sandbox policy with network/browser/app features disabled.
Nested provider agents are off by default. Executables are capability-probed and
selected by compatible semantic version. Explicit incompatible paths fail.

A Git worktree is not an OS security sandbox. Repository code and configured
test commands must still be trusted. Secrets are redacted before display and
persistence. Age/byte retention removes only raw streams; state, decisions,
events, results, summaries, and recovery exports remain.

## Testing boundary

The authoritative suite is offline. One versioned scenario-driven fake speaks
both provider protocols and drives the real adapters, CLI, process runner,
worktree, budgets, state, and artifacts. Temporary real Git repositories cover
dirty trees, fast-forward identity, and conflicts. Live compatibility checks are
explicit and refuse before invocation when native spend enforcement is absent.
