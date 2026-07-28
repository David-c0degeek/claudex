# claudex Architecture (Go)

claudex is a **processless coordinator** for two human-interactive AI agent
terminals — Claude Code and Codex — pair-programming on one plan and one
implementation, converging by agreement. The human picks the LEAD by choosing
which terminal to drive; both terminals stay interactive so either agent can ask
its human at any phase.

## The loop

```
INIT → PLAN_DRAFT → PLAN_CRITIQUE → PLAN_REVISE
     → IMPLEMENT_STEP → CHECKPOINT → FIX  (per step)
     → TESTS → VERIFY → DONE
AWAIT_GUIDANCE / PAUSED_BUDGET on gates.
```

Convergence = pair verdict `AGREE` with zero blocking/major findings (plus no
missing-evidence and no `tests_adequate:false`). The coordinator is code, not a
model: it computes transitions, caps, diffs, test results, and convergence
deterministically. Models argue; the state machine decides.

## Coordinator shape (D002)

Every `claudex` invocation is short-lived. State lives under `.claudex/` on a
**local filesystem** (D015). Two lock tiers (D015 impl notes):
- a **repository-level allocation lock** so two simultaneous first-attaches
  cannot split-brain a run (the run dir does not exist yet), and
- a **per-run mutation lock** that `wait` never holds.

Both are OS-held advisory locks, so a crashed CLI's lock is reclaimed by the
kernel — no PID-guessing. Every state mutation is an atomic CAS on the expected
`state_revision`. Because file replace is not atomic on Windows (D015),
authoritative state is persisted as **immutable, checksummed generations that are
never overwritten** (D017): a torn new generation is invalid-by-checksum and
ignored, recovery enumerates and picks the highest valid generation (an
untrusted `current` pointer is only an optimization), and the journal is itself
such a record so it never depends on the replace it repairs. The human-readable
ledger is a projection of accepted artifacts, never an independent source of
truth.

## Run input and policy (D016)

A run's inputs are explicit and frozen at bootstrap, never ambient. The first
`attach` resolves a **versioned task-contract file** (goal, desired behaviour,
scope, non-goals, acceptance criteria, required tests, relevant files) and a
**config / run-policy** (the mechanical test gate, observable caps, timeouts,
evidence limits, base/repo policy) from a config file + flags, validates both,
hashes and copies them into the run directory, and persists the **effective run
policy** into run state. Later config edits cannot change a live run.
`internal/config` owns parse/default/validate; `internal/state` persists the
frozen policy and the task-contract hash.

At run-policy v2 the gate is exactly one of an explicit `argv` vector or
`disabled: true` — never a command string, which would need a splitter and
therefore an implicit shell — and it carries the environment it is authorized to
run with (`env{inherit,set}`). Attach resolves that environment from the host
ONCE and persists the frozen result as `ResolvedExecution`; attempts and recovery
read it from state and never re-read ambient values. `HOME` is deliberately not
inherited, and a closed platform-required set is part of the authority rather
than something resolution adds. See D016.

## Transport protocol (D001, D004)

Role-addressed file mailbox under `.claudex/session/`. Four verbs:
- `pull` — returns the complete authoritative assignment (protocol/schema
  version, session/run/turn ids, role, phase, expected `state_revision`, binding
  guidance, artifact schema; the mutable worktree path **only** for the lead's
  IMPLEMENT/FIX turns — everyone else gets hash-addressed immutable evidence).
  Read-only and idempotent; never mints identity.
- `submit` — validates against the assignment schema, accept-once under the lock
  via CAS, returns a durable receipt. Duplicate identity is a **canonical
  artifact digest** (restricted canonical JSON — JCS escaping/key order, safe
  integer-only numbers) match → returns the receipt; different digest →
  conflict; stale revision → rejected.
- `wait --timeout N` — bounded long-poll; wakes on any relevant revision (my
  turn, a gate, cancellation, terminal/failure, session replacement); holds no
  lock; Ctrl-C-safe and idempotent.
- `status` — lock-free projection of lifecycle, current turn/gate, whose turn,
  caps remaining, and honesty labels.

