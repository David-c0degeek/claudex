# Engineering lessons (permanent)

Durable, project-level lessons that outlive any single plan. Disposable
per-plan run-notes live in `tasks/<name>/lessons.md` and migrate here at the
plan's gate. Rephrased for the Go rebuild; the original Python-era phrasing is in
git history.

## Design / architecture
- The coordinator is code, not a model: phase order, caps, diffs, test results,
  and convergence are computed deterministically. Models argue; the state machine
  decides.
- A git ref update and a coordinator state CAS cannot commit atomically. Any
  operation spanning the two stores needs a prepared-transaction journal + startup
  reconciliation, idempotent at every crash cut — never an assumption of atomicity.
- Reviews read committed/immutable git objects identified by hash, never the live
  worktree. The mutable worktree is never a review input.
- Enforce only what you can observe. Interactive attach can enforce turn/fix
  counts, artifact bytes, and wall time; token/cost metering needs a real
  telemetry channel. Never label a capability "enforced" without the mechanism
  that backs it, and never accept an agent's self-report as enforcement.
- Keep orchestration phase separate from operator lifecycle: phase selects the
  next deterministic action; lifecycle records why execution stopped and the exact
  resume/retry/resolve/cancel/merge action.

## Correctness primitives
- OS-held advisory locks are released by the kernel on process exit — that death
  is the crash-stale reclaim signal, so never reclaim a lock by guessing from a
  PID. (POSIX `flock`, Windows `LockFileEx`.)
- Atomic rename gives process-crash atomicity (a reader sees old or new bytes,
  never torn); power-loss *durability* is a separate, stronger claim requiring
  explicit file+directory fsync (POSIX) and write-through semantics (Windows).
  State the guarantee you actually provide.
- Redact before every persistence and display boundary, over the exact canonical
  bytes; take digests over the redacted bytes so idempotency is stable.
- Verify a git tree by its full identity: HEAD, index, status, and untracked
  bytes. Cleanliness and tested-content identity are related but distinct.
- Normalize provider usage only at the terminal result boundary and charge each
  attempt idempotently; overlapping token categories differ per provider (some are
  subsets, some additive), and missing usage/cost must stay unknown, not estimated.

## Build / process
- Rebuild from evidence, not from code: harvest the retired suite's tests,
  schemas, and fixtures as executable requirements (test vectors) and rederive the
  implementation. This preserves hard-won failure-mode knowledge without inheriting
  the old bugs. Tag each harvested behaviour with a disposition so a rewrite does
  not resurrect a discarded engine.
- OS behaviour is not uniform across platforms even in one language. Put locks,
  atomic files, process-tree kill, and terminal handling behind tiny build-tagged
  packages and integration-test them on every first-class OS.
- Two AI agents can pair-program over a shared file mailbox: one leads, one
  reviews, and they converge by agreement (AGREE + zero blocking findings). The
  reviewer catches what the author cannot — treat REVISE findings as the point of
  the exercise, not an obstacle.
