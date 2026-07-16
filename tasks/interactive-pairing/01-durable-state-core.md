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
- [x] **01.1** (agent) Add `state_revision` to run state and an atomic CAS mutation, persisted as **immutable generations (D017)** — never overwrite in place: a mutation supplies the expected revision and writes a NEW checksummed generation file (via `internal/atomicfile`) only if the current revision still matches, else returns a conflict with current status. Test: concurrent writers, only one wins; a new generation never corrupts the prior one.
- [x] **01.2** (agent) Issue unique immutable IDs: `turn_id`/assignment ID and `gate_id`, each bound to a `state_revision`, durably persisted **by the transition that creates them** (never minted on a read). Accept-once semantics under the lock. Test: duplicate identical accept → same receipt; stale/conflicting → rejected.
- [x] **01.3** (agent) Ledger as projection: derive the append-only human-readable ledger from accepted artifacts only; never treat it as an independent source of truth. Test: ledger reconstructs from artifacts.
- [x] **01.4** (agent) Two-tier locking with safe reclamation. (a) A **repository-level allocation / current-pointer lock+CAS** that serializes two *simultaneous first attaches* — the run dir does not exist yet, so a run-dir lock cannot. (b) The per-run mutation lock that `wait` never holds. Lock ownership is **nonce-based** (release only the exact instance acquired), AND has a **proven crash-stale reclaim mechanism** chosen in 00.4 (OS-held advisory lock, or nonce + process-start identity / other liveness) so a crashed short-lived CLI cannot brick the repository forever — "no PID-only detection" must not mean "never reclaimable." Test: concurrent first-attach yields one run; `wait` running while `status` returns promptly; a stale lock from a dead process is safely reclaimed while a live holder is not.
- [x] **01.5** (agent) Prepared-transaction journal — itself an **immutable generation record (D017)** so it never depends on the single replace whose failure it diagnoses — with a full **crash-cut matrix**. Enumerate every cut and make recovery idempotent at each: journal generation written → immutable result artifact → commit creation → ref CAS → checked-out index reconciliation → state CAS → receipt → ledger projection → journal completion. Reconciliation defines every observable target/temp/pointer/generation state. On an incomplete transaction, replay forward or roll back deterministically. Test the fake-participant variant of each cut here (inject cuts at write/flush/replace/pointer-update); the Git-specific cuts are proven in 04.
- [x] **01.6** (agent) Recovery + read-only discipline. Recovery **enumerates generations and selects the highest valid by checksum** (the `current` pointer is an untrusted optimization; a torn pointer falls back to enumeration); an empty/all-invalid set fails closed with remediation; a crash-left `.claudex-tmp-*` or torn generation is swept only under the exclusive lock, never removing the generation recovery would select. `pull`/`status` never mutate: a pending journal projects `recovery_pending` + the exact action; the next *mutating* command (or explicit `recover`) reconciles under lock. Test: highest valid generation always wins; torn generation/pointer never wins; `status` over a pending journal is side-effect-free.
- [x] **01.7** (agent) Migration + fail-closed with a **major-version bump** (D011), plus a working **read-only legacy inspector/exporter**: new attach fields are a new state major version; a pre-pivot Python `state.json`/`current` run is never reinterpreted as an attach run — `resume`/execution fail closed with remediation. The Go inspector reads a harvested Python-state fixture, prints/exports a redacted view (§ redaction), and refuses execution/resume. Carry `.bak` retention forward; unknown/future schema fails closed. Test: on a harvested Python-state fixture the inspector exports redacted output and refuses to resume/execute; unknown schema fails closed; `.bak` retained.
- [x] **01.9** (agent) Run input + policy (`internal/config`, D016): parse/default/validate a versioned task-contract file + config/run-policy (test_command, observable caps, timeouts, evidence limits, base/repo policy); `internal/state` persists the **frozen effective policy** + task-contract hash so later edits cannot change a live run. Test: a valid contract+config freezes into state; an invalid one is rejected with remediation; a post-freeze config edit does not change the run's effective policy.
- [x] **01.10** (agent) Redaction at every persistence boundary: all state/guidance/`human_context`/artifact bytes pass through `internal/redact` (already built) before they are written; the durable digest is taken over the exact persisted (redacted, canonical) bytes so two submissions differing only in a secret collapse identically. Test: a secret in persisted state/guidance is absent from bytes on disk; digest is over the redacted bytes.
- [x] **01.8** (agent) Local-filesystem classifier (`internal/fsclass`, D015): classify the coordinator state dir as `supported-local | known-unsupported | unknown`. Detect known-unsupported (SMB/NFS and detectable sync roots) where the OS exposes it; `unknown` covers third-party OneDrive-like providers that cannot be universally detected — do NOT claim perfect detection. Provide the classification for 03 bootstrap to enforce and 07 `doctor` to report; define the policy for `unknown` (refuse by default, or explicit acknowledgement). Test: a local dir → `supported-local`; a simulated network path → `known-unsupported`; classification is advisory-honest about `unknown`.

