# Engineering lessons

- Streamed subprocess safety requires concurrent stdin writing plus stdout and
  stderr draining. Persist raw lines before decoding so a malformed provider
  event cannot erase evidence or deadlock a busy process.
- Normalize usage only at the terminal provider-result boundary and charge an
  attempt idempotently. Provider token categories overlap differently; missing
  usage or cost must stay unknown rather than becoming a local estimate.
- Keep orchestration phase separate from operator lifecycle. Phase selects the
  next deterministic action; lifecycle records why execution stopped and the
  exact resume, retry, resolve, cancel, or merge action.
- Bound fresh model context with immutable, hashed evidence manifests. A short
  prompt alone does not prevent accidental context growth or stale evidence.
- Resume should preserve execution identity; restart should verify a complete
  canonical checkpoint before replacing identity. Destructive reset deserves a
  separate explicit command-line option.
- Cancellation works only when the coordinator owns a process group or job
  before execution starts. Provider calls and mechanical tests should share the
  same streamed timeout/cancel primitive and immutable attempt evidence.
- A verified Git tree must include untracked bytes as well as HEAD, index,
  status, and patch identity. Cleanliness and tested-content identity are
  related but distinct invariants.
- Executable discovery is protocol negotiation. Probe required capabilities,
  fail closed for explicit incompatible paths, and rank automatic candidates
  by semantic version rather than timestamps.
- Redact before every persistence/display boundary and prune only raw streams.
  Recovery export must re-redact legacy text, exclude exact rollback backups,
  and reject symlinks.
- Observer commands need explicit non-persisting state/config loads. On Windows,
  keep writer replacement atomic and use a short bounded retry for file-sharing
  collisions instead of making watchers acquire the coordinator lock.
- Scenario-driven fake executables are the authoritative integration boundary:
  they make live streaming, rate limits, faults, budgets, restart, cancellation,
  and real worktree behavior reproducible without credentials or spend.
- Focused migration/state tests must isolate unrelated provider discovery.
  Workstation-installed CLIs can hide an accidental dependency that clean CI
  correctly rejects.
- A paid compatibility smoke is subordinate to enforceable native caps. Missing
  native spend enforcement is a safe preflight refusal, not permission to call
  a provider and inspect cost afterward.
