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

- [ ] **03.1** (agent) Make a human gate valid only when
      `requires_human_decision` is explicitly true and a concrete non-empty
      question is present; treat inconsistent combinations as bounded protocol
      failures. Add the exact incident fixture—explicit false, no question, and
      multiple major/blocking `decision` findings—and prove it proceeds.
- [ ] **03.2** (agent) Replace ever-growing review history with a size-bounded,
      manifest-backed evidence packet containing task/binding guidance, canonical
      plan or diff, decision ledger, unresolved accepted findings, relevant repo
      facts, and selected verification. Use fresh plan-review sessions by default
      and test omission/request behavior without unrestricted re-browsing.
- [ ] **03.3** (agent) Implement the default protocol of one plan draft, one
      critique, one revision, and at most one short conditional audit; make extra
      rounds explicit policy. Support validated section replacement/structured
      plan deltas so the coordinator retains the last canonical plan when a
      revision is malformed.
- [ ] **03.4** (agent) Define and persist versioned lifecycle transitions for
      `RUNNING`, `PAUSED`, `PAUSED_BUDGET`, `RATE_LIMITED`, `CANCELLED`,
      `FAILED_RETRYABLE`, `FAILED_TERMINAL`, and `COMPLETED`, with timestamp,
      reason, phase, attempt, and resume instruction; reject illegal transitions
      through table-driven tests.
- [ ] **03.5** (agent) Make `resume` continue the same durable run and make
      `restart` copy the full latest validated checkpoint—canonical plan,
      findings, decision ledger, checks, budgets, worktree/base identity, and
      safe session metadata. Add explicit `--fresh-plan`, atomic versioned
      migration, original-state backup, and hash-equivalence regression tests.
- [ ] **03.6** (agent) Exercise the complete bounded planning/review/revision,
      explicit human resolution, malformed result, pause/resume, restart, and
      migration paths with deterministic agents; record call counts, context
      manifests, state histories, and observed-versus-expected behavior.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.
