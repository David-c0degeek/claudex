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

- [ ] **05.1** (agent) Create reusable fake Claude/Codex executables and versioned
      JSON/JSONL scenarios for partial text, tools, usage/cost, final results,
      interleaved stderr, malformed/unknown events, rate limits, non-zero exits,
      hangs, descendants, signals, and capability/version variations; make timing
      synchronization event-based rather than sleep-race dependent.
- [ ] **05.2** (agent) Add black-box coordinator/CLI scenarios for the bounded
      plan→critique→revision→implementation→test→verify path, the exact false
      gate, accepted explicit human gates, context manifests, budget admission,
      pause/resume/restart/fresh-plan, and final summaries; assert provider call
      count and durable artifacts, not internal helper calls.
- [ ] **05.3** (agent) Build real temporary Git repository/worktree scenarios for
      dirty starting state, tracked/untracked/deleted files, checkpoint/tree
      identity, test failure, merge readiness, conflicts, and recovery; prove
      tests and proposed integration refer to the same exact content.
- [ ] **05.4** (agent) Add a fault-injection matrix for partial state writes,
      migration interruption, malformed streams, full/broken artifact storage,
      reader failure, lock contention, timeout, cancel, child survival, retry,
      rate-limit reset, and retention/redaction; assert legal terminal state,
      unique evidence, lock release, and documented resume action.
- [ ] **05.5** (agent) Add Windows-specific coverage for process-tree cleanup,
      executable capability/version resolution, and subprocess path quoting,
      plus supported non-Windows fallbacks; configure CI or record the exact
      repeatable platform verification required by §7.
- [ ] **05.6** (agent) Add a credential-explicit, default-skipped live-provider
      smoke that first runs `doctor`, requires both provider-native and
      coordinator spend/time caps, disables nested agents, uses a tiny fixture
      task, redacts artifacts, and aborts before invocation when any cap cannot be
      enforced; document safe operator invocation and record release evidence.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.