## Hindsight checkpoint
- [x] Captain Hindsight review recorded
- [x] Verdict is `CLOSE`

### Captain Hindsight — subject 01 (2026-07-16)

**1. Keep.**
- Immutable-generation persistence (never overwrite; recover by enumerating + validating the prev-digest chain from generation 1) is the correct root of trust on a platform without an atomic in-place replace. It made the state store, the catalog, and the transaction journal all crash-safe by the same mechanism.
- The transaction journal as a **step-progress machine** (observe → apply → re-observe → record), bound to its plan by persisted step IDs, with recovery re-observing every durable cut. Three review rounds converged it; the crash-cut matrix (7 canonical steps × 3 restart points) plus prefix-regression / re-observe / plan-mismatch / guard-before-effect / intent-bound-abort tests pin observable behaviour, not internals.
- Composable one-lock mutation (`genstore.Guard` shared across state + journal in one critical section); `wait`/read paths hold no lock.
- Honesty labels held: `fsclass` is explicit about the `unknown` tier; the legacy refusal is labeled seam-only until its bootstrap call-site lands.
- Validation lives with the data (cross-field run-state invariants, accept-once/append-only turns, canonical locators, frozen-policy coherence), enforced inside the generation builder so a bad state can never be written.

**2. Fix before closing (all resolved).**
- Ledger box was architecturally present (accepted-turns as truth) but had no projection function/test → added `state.Ledger` + reconstruction/purity test.
- Migration/legacy box was entirely unbuilt → added `internal/legacy` (detector, redacted read-only inspector, `CheckRunDir` fail-closed guard) with a harvested schema-6 fixture and the mandatory bootstrap call-site recorded in 03.1.
- Guard could reach an external effect before validation, and abort wasn't intent-bound → fixed in txn (`CheckGuard` first; `reflect.DeepEqual` intent bind).
- `CheckRunDir` initially returned nil for unrecognized `state.json` → made conservative (`*UnknownStateError`), since attach state never lives at `<dir>/state.json`.
- **Plan-hygiene:** shipped Go comments cited decision/box IDs (`D0xx`, `NN.M`) → swept to plan-agnostic prose (rationale kept, identifiers dropped); grep for IDs in `internal/`+`cmd/` returns zero.

**3. Record.** Lessons captured in `tasks/interactive-pairing/lessons.md`: (a) audit shipped code against every box before claiming a subject done — a green build on the packages that exist is not evidence the missing boxes are done; (b) a durable guard must key on the real storage boundary (absence of the store), not on decoding a file the real path never reads; (c) keep comments plan-agnostic from the first commit, not as a close-time sweep.

**4. Risk.** The legacy end-to-end refusal is a seam only until 03.1 wires `CheckRunDir`; tracked as a mandatory, explicit closure dependency in subject 03. No two-session end-to-end exists yet (that arrives in 03/06); subject 01's behaviour is verified at the unit/crash-matrix/CLI level, which is the appropriate tier for a durable-state core.

**5. Verdict: CLOSE.** Verified: `go build/vet ./...`, `gofmt -l .`, `go test ./...` green; cross-compile linux/amd64+arm64, darwin/arm64. Every box reviewed to `AGREE` by CX over the mailbox.

