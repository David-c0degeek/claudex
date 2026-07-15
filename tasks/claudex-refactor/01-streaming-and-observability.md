# 01 — Streaming And Observability

## Goal

Replace buffered provider execution with a deadlock-safe streamed event
pipeline. Persist immutable raw and normalized attempt artifacts and expose a
stable live subscription/read model that terminal UX can consume later, while
preserving typed final results and existing coordinator ownership.

## Integration analysis

- **Existing code found** — `claudex/agents.py::AgentRunner._exec`,
  `AgentResult`, Claude/Codex command construction and result parsers;
  `claudex/artifacts.py` run artifact helpers; `claudex/phases.py::_run_agent`;
  `claudex/state.py` run state; `claudex/cli.py` status surface.
- **Behaviour to preserve** — provider commands remain non-interactive and
  schema-driven; final structured output still becomes a typed result;
  timeouts/non-zero exits remain explicit failures; read-only and write phases
  retain their permissions; existing callers need one coherent result.
- **Reuse / extend** — refactor the existing execution boundary and artifact
  writer; keep provider parsing at the adapter edge; introduce a focused event
  module only if 00.2 shows no clean existing home.
- **Do not duplicate** — do not build a second coordinator, mailbox, or provider
  launcher. Human-readable mailbox summaries derive from the journal instead of
  becoming a competing event store.
- **Integration point + why** — subprocess bytes enter through the existing
  agent runner, where concurrent pipe draining can emit provider-neutral events
  before `phases.py` receives the final result.
- **Vision fit** — directly supports `refactor.md`'s structured-provider and
  coordinator-owned state boundaries while making the current work visible.
- **Risks** — stdout/stderr ordering ambiguity, partial JSON framing, slow
  consumers, log disclosure, provider schema drift, Windows pipe behavior, and
  losing partial artifacts when parsing or process exit fails.

## Boxes

- [x] **01.1** (agent) Define a versioned `AgentEvent`/attempt contract with
      run/attempt/agent/phase identity, monotonic ordering, timing, event kind,
      safe display summary, raw-event reference, tool fields, usage fields, and
      explicit unknowns; add serialization/forward-compatibility tests.
- [x] **01.2** (agent) Refactor the current `communicate()` execution path into
      concurrent incremental stdout/stderr readers that cannot deadlock, emit
      promptly, preserve partial data on every exit path, and write unique
      immutable raw attempt artifacts; prove it with a timed fake subprocess.
- [x] **01.3** (agent) Implement and fixture-test the Codex JSONL adapter for
      text/reasoning, tool lifecycle, usage, completion, error, unknown, and
      malformed events while preserving the current final result/schema
      contract.
- [x] **01.4** (agent) Implement and fixture-test the Claude streamed-JSON
      adapter for partial messages, tool lifecycle, usage/cost, completion,
      rate-limit/error, unknown, and malformed events while preserving the
      current final result/schema contract.
- [x] **01.5** (agent) Add an append-only normalized event journal plus a bounded
      live reader/subscription and status projection; integrate phase summaries
      and mailbox output as consumers, with redaction hooks and deterministic
      replay tests rather than a second source of truth.
- [x] **01.6** (agent) Exercise a slow fake Claude and Codex run end to end:
      demonstrate that text/tool events are readable before completion, both
      pipes drain under load, sequence/replay is stable, partial artifacts
      survive failure, and final typed results match the old public contract;
      record observed-versus-expected evidence.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [x] Captain Hindsight review recorded
- [x] Verdict is `CLOSE`

## Progress log

> One line per slice: date · slice number · boxes touched · outcome · verification
> · checkpoint commit/push. Record documentation impact and learned lessons.

- 2026-07-15 · slice 1 · 01.1–01.4 · added versioned `AgentEvent`, ordered JSONL
  journal/replay, immutable unique attempt paths, concurrent stdin/stdout/stderr
  process handling, streamed Claude/Codex decoders, and typed terminal-result
  parsing; split process, limit, event, and provider-adapter responsibilities
  into focused modules after diff review exposed `agents.py` growth · verified
  adapter/parser/command tests and complete legacy suite · checkpoint is the
  subject-close commit containing this log; push required before 02.
- 2026-07-15 · slice 2 · 01.5–01.6 · wired a high-signal live coordinator
  consumer, derived attempt status projection, raw/normalized/final attempt
  artifacts, README event/mailbox corrections, and deterministic slow/noisy
  subprocess behavior tests · observed provider event callback before the fake
  process was released, drained >1 MB on each pipe without deadlock, retained
  partial/unique evidence, and passed 36 tests plus compileall/diff-check · docs:
  `README.md` updated in the same checkpoint.

### Captain Hindsight — closing review

1. **Keep:** Provider-specific JSON stays at the adapter edge, the coordinator
   consumes one versioned event/result contract, raw lines are written before
   decoding, and console/status are consumers of the append-only journal. The
   slow-process synchronization proves visibility before exit rather than merely
   asserting a final log file.
2. **Fix before closing:** The first implementation made `agents.py` a large
   mixed-responsibility file. Before closure, limit parsing, process lifecycle,
   provider-event decoding, and journal contracts were extracted into
   `limits.py`, `processes.py`, `provider_events.py`, and `events.py`. No open
   subject-01 fix remains.
3. **Record:** Added the concurrent-stdin lesson to `lessons.md`. D002 remains
   the durable provider-boundary decision for promotion in 06.4.
4. **Risk:** Real provider schemas can add or reorder events. Unknown/malformed
   lines are retained and non-fatal, final typed parsing is independent, and
   subject 05 adds broader versioned fixtures. Job-object-grade cancellation,
   redaction, and retention intentionally remain tracked by subject 04.
5. **Verdict:** CLOSE.
