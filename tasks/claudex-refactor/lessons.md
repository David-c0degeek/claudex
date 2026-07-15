- 2026-07-15 — A streaming subprocess can still deadlock if the coordinator
  writes a large stdin prompt synchronously before it starts draining stdout and
  stderr. Start both readers and a stdin writer before waiting; persist each raw
  line before decoding so malformed provider data cannot erase evidence.
- 2026-07-15 — Provider token categories are not uniformly disjoint: Codex
  cached input and reasoning details are subsets of its input/output totals,
  while Anthropic cache-read/cache-creation fields are separate categories.
  Normalize into mutually exclusive buckets at the terminal-result boundary,
  retain raw usage, derive totals, and make attempt charging idempotent by ID.
- 2026-07-15 — Economic CLI overrides must be frozen into run state at start.
  Reading project config again on resume can silently change model, effort, or
  spend limits; later capacity changes need a named, durable override instead.
- 2026-07-15 — Bounded review context must be an immutable manifest, not just a
  shorter prompt. Hash every selected file, reject traversal/absolute requests,
  suffix bounded evidence expansions, and quarantine inconsistent structured
  artifacts so artifact-presence recovery cannot replay poison forever.
- 2026-07-15 — Orchestration phase and macro lifecycle solve different
  problems. Persist both: phase selects deterministic work, while lifecycle
  governs operator actions such as pause, retry, resume, cancel, and completion
  through a legal transition table with an explicit resume instruction.
- 2026-07-15 — A restart equivalence digest must normalize execution-identity
  paths while covering every canonical counter, ledger, budget, worktree/base
  field, and safe session. Verify the copy before retiring the predecessor;
  make destructive planning reset a separate explicit option.
