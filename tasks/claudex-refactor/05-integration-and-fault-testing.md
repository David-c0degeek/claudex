# 05 — Integration And Fault Testing

## Goal

Build a deterministic, default-offline test foundation that proves provider
streaming, budgets, orchestration, migrations, process trees, Git worktrees, and
operator projections together. Live paid providers remain a narrowly capped
opt-in compatibility smoke, never the primary correctness signal.

## Integration analysis

- **Existing code found** — `tests/` contains fast `unittest` orchestration
  coverage but predominantly mocks `_run_agent`; project tests run through
  `python -m unittest discover -s tests -v`; Git/process/provider code resides in
  the modules identified by subjects 01–04; no established live-provider test
  harness or fixture executable was found during plan creation.
- **Behaviour to preserve** — default tests remain fast, deterministic, local,
  and free of provider credentials/spend; existing unit regressions keep passing;
  platform-specific tests skip with a precise capability reason.
- **Reuse / extend** — extend the current `unittest` suite and reuse the real
  public CLI/coordinator, subject-01 fixtures, subject-03 state transitions, and
  subject-04 process/Git lifecycle instead of manufacturing test-only workflows.
- **Do not duplicate** — one configurable fake-provider executable/harness feeds
  adapter, coordinator, fault, and golden tests. Shared fixtures do not reencode
  production parsers or coordinator decisions.
- **Integration point + why** — subprocess-level fixtures validate the real seam
  mocks currently bypass; temporary real Git repositories validate worktree
  behavior; golden output validates the same event projection terminal UX uses.
- **Vision fit** — converts the incident's expensive trial-and-error into cheap
  reproducible evidence and makes provider drift detectable before a paid run.
- **Risks** — fixtures diverging from real provider schemas, flaky timing tests,
  platform variance, overfitted snapshots, accidental credential use, and a live
  smoke exceeding budget when provider-native caps are absent.

## Boxes

- [x] **05.1** (agent) Create reusable fake Claude/Codex executables and versioned
      JSON/JSONL scenarios for partial text, tools, usage/cost, final results,
      interleaved stderr, malformed/unknown events, rate limits, non-zero exits,
      hangs, descendants, signals, and capability/version variations; make timing
      synchronization event-based rather than sleep-race dependent.
- [x] **05.2** (agent) Add black-box coordinator/CLI scenarios for the bounded
      plan→critique→revision→implementation→test→verify path, the exact false
      gate, accepted explicit human gates, context manifests, budget admission,
      pause/resume/restart/fresh-plan, and final summaries; assert provider call
      count and durable artifacts, not internal helper calls.
- [x] **05.3** (agent) Build real temporary Git repository/worktree scenarios for
      dirty starting state, tracked/untracked/deleted files, checkpoint/tree
      identity, test failure, merge readiness, conflicts, and recovery; prove
      tests and proposed integration refer to the same exact content.
- [x] **05.4** (agent) Add a fault-injection matrix for partial state writes,
      migration interruption, malformed streams, full/broken artifact storage,
      reader failure, lock contention, timeout, cancel, child survival, retry,
      rate-limit reset, and retention/redaction; assert legal terminal state,
      unique evidence, lock release, and documented resume action.
- [x] **05.5** (agent) Add Windows-specific coverage for process-tree cleanup,
      executable capability/version resolution, and subprocess path quoting,
      plus supported non-Windows fallbacks; configure CI or record the exact
      repeatable platform verification required by §7.
- [x] **05.6** (agent) Add a credential-explicit, default-skipped live-provider
      smoke that first runs `doctor`, requires both provider-native and
      coordinator spend/time caps, disables nested agents, uses a tiny fixture
      task, redacts artifacts, and aborts before invocation when any cap cannot be
      enforced; document safe operator invocation and record release evidence.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [x] Captain Hindsight review recorded
- [x] Verdict is `CLOSE`

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.

- 2026-07-15 · slice 1 · 05.1–05.6 · added one scenario-driven fake-provider
  executable for both protocols, versioned event/fault scenarios, real public-CLI
  acceptance runs, real Git/worktree/merge-conflict coverage, persistence/reader/
  lock faults, Windows quoting and descendant coverage, Windows+Ubuntu CI, and a
  credential-explicit live smoke that refuses before spend while Codex lacks a
  native monetary cap. README operator invocation and testing lessons updated. ·
  `python -m compileall -q claudex tests`; 109 tests pass on Windows (one default
  live skip); the explicitly enabled live preflight also skipped before invocation
  with `no provider-native spend cap for codex`; black-box acceptance observed 7
  provider calls, bounded manifests, one revision, commit/test/review/verify,
  completed lifecycle, exact verified tree, and final status. · checkpoint
  commit/push recorded after closing review.

### Captain Hindsight — closing review

1. **Keep:** The fake owns only provider protocol and scripted external actions;
   production parsers, state transitions, budgets, CLI, worktree, process runner,
   and artifacts all execute unchanged in black-box tests. Assertions target
   calls, lifecycle, hashes, artifacts, and output rather than helper calls.
2. **Fix before closing:** The initial reader-fault test incorrectly expected a
   zero child exit even though closing a broken stdout pipe is platform-specific;
   it now asserts prompt termination, drained stderr, and durable pipe-error
   evidence. The first conflict probe assumed a newer one-argument `merge-tree`
   form and was corrected against installed Git's two-branch contract. Most
   importantly, the first live-smoke draft would have invoked Codex with only an
   after-the-fact coordinator dollar total; it now treats the missing native cap
   as a pre-invocation safe refusal.
3. **Record:** Added the shared-fake and paid-smoke safety lessons. D008 makes
   missing native spend enforcement an explicit fail-closed release decision.
4. **Risk:** Windows execution is observed locally, while Ubuntu behavior is
   encoded in `.github/workflows/tests.yml` and its process-group fallbacks; the
   remote CI result becomes release evidence in Subject 06. No live provider
   tokens were spent because current Codex capability cannot meet the contract.
5. **Verdict:** CLOSE.
