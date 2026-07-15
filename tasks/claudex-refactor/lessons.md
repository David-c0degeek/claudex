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
- 2026-07-15 — Cancellation is reliable only when the process owner creates a
  group/job before work begins and the operator writes a durable request without
  waiting on the coordinator lock. The same streamed primitive must own provider
  and mechanical-test descendants, partial logs, timeout, and terminal reason.
- 2026-07-15 — Git cleanliness and tested-content identity are different facts.
  Include untracked files in cleanliness and hash HEAD/tree/status/patch plus
  untracked bytes so a passing test cannot silently validate content that will
  not be integrated.
- 2026-07-15 — Executable discovery is a protocol negotiation, not a path
  lookup. Explicit binaries fail closed; automatic candidates are capability
  probed and ranked by semantic version, never by installation timestamp.
- 2026-07-15 — Redact at every persistence boundary, then prune only raw
  streams. Compact events/results/summaries, lifecycle state, decisions, and
  exact content identities must survive retention and pruning faults.
- 2026-07-15 — A reusable subprocess fake should model the provider boundary,
  not duplicate coordinator decisions. Versioned call scenarios, prompt/call
  logs, filesystem commit actions, and ready/release files let the same fixture
  exercise adapters, the public CLI, faults, and golden views deterministically.
- 2026-07-15 — A paid smoke is subordinate to enforceable safety. Capability
  preflight must happen before invocation and a missing provider-native spend
  cap is a successful safe refusal, not permission to rely on an after-the-fact
  coordinator total.
- 2026-07-16 — An observer is not read-only merely because it skips the run
  lock. If its shared state loader performs migrations, a watcher can still
  rewrite state. Inspection paths need an explicit non-persisting load mode;
  control paths remain responsible for durable migration.
- 2026-07-16 — Windows can deny an atomic state replace during a brief watcher
  read. Keep the replace atomic and retry the sharing violation for a small,
  bounded interval instead of making observers acquire the coordinator lock.
- 2026-07-16 — Recovery export is a separate security boundary. Re-redact
  exported text, exclude exact legacy rollback backups and raw streams, and
  reject symlinks so a support bundle cannot escape the run directory.
- 2026-07-16 — A focused state-loader regression must not accidentally
  instantiate provider discovery. Local Claude/Codex installations can hide
  that dependency; mock the unrelated constructor so clean CI proves the
  intended boundary instead of workstation tooling.