Durable writes sit on rooted, capability-split atomic-file primitives
(`internal/atomicfile` over `os.Root` confinement, with the Windows sharing/access
retry and a committed-but-unsynced `PostCommitSyncError`). Artifacts are immutable
content-addressed `<turn_id>/<digest>.json` published **no-clobber** (hard-link,
fails if present) so two racing publishers cannot overwrite and a reader sees
complete-or-nothing; the store admits only the allowlisted submit artifact types
and only already-redacted canonical bytes. Session inboxes are role-addressed and
re-validated on read (canonical + schema + full assignment validation + session
identity). The single human-readable `.claudex/mailbox.md` mirror is a rebuildable
projection: it is re-derived from the accepted-artifact ledger and independently
re-validates every artifact (recomputed digest, canonical + redacted bytes,
schema) before rendering typed, injection-safe summaries — role and artifact type
come solely from the turn's authoritative accepted phase, the one fact state
persists per turn. Acceptance is bound, symmetrically in state and in `submit`, to
the exact live, current-revision assigned turn, and a stopped/gated/recovering run
refuses before any artifact is written. A required-field state-shape change is an
on-disk schema-version bump, gated on a loose version probe before strict decode
so an older generation fails with clear remediation.

## Git transaction (D006)

On an implementation `submit` the coordinator, via native `git` plumbing (shell
out, never a library):
1. builds the exact snapshot (tracked + non-ignored untracked) in a temp index
   (`GIT_INDEX_FILE` + `read-tree`/`write-tree`),
2. `commit-tree` from that tree with a deterministic plan-agnostic message,
3. `update-ref <ref> <new> <old>` — the native old-OID CAS,
4. synchronises the checked-out index to the new tree so the worktree does not
   read perpetually dirty,
5. records commit + tree hashes as immutable review evidence.

Because the git ref move and the state CAS cannot commit atomically, a
prepared-transaction journal is written first and reconciled on startup
(idempotent at every crash cut: journal → result artifact → commit → ref CAS →
index sync → state CAS → receipt → ledger → completion). Reviews read a hashed
evidence packet materialised from the committed object — never the live worktree.
The mechanical test gate runs outside the lock.

## Two tiers (D005, D007)

- **BYO attach** (`protocol-only`) — the human's own terminals; the coordinator
  enforces phase/role ownership, schema, commit ancestry, worktree cleanliness,
  exact diffs, tests, and merge gating, but cannot detect out-of-worktree writes
  or observe TUI usage. Only observable caps (turn/fix counts, artifact bytes,
  wall time) are enforced.
- **Managed launch** — claudex starts each TUI with inherited stdio + pinned cwd
  + provider sandbox/tool flags; still fully interactive. Capabilities are
  labelled individually; usage metering only if a real telemetry channel exists.

## Package map

Created per-slice as each subject lands (not all up front):

| Package | Role | Subject |
|---|---|---|
| `cmd/claudex` | CLI entry, command dispatch | 00 (skeleton) → grows |
| `internal/buildinfo` | version/build metadata | 00 |
| `internal/genstore` | append-only immutable-generation store (root of trust) | 01 |
| `internal/state` | durable run state, `state_revision`, CAS, run catalog, ledger projection, frozen run-policy | 01 |
| `internal/legacy` | read-only pre-pivot state inspector + bootstrap refusal guard | 01 |
| `internal/txn` | generic prepared-transaction journal + reconciliation | 01 |
| `internal/config` | task-contract + run-policy parse/default/validate/freeze | 01 |
| `internal/redact` | credential redaction at every persist/display boundary | 00/01 |
| `internal/oslock` | build-tagged OS advisory locks | 01 |
| `internal/atomicfile` | build-tagged atomic writes + rooted no-clobber/replace/read/sync primitives over `os.Root` | 01/02 |
| `internal/fsclass` | local-filesystem classifier | 01 |
| `internal/protocol` | embedded versioned JSON schema registry, keyword-strict compiler, value-free validation | 02 |
| `internal/canonjson` | restricted canonical JSON (JCS escaping/key order, safe integer-only numbers) + digest | 02 |
| `internal/transport` | `pull`/`submit`/`wait`/`status`, content-addressed artifact store, role-addressed inboxes, mailbox mirror | 02 |
| `internal/engine` | phase-transition table + convergence | 03 |
| `internal/gitx` | native-git shell-out plumbing | 04 |
| `internal/evidence` | committed-object review packet | 04 |
| `internal/proctree` | build-tagged process-tree runner (test gate) | 04 |
| `internal/gates`, `internal/caps`, `internal/lifecycle` | human gates, observable caps, macro states | 05 |
| `internal/harness` | Go e2e two-terminal simulator | 06 |
| `internal/launch`, `internal/provider` | managed launch, capability matrix; `internal/pty` only if telemetry needs it | 07 |

## Requirements source

Behaviour is pinned by harvested test vectors from the retired Python suite —
see [`evidence-harvest.md`](evidence-harvest.md). Decisions are in
[`decisions.md`](decisions.md).