## Progress log
> One line per slice.

- 2026-07-16 · slice 1 · leaf primitives (01.4 lock, atomic writes) · Built `internal/atomicfile` (temp→chmod→fsync→rename with injectable ops; POSIX-atomic / Windows best-effort + `replaceWith` retry seam; `PostCommitSyncError`) and `internal/oslock` (flock/LockFileEx advisory locks with kernel death-semantics; cross-process crash-reclaim helper-process test). Pinned `golang.org/x/sys` v0.47.0. Green on Windows + cross-compiles. CX AGREE (`47559a4`→`147fb12`, 3 rounds — caught the Windows non-atomic-rename over-claim and the circular-recovery story → immutable-generation persistence adopted).
- 2026-07-16 · slice 2 · classifier + run input (01.8, 01.9) · `internal/fsclass` (tri-state `supported-local`/`known-unsupported`/`unknown`; Windows sync-root probe + `%OneDrive%` fallback; honest about undetectable providers) and `internal/config` (versioned task-contract + run-policy parse/default/validate; frozen effective policy). CX AGREE (`d9442d3`,`0059b35`,`aa54371`,`3ae8f4f`,`4ef7253` — caught the GetDriveType sync-root escape and input-strictness gaps).
- 2026-07-16 · slice 3 · generation store (01.1, 01.6) · `internal/genstore` — append-only immutable checksummed generations, `Guard` (one-lock mutation), `AppendLocked` with injected-write reconciliation (precommit→err / committed→record / else→ambiguous incl. `PostCommitSyncError`), chain-rooted recovery, quarantined torn slots. CX AGREE (`a08be6d`,`4e6bf72`,`dba76e0`,`71b4775`).
- 2026-07-16 · slice 4 · typed state + catalog (01.1, 01.2, 01.10) · `internal/state` — typed `RunState` over genstore, `Mutate`/`MutateLocked` CAS on revision, in-builder identity/receipt binding, accept-once/append-only turns, `redactAndGuard` at the persistence boundary (digest over redacted bytes), full cross-field invariants; repository run catalog with canonical locators + case-folding. CX AGREE (`fbee6ad`,`405445e`,`e8859ba`,`24b9e21`,`05e606a`).
- 2026-07-16 · slice 5 · transaction journal (01.5, 01.6) · `internal/txn` — step-progress machine bound to its plan by persisted step IDs; recovery re-observes every durable cut; guard validated before any effect; intent-bound abort. Crash-cut matrix (7 steps × 3 restart points) + prefix/re-observe/plan-mismatch/nil-callback tests. CX AGREE (`c0eba5c`,`50fc3f2`,`82e2bfc`,`30779c0`, 3 rounds — caught the two-phase corruption window, plan-binding, prefix-trust, and guard-before-effect holes).
- 2026-07-16 · slice 6 · ledger + legacy (01.3, 01.7) · `state.Ledger` (pure projection of accepted turns) and `internal/legacy` (detector + redacted read-only inspector + conservative `CheckRunDir` fail-closed guard; harvested schema-6 fixture; `claudex inspect-legacy` CLI). Bootstrap wiring recorded as a mandatory 03.1 dependency. CX AGREE (`b57ddc0`,`29663be`,`8e64354` — caught the hypothetical-vs-real refusal path, synthetic fixture, and the unrecognized-state nil hole).
- 2026-07-16 · slice 7 · subject close · Ticked 01.1–01.10; Captain Hindsight `CLOSE`; swept plan/decision IDs out of shipped Go comments (gate §6.11); updated CHANGELOG + architecture docs.

> **Earlier CX watch-items (state/recovery), all discharged:** (1) immutable generations cover EVERY authoritative mutable locator incl. the repo-level allocation catalog — any `current` file is an untrusted cache; (2) generation allocation handles invalid/torn filenames occupying the next revision, and an ambiguous `atomicfile` result reconciles by enumeration/validation before retrying; (3) ≥1 previously-validated generation is kept through cleanup; temps/generations are swept only under the exclusive owning lock.
