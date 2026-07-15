# Architecture decisions

## Structured provider CLIs are the runtime boundary

Claude stream-json and Codex exec JSONL remain provider-specific at adapters.
Claudex has no provider SDK runtime dependency and consumes a small normalized
event/result contract.

## Offline integration evidence is authoritative

Correctness must not depend on credentials, spend, model nondeterminism, or
rate-limit availability. Scenario-driven subprocess fakes and temporary real Git
repositories are the release gate. Paid smoke tests are compatibility checks
only and fail closed when native spend enforcement is missing.

## Accounting uses terminal provider facts

Intermediate stream usage may be cumulative and is display-only. Each attempt's
terminal usage/cost is charged idempotently. Non-USD amounts are retained without
conversion; absent values remain unknown.

## Planning and human gates are bounded and explicit

Planning uses compact canonical sections, hash-guarded replacements, fresh
reviews, bounded evidence, one default revision, and at most one conditional
audit. Finding labels never infer a human gate.

## Resume preserves identity; restart preserves checkpoint

`resume` continues the same run. `restart` changes execution identity only after
checkpoint equivalence succeeds. Planning destruction requires `--fresh-plan`
and cannot silently discard implementation.

## The coordinator owns safety boundaries

Providers and tests share process-tree control. Rate limits return control.
Cancellation is durable and lock-independent. Exact Git identity includes
untracked bytes. Permissions are phase capabilities. Provider paths negotiate
capabilities. Redaction precedes persistence and retention prunes raw data only.

## Watchers are views

Normalized journals and durable state are the sole terminal read model. Watcher
windows do not host provider TUIs, scrape output, acquire run locks, or interpret
pane closure as cancellation.
