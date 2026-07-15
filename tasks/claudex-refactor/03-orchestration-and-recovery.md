# 03 — Orchestration And Recovery

## Goal

Correct human-gate semantics, bound the planning/review loop and its context,
make lifecycle transitions explicit, and make resume/restart preserve the latest
validated checkpoint. The default protocol should reach implementation after a
small predictable number of calls or stop with a precise durable reason.

## Integration analysis

- **Existing code found** — `claudex/schemas.py::CritiqueResult` and
  `requires_human_decision`; `claudex/phases.py` phase transitions, review loops,
  rate-limit and test gates; `claudex/prompts.py` full-plan and critique context;
  `claudex/state.py` state/checkpoint fields; `claudex/cli.py::_replacement_state`
  and resolve/restart commands; `claudex/artifacts.py` plan/critique history.
- **Behaviour to preserve** — coordinator-owned transitions, typed schemas,
  immutable task/binding guidance, explicit resolve notes, bounded findings,
  isolated implementation, mechanical tests, and a fresh final verifier.
- **Reuse / extend** — strengthen the current phase machine and versioned state;
  assemble bounded evidence from canonical artifacts; evolve existing restart
  rather than adding a parallel recovery path.
- **Do not duplicate** — no second workflow engine, hidden chat history, or
  provider-owned canonical plan. Human resolution remains a coordinator command.
- **Integration point + why** — schemas validate provider intent; phases decide
  transitions; state stores the canonical checkpoint; prompts receive only the
  coordinator-built bounded packet.
- **Vision fit** — restores the deterministic state-machine foundation identified
  as worth preserving in `refactor.md` and removes the token-amplifying review
  behavior that made the incident unusable.
- **Risks** — accepting a truly blocked result, rejecting older provider output,
  context caps omitting decisive evidence, malformed plan deltas corrupting the
  canonical plan, migration ambiguity, or resume reusing an unsafe session.

## Boxes

- [x] **03.1** (agent) Make a human gate valid only when
      `requires_human_decision` is explicitly true and a concrete non-empty
      question is present; treat inconsistent combinations as bounded protocol
      failures. Add the exact incident fixture—explicit false, no question, and
      multiple major/blocking `decision` findings—and prove it proceeds.
- [x] **03.2** (agent) Replace ever-growing review history with a size-bounded,
      manifest-backed evidence packet containing task/binding guidance, canonical
      plan or diff, decision ledger, unresolved accepted findings, relevant repo
      facts, and selected verification. Use fresh plan-review sessions by default
      and test omission/request behavior without unrestricted re-browsing.
- [x] **03.3** (agent) Implement the default protocol of one plan draft, one
      critique, one revision, and at most one short conditional audit; make extra
      rounds explicit policy. Support validated section replacement/structured
      plan deltas so the coordinator retains the last canonical plan when a
      revision is malformed.
- [x] **03.4** (agent) Define and persist versioned lifecycle transitions for
      `RUNNING`, `PAUSED`, `PAUSED_BUDGET`, `RATE_LIMITED`, `CANCELLED`,
      `FAILED_RETRYABLE`, `FAILED_TERMINAL`, and `COMPLETED`, with timestamp,
      reason, phase, attempt, and resume instruction; reject illegal transitions
      through table-driven tests.
- [x] **03.5** (agent) Make `resume` continue the same durable run and make
      `restart` copy the full latest validated checkpoint—canonical plan,
      findings, decision ledger, checks, budgets, worktree/base identity, and
      safe session metadata. Add explicit `--fresh-plan`, atomic versioned
      migration, original-state backup, and hash-equivalence regression tests.
- [x] **03.6** (agent) Exercise the complete bounded planning/review/revision,
      explicit human resolution, malformed result, pause/resume, restart, and
      migration paths with deterministic agents; record call counts, context
      manifests, state histories, and observed-versus-expected behavior.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [x] Captain Hindsight review recorded
- [x] Verdict is `CLOSE`

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.

- 2026-07-15 · slice 1 · 03.1–03.3 · removed finding-based gate inference,
  made inconsistent flag/question pairs retryable protocol failures, reproduced
  the exact false-gate incident, made plan review/revision sessions fresh, and
  replaced critique globs/full-plan rewrites with immutable size-capped hashed
  evidence manifests plus hash-guarded section replacement · observed the
  normal default reach implementation in exactly four calls (draft, critique,
  revision, conditional audit), with both reviewer calls using empty resume
  IDs and the run directory as their bounded evidence root.
- 2026-07-15 · slice 2 · 03.4–03.6 · added the legal lifecycle transition
  table/history, same-ID resume, retryable/terminal failure distinction,
  atomic state migration with original backup, canonical checkpoint cloning,
  safe-session filtering, exact path remapping, checkpoint digest verification,
  and explicit `--fresh-plan` discard · deterministic lifecycle, evidence,
  false-gate, malformed delta, pause/resume, migration, restart command, and
  hash-equivalence scenarios pass with the full suite; README, CLI help, and
  packaged live-pair prompts were updated in the same checkpoint.

### Captain Hindsight — closing review

1. **Keep:** Gate intent now comes only from explicit true plus a concrete
   question; fresh reviewers consume one immutable manifest containing the
   canonical plan, decision/finding ledgers, and selected hashed files; plan
   deltas cannot apply to the wrong baseline. Lifecycle and phase remain
   separate deterministic concepts, and restart proves checkpoint equivalence
   before the original run is retired.
2. **Fix before closing:** The first implementation could replay an existing
   inconsistent critique forever, accepted absolute in-repo evidence paths,
   overwrote a rejected structured revision on retry, and hashed too small a
   restart-state subset. Before closure, protocol/inconclusive artifacts and
   malformed deltas are uniquely quarantined, requests require safe relative
   files, immutable evidence packets use expansion suffixes, and the digest now
   covers plan/fix/test/verify counters, ledgers, budgets, worktree identity,
   mailbox position, and safe sessions. Fresh-plan restart also falls back to
   INIT when no task snapshot exists.
3. **Record:** D005 records explicit gate semantics plus bounded fresh review;
   D006 records non-lossy restart/same-ID resume. Added manifest immutability,
   phase/lifecycle separation, and identity-neutral checkpoint hashing lessons
   for promotion into the architecture/recovery ADRs in 06.4.
4. **Risk:** Prompt/cwd boundaries constrain fresh reviewers, but provider
   permission hardening is completed in subject 04. `RATE_LIMITED` is defined,
   persisted, and legal-transition tested here; replacing the current sleep
   path with that state is also deliberately subject 04. Reusable black-box
   provider/worktree coverage remains subject 05.
5. **Verdict:** CLOSE.
