# 01 — Durable State Core

## Goal
The processless coordinator's foundation: a durable run-state store with atomic
compare-and-swap transitions keyed on `(run_id, state_revision, turn_id)`, a
per-mutation lock that `wait` never holds, unique immutable `turn_id`/`gate_id`
issuance, a ledger that is a projection of accepted artifacts, and a
prepared-transaction journal so git+state operations survive a crash between the
git ref move and the state CAS.

## Integration analysis
> **Superseded by D013/D014 (Go greenfield):** the Python module names below are *reference-only requirements*, not reuse targets — implement fresh in Go (`internal/state`, `internal/txn`, `internal/oslock`, `internal/atomicfile`, `internal/fsclass`). "Reuse" means the harvested test vectors/schemas from 00.2, not ported code. Re-fill in Go terms before ticking any box.
> Fill/confirm against the 00.2 harvest before ticking any box.
- **New Go packages** — `internal/state` (durable run state + `state_revision` + CAS), `internal/txn` (generic prepared-transaction journal + reconciliation, tested with a fake participant), `internal/oslock` + `internal/atomicfile` (build-tagged, from 00.4), `internal/fsclass` (local-filesystem classifier). No Python is reused.
- **Requirements harvested (D014)** — from refactor-branch tests/history as vectors: schema-versioned state with `.bak` retention + fail-closed on unknown schema; a lock that serializes cross-process mutation; recovery that records phase/attempt/reason/next-action. These are the *behaviours to satisfy*, drawn from the old tests, not the old `state.py`/`lifecycle.py` code.
- **Integration point + why** — CAS + revision live in `internal/state`; the journal is `internal/txn` because it spans state + git; locks/atomic-writes are build-tagged OS packages (§6.23) so Windows/POSIX differences are isolated and integration-tested.
- **Do not duplicate** — one state store, one lock package, one journal; `internal/txn` is git-agnostic (subject 04 plugs in the git participant).
- **Vision fit** — realizes D002/D004/D006/D014/D015; "coordinator is code, not a model."
- **Risks** — Windows lock death-semantics + atomic rename edge cases; torn writes; journal/reconciliation complexity; filesystem misclassification (over/under-detection).

> The prepared-transaction machine here is **generic** — tested against a fake
> participant. Subject 04 supplies the real Git participant and the crash-cut
> tests. Keep the seam; do not fold Git specifics into this subject.
> Digests/canonical bytes are **supplied by tests** here; canonical-JSON +
> schema embedding is subject 02's job (`internal/canonjson`/`internal/protocol`),
> so state never depends on transport schemas (00→01→02 preserved).

## Boxes
- [ ] **01.1** (agent) Add `state_revision` to run state and an atomic CAS write helper: a mutation supplies the expected revision, and the write commits only if it still matches, else returns a conflict with current status. Temp→fsync→rename persistence. Test: concurrent writers, only one wins.
- [ ] **01.2** (agent) Issue unique immutable IDs: `turn_id`/assignment ID and `gate_id`, each bound to a `state_revision`, durably persisted **by the transition that creates them** (never minted on a read). Accept-once semantics under the lock. Test: duplicate identical accept → same receipt; stale/conflicting → rejected.
- [ ] **01.3** (agent) Ledger as projection: derive the append-only human-readable ledger from accepted artifacts only; never treat it as an independent source of truth. Test: ledger reconstructs from artifacts.
- [ ] **01.4** (agent) Two-tier locking with safe reclamation. (a) A **repository-level allocation / current-pointer lock+CAS** that serializes two *simultaneous first attaches* — the run dir does not exist yet, so a run-dir lock cannot. (b) The per-run mutation lock that `wait` never holds. Lock ownership is **nonce-based** (release only the exact instance acquired), AND has a **proven crash-stale reclaim mechanism** chosen in 00.4 (OS-held advisory lock, or nonce + process-start identity / other liveness) so a crashed short-lived CLI cannot brick the repository forever — "no PID-only detection" must not mean "never reclaimable." Test: concurrent first-attach yields one run; `wait` running while `status` returns promptly; a stale lock from a dead process is safely reclaimed while a live holder is not.
- [ ] **01.5** (agent) Prepared-transaction journal with a full **crash-cut matrix**. Enumerate every cut and make recovery idempotent at each: prepared journal written → immutable result artifact → commit creation → ref CAS → checked-out index reconciliation → state CAS → receipt → ledger projection → journal completion. On an incomplete transaction, replay forward or roll back deterministically. Test the fake-participant variant of each cut here; the Git-specific cuts are proven in 04.
- [ ] **01.6** (agent) Read-only commands do not reconcile. `pull`/`status` never mutate: if a journal is pending they project `recovery_pending` + the exact recovery action; the next *mutating* command reconciles under lock, or an explicit `recover` command does. Test: `status` over a pending journal is side-effect-free and reports the pending state.
- [ ] **01.7** (agent) Migration + fail-closed with a **major-version bump** (D011), plus a working **read-only legacy inspector/exporter**: new attach fields are a new state major version; a pre-pivot Python `state.json`/`current` run is never reinterpreted as an attach run — `resume`/execution fail closed with remediation. The Go inspector reads a harvested Python-state fixture, prints/exports a redacted view (§ redaction), and refuses execution/resume. Carry `.bak` retention forward; unknown/future schema fails closed. Test: on a harvested Python-state fixture the inspector exports redacted output and refuses to resume/execute; unknown schema fails closed; `.bak` retained.
- [ ] **01.8** (agent) Local-filesystem classifier (`internal/fsclass`, D015): classify the coordinator state dir as `supported-local | known-unsupported | unknown`. Detect known-unsupported (SMB/NFS and detectable sync roots) where the OS exposes it; `unknown` covers third-party OneDrive-like providers that cannot be universally detected — do NOT claim perfect detection. Provide the classification for 03 bootstrap to enforce and 07 `doctor` to report; define the policy for `unknown` (refuse by default, or explicit acknowledgement). Test: a local dir → `supported-local`; a simulated network path → `known-unsupported`; classification is advisory-honest about `unknown`.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
