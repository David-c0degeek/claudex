# 03 — Attach And Phase Engine

## Goal
Bootstrap a run, register exactly one Claude and one Codex terminal to roles,
and drive the pairing state machine. `attach --agent claude|codex` binds a
session to lead or pair (no mid-run role switch; idempotent reattach; explicit
same-role session replacement so a crashed TUI never bricks the run). The phase
graph (PLAN_DRAFT → PLAN_CRITIQUE → PLAN_REVISE → IMPLEMENT_STEP → CHECKPOINT →
FIX → TESTS → VERIFY → DONE) advances on accepted `submit`s, each transition
durably issues the *next* assignment before releasing the lock, and convergence
is `AGREE` + zero blocking/major, evaluated by the coordinator.

## Integration analysis
> **Superseded by D013/D014 (Go greenfield):** the Python module names below are *reference-only requirements*, not reuse targets — implement fresh in Go (`internal/engine`, `internal/state`). The single authoritative phase-transition table/evaluator is new Go (03.4), informed by — not ported from — the old `phases.py:Orchestrator`. "Reuse" means the harvested test vectors from 00.2. Re-fill in Go terms before ticking any box.
> Fill/confirm against the 00.2 harvest before ticking any box.
- **Existing code found** — `claudex/state.py` holds `Phase` and `RunState.advance` (the real phase state, **not** `phases.py`). `claudex/phases.py` is a large **subprocess-coupled `Orchestrator`** — a candidate to adapt or retire, not a reusable transition table. `claudex/schemas.py` holds convergence helpers + finding/verdict schemas. `claudex/agents.py` holds provider command/result adapters (`ClaudeAgent`/`CodexAgent`), not reusable role concepts. README convergence semantics + lead-response budgets.
- **Behaviour to preserve** — phase order and convergence definition; lead-response budgets; "planning stays planning" finding categories; the fresh-verifier intent (now via D010, made explicit).
- **Reuse / extend** — extend `state.py`'s `Phase`/`advance` for submit-driven transitions + assignment issuance; reuse `schemas.py` convergence helpers; harvest any reusable step logic out of the `phases.py` `Orchestrator` while retiring its subprocess coupling.
- **Do not duplicate** — no second phase-state or convergence evaluator; extend `state.py`.
- **Integration point + why** — `state.py` already owns `Phase`/`advance` + durable persistence; wire transitions to accepted submits (02) and turn issuance (01.2).
- **Vision fit** — realizes the README loop for real; lead-by-initiation (D001), no rotation; D010 verifier freshness.
- **Risks** — extracting reusable logic from the subprocess-coupled `Orchestrator`; ensuring only the turn-owning role advances a phase; keeping two public engines alive would violate D003.

## Boxes
- [ ] **03.1** (agent) Run/session bootstrap as a two-step attach sequence (a single call cannot register two terminals): the **first** `attach` atomically creates the run (snapshots task + base, allocates the worktree, chooses the lead), **reserves both agent/role slots**, registers the caller, and returns `run_id` + `session_id` + the exact pair-join command. The **second** `attach` fills the reserved pair slot. PLAN_DRAFT is issued only once both slots are registered. Uses the 01.4 repo-level allocation lock. Test: first attach reserves both slots + returns join command; second attach fills the pair; PLAN_DRAFT not issued until both present.
- [ ] **03.2** (agent) `attach --agent claude|codex --role lead|pair` binding + reattach + replacement: bind a terminal to a role in durable state; reject a mid-run role switch. **Reattach must present its existing `session_id`** — an idempotent reattach returns the same session; an attach that supplies no prior identity for an already-filled slot is a conflict and must use explicit **same-role replacement with an expected generation**, minting a new session generation so a crashed/abandoned TUI can be superseded (no leases, but replacement is possible). Auth is out of scope (same-user FS) — state so. Test: reattach with session_id idempotent; identity-less attach on a filled slot conflicts; replacement with expected generation supersedes.
- [ ] **03.3** (agent) In-transition assignment issuance: the state transition that accepts a submit durably issues the next role's immutable `turn_id`/assignment **before releasing the mutation lock**; the non-owning role's `pull` reads a `waiting` projection and never mutates. Test: after a submit, exactly one role has an actionable assignment and no identity is minted on pull.
- [ ] **03.4** (agent) Create the authoritative submit-driven **phase transition table + evaluator** (location chosen per the 00.2 map — `state.py` today has only `Phase` + an unvalidated `RunState.advance`, not a transition table): each phase's accepted artifact advances per this table, harvesting the relevant logic out of `phases.py:Orchestrator` while dropping its subprocess coupling; wrong-phase or wrong-role submit rejected. Test: full PLAN→…→DONE dry run over fake artifacts driven by the new table.
- [ ] **03.5** (agent) Convergence evaluation: `AGREE` + zero blocking/major closes a review phase; any blocking/major routes to the lead's fix phase. Reuse `schemas.py` finding categories. Test: mixed findings route correctly.
- [ ] **03.6** (agent) Lead-response budgets as phase counters: plan revisions, checkpoint fixes/step, test fixes, verify fixes — counters only increase; the threshold transition routes an exhausted budget to a human gate (subject 05 owns cap *policy/UX*; this subject owns the counter + transition). Exhaustion reports a quality-budget stop, not a false disagreement. Test: exhaustion produces the correct stop transition.
- [ ] **03.7** (agent) Phase edit policy: codify that the lead may mutate the repo **only** during IMPLEMENT/FIX; lead planning turns and **all** pair turns are repo-read-only. BYO attach checks this at `submit` (reject a repo mutation submitted from a read-only phase); managed launch also applies provider read-only flags (subject 07). Test: a mutation from a read-only phase is rejected.
- [ ] **03.8** (agent) VERIFY blocks for a fresh session generation (D010, single locked behavior — no downgrade branch): the incumbent pair generation is invalid for VERIFY; the phase **blocks until a new same-role session generation attaches**. BYO reports `fresh-session-declared` (the session *generation* is enforced; model-context freshness is not). Managed reports `fresh-process` only when it actually launches a new process. Changing this to allow a downgrade requires a later explicit decision/gate. Test: VERIFY does not proceed on the incumbent generation; a new generation unblocks it; labels match the tier.
- [ ] **03.9** (agent) Once this subject lands, the old public engine cannot create **any new** run: `cli.py cmd_run` and the old `pair` dispatch globally refuse to start a new headless/old-live run (not merely when an attach run is active); pre-pivot runs are inspect/export-only per D011. Residual adapter/test **deletion** happens in 07. Test: `claudex run`/old `pair` refuse to create a new run globally; legacy runs remain inspectable/exportable.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
