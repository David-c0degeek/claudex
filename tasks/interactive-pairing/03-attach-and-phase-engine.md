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
- **New Go packages** — `internal/engine` (the authoritative submit-driven phase-transition table + evaluator + convergence check), building on `internal/state` (01) and `internal/transport` (02). Registration/session records live in `internal/state`.
- **Requirements harvested (D014)** — phase order, convergence definition (`AGREE` + zero blocking/major), lead-response budgets, and "planning stays planning" finding categories are drawn from the README + the old tests as *vectors*. The old Python `phases.py:Orchestrator` and `agents.py` adapters are **reference-only** for behaviour; no logic is ported — the Go transition table is written fresh against the vectors.
- **Behaviour to preserve** — phase order and convergence; lead-response budgets; fresh-verifier intent (D010, explicit).
- **Do not duplicate** — one phase-transition table/evaluator (`internal/engine`), one convergence check; no second engine anywhere (D003 — greenfield means there is no Python engine to coexist with, but the Go CLI must not grow a second one).
- **Integration point + why** — `internal/engine` wires accepted submits (02) to transitions and turn issuance (01.2); it enforces the filesystem classification (01.8) at bootstrap.
- **Vision fit** — realizes the README loop for real; lead-by-initiation (D001), no rotation; D010 verifier freshness.
- **Risks** — ensuring only the turn-owning role advances a phase; deriving faithful transitions from vectors without a Python crutch.

## Boxes
- [ ] **03.1** (agent) Run/session bootstrap as a two-step attach sequence (a single call cannot register two terminals): the **first** `attach` enforces the filesystem classification (01.8 — refuse/label `known-unsupported`, apply `unknown` policy) then atomically creates the run (snapshots task + base, allocates the worktree, chooses the lead), **reserves both agent/role slots**, registers the caller, and returns `run_id` + `session_id` + the exact pair-join command. The **second** `attach` fills the reserved pair slot. PLAN_DRAFT is issued only once both slots are registered. Uses the 01.4 repo-level allocation lock. Test: a `known-unsupported` state dir is refused at bootstrap; first attach reserves both slots + returns join command; second attach fills the pair; PLAN_DRAFT not issued until both present.
- [ ] **03.2** (agent) `attach --agent claude|codex --role lead|pair` binding + reattach + replacement: bind a terminal to a role in durable state; reject a mid-run role switch. **Reattach must present its existing `session_id`** — an idempotent reattach returns the same session; an attach that supplies no prior identity for an already-filled slot is a conflict and must use explicit **same-role replacement with an expected generation**, minting a new session generation so a crashed/abandoned TUI can be superseded (no leases, but replacement is possible). Auth is out of scope (same-user FS) — state so. Test: reattach with session_id idempotent; identity-less attach on a filled slot conflicts; replacement with expected generation supersedes.
- [ ] **03.3** (agent) In-transition assignment issuance: the state transition that accepts a submit durably issues the next role's immutable `turn_id`/assignment **before releasing the mutation lock**; the non-owning role's `pull` reads a `waiting` projection and never mutates. Test: after a submit, exactly one role has an actionable assignment and no identity is minted on pull.
- [ ] **03.4** (agent) Create the authoritative submit-driven **phase transition table + evaluator** in `internal/engine`, written fresh and driven by the harvested phase-order/convergence vectors (D014) — not ported from `phases.py:Orchestrator` (reference-only). Each phase's accepted artifact advances per this table; wrong-phase or wrong-role submit rejected. Test: full PLAN→…→DONE dry run over fake artifacts driven by the new table + the harvested transition vectors.
- [ ] **03.5** (agent) Convergence evaluation: `AGREE` + zero blocking/major closes a review phase; any blocking/major routes to the lead's fix phase. Reuse `schemas.py` finding categories. Test: mixed findings route correctly.
- [ ] **03.6** (agent) Lead-response budgets as phase counters: plan revisions, checkpoint fixes/step, test fixes, verify fixes — counters only increase; the threshold transition routes an exhausted budget to a human gate (subject 05 owns cap *policy/UX*; this subject owns the counter + transition). Exhaustion reports a quality-budget stop, not a false disagreement. Test: exhaustion produces the correct stop transition.
- [ ] **03.7** (agent) Phase edit policy: codify that the lead may mutate the repo **only** during IMPLEMENT/FIX; lead planning turns and **all** pair turns are repo-read-only. BYO attach checks this at `submit` (reject a repo mutation submitted from a read-only phase); managed launch also applies provider read-only flags (subject 07). Test: a mutation from a read-only phase is rejected.
- [ ] **03.8** (agent) VERIFY blocks for a fresh session generation (D010, single locked behavior — no downgrade branch): the incumbent pair generation is invalid for VERIFY; the phase **blocks until a new same-role session generation attaches**. BYO reports `fresh-session-declared` (the session *generation* is enforced; model-context freshness is not). Managed reports `fresh-process` only when it actually launches a new process. Changing this to allow a downgrade requires a later explicit decision/gate. Test: VERIFY does not proceed on the incumbent generation; a new generation unblocks it; labels match the tier.
- [ ] **03.9** (agent) The Go CLI exposes **no legacy run-creation surface** at all (greenfield — the Python engine was deleted in 00.3, so there is nothing to disable): assert the command set has only the attach protocol (`attach`/`pull`/`submit`/`wait`/`status`/gates/operator) plus the read-only legacy **inspect/export** utility (01.7); there is no `run`/old-`pair` create path. Test: the CLI has no run-creation command; the legacy inspector is read-only and refuses execution/resume.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
