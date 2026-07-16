# 01 — Durable State Core

## Goal
The processless coordinator's foundation: a durable run-state store with atomic
compare-and-swap transitions keyed on `(run_id, state_revision, turn_id)`, a
per-mutation lock that `wait` never holds, unique immutable `turn_id`/`gate_id`
issuance, a ledger that is a projection of accepted artifacts, and a
prepared-transaction journal so git+state operations survive a crash between the
git ref move and the state CAS.

## Integration analysis
> Fill/confirm against the 00.2 prior-art map before ticking any box.
- **Existing code found** — `claudex/state.py` (run state, likely schema-versioned + `.bak` migration per README), `claudex/schemas.py` (typed artifact schemas), `claudex/lifecycle.py` (macro lifecycle: running/paused/rate_limited/…), run-dir lockfile logic (README: "run-dir lockfile around every state-mutating command"), `claudex/recovery.py`.
- **Behaviour to preserve** — schema-versioned state with `.bak` retention + fail-closed on unknown schema; lifecycle records phase/attempt/reason/next-action; lockfile serializes cross-process mutation.
- **Reuse / extend** — extend `state.py` with `state_revision` + CAS write; reuse `schemas.py` validation; reuse lockfile primitive. Confirm exact symbols in 00.2.
- **Do not duplicate** — do not add a second state store or lock; extend the existing ones.
- **Integration point + why** — CAS + revision belong in `state.py` because it already owns durable persistence and migration; the journal is a new sibling module (`transaction.py`?) because it spans state + git.
- **Vision fit** — realizes D002/D004/D006; supports "coordinator is code, not a model."
- **Risks** — Windows file-locking + atomic rename edge cases; torn writes; journal/reconciliation complexity.

> The prepared-transaction machine here is **generic** — tested against a fake
> participant. Subject 04 supplies the real Git participant and the crash-cut
> tests. Keep the seam; do not fold Git specifics into this subject.

## Boxes
- [ ] **01.1** (agent) Add `state_revision` to run state and an atomic CAS write helper: a mutation supplies the expected revision, and the write commits only if it still matches, else returns a conflict with current status. Temp→fsync→rename persistence. Test: concurrent writers, only one wins.
- [ ] **01.2** (agent) Issue unique immutable IDs: `turn_id`/assignment ID and `gate_id`, each bound to a `state_revision`, durably persisted **by the transition that creates them** (never minted on a read). Accept-once semantics under the lock. Test: duplicate identical accept → same receipt; stale/conflicting → rejected.
- [ ] **01.3** (agent) Ledger as projection: derive the append-only human-readable ledger from accepted artifacts only; never treat it as an independent source of truth. Test: ledger reconstructs from artifacts.
- [ ] **01.4** (agent) Two-tier locking with safe reclamation. (a) A **repository-level allocation / current-pointer lock+CAS** that serializes two *simultaneous first attaches* — the run dir does not exist yet, so a run-dir lock cannot. (b) The per-run mutation lock that `wait` never holds. Lock ownership is **nonce-based** (release only the exact instance acquired), AND has a **proven crash-stale reclaim mechanism** chosen in 00.4 (OS-held advisory lock, or nonce + process-start identity / other liveness) so a crashed short-lived CLI cannot brick the repository forever — "no PID-only detection" must not mean "never reclaimable." Test: concurrent first-attach yields one run; `wait` running while `status` returns promptly; a stale lock from a dead process is safely reclaimed while a live holder is not.
- [ ] **01.5** (agent) Prepared-transaction journal with a full **crash-cut matrix**. Enumerate every cut and make recovery idempotent at each: prepared journal written → immutable result artifact → commit creation → ref CAS → checked-out index reconciliation → state CAS → receipt → ledger projection → journal completion. On an incomplete transaction, replay forward or roll back deterministically. Test the fake-participant variant of each cut here; the Git-specific cuts are proven in 04.
- [ ] **01.6** (agent) Read-only commands do not reconcile. `pull`/`status` never mutate: if a journal is pending they project `recovery_pending` + the exact recovery action; the next *mutating* command reconciles under lock, or an explicit `recover` command does. Test: `status` over a pending journal is side-effect-free and reports the pending state.
- [ ] **01.7** (agent) Migration + fail-closed with a **major-version bump** (D011): new attach fields are a new state major version; a pre-pivot in-flight run is not silently reinterpreted — `resume` fails closed with remediation, inspect/export allowed, converter opt-in. Carry `.bak` retention forward; unknown/future schema fails closed. Test: loading a pre-pivot run refuses resume with the remediation message; `.bak` retained.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
