# Architecture Decision Log — claudex (Go)

Durable rationale for the interactive-pairing coordinator. Each entry is stable;
supersession is recorded, not deleted.

## D001 — Attach over subprocess driving
The coordinator arbitrates two already-running, **human-interactive** agent
terminals (Claude Code + Codex) over a durable file protocol. It does **not**
spawn agents as headless subprocesses. The product is human-in-both-terminals
pair programming; the earlier subprocess driver was the wrong shape.

## D002 — Processless durable coordinator
Short-lived CLI + durable state + per-mutation lock. Every transition is an
atomic compare-and-swap on `(run_id, state_revision, turn_id)`. The ledger is a
projection of accepted artifacts, not an independent source of truth. `wait`
holds no lock. No daemon — a daemon adds lifecycle/recovery failure modes without
adding correctness.

## D003 — One orchestration protocol
There is a single protocol: `attach`/`pull`/`submit`/`wait`/`status` (+ gates,
operator commands). No headless `run` engine. A future headless adapter, if ever
built, is just another `pull/submit` client — not a second engine.

## D004 — Identity, not leases
Unique immutable `turn_id`/assignment id and `gate_id`, each bound to a
`state_revision`, accepted once under the lock. Duplicate identical submit →
prior receipt (idempotent, keyed by canonical artifact digest); stale/conflicting
→ rejected with current status. Tokens guard against accidental misrouting, not
security (same-user filesystem).

## D005 — Two tiers, honestly labelled
(1) **BYO attach** — pre-existing user terminals; cooperative; `protocol-only`;
the coordinator cannot detect out-of-worktree writes. (2) **Managed launch** —
claudex starts each provider TUI with inherited stdio + pinned cwd + provider
sandbox/tool flags; still fully interactive. Capabilities are labelled
individually (`sandboxed`, `cwd-pinned`, `telemetry-observed`), never one blanket
"enforced".

## D006 — Coordinator-owned git commit
Agents edit but never commit. On an implementation `submit` the coordinator
builds the snapshot in a temporary index, commits from that tree, CAS-updates the
run branch from the expected old HEAD, synchronises the checked-out index to the
new tree, and records commit + tree hashes. Review reads the committed object,
never the live worktree. Git ref update and state CAS cannot be atomic together,
so a prepared-transaction journal + startup reconciliation bridges them.

## D007 — Observable-only caps
Attach enforces only what the coordinator can observe: turn/fix counts, artifact
bytes, wall time. Token/cost/tool-call metering requires managed launch with a
real telemetry channel; unavailable usage caps are labelled `requires-telemetry`,
never silently "enforced". Agent self-report is never enforcement.

## D008 — MVP is the BYO tier with executable clients
The minimum viable product is autonomous BYO attach: the agent-side skills are
executable protocol clients (not documentation). Managed launch + distribution is
the enforcement/release layer on top.

## D009 — Base branch
Work branch `interactive-pairing`, based on `main` (the lean v2 baseline). The
prior `reliability-observability-refactor` work is parked; its tests/schemas are
harvested as evidence, not reused as code.

## D010 — Verifier context independence
VERIFY blocks until a fresh same-role session generation attaches; the incumbent
pair generation is invalid for VERIFY. BYO reports `fresh-session-declared`
(generation enforced, model-context freshness not); managed reports `fresh-process`
only when it truly launches one. No silent proceed-with-downgrade.

## D011 — Legacy in-flight run compatibility
Pre-pivot Python `state.json`/`current` runs are not migrated into attach
semantics. A read-only Go inspector/exporter can inspect and export (redacted)
legacy state and refuses execution/resume. The Go state format is a new major
version; unknown/future versions fail closed.

## D012 — (superseded by D013/D014) Reuse posture
Original: cherry-pick Python modules by evidence. Superseded once the language
changed to Go — there are no Python modules to port into a Go binary.

## D013 — Language: Go
Implemented in Go, not Python. Rationale: single-binary distribution (drop one
file next to `claude`/`codex`), trivial cross-compilation to Windows/Linux/macOS,
typed contracts + race detector + fuzzing for enough correctness, and native-git
shell-out. Rust rejected: this is an I/O-bound state machine around files, git,
and child processes — Rust's compile-time-safety edge is underused while its
build/cross-compile/velocity costs are paid on every change. The existing Python
is reference-only in git history.

Module path `github.com/David-c0degeek/claudex`. Go floor **1.25.0** (two-release
support window; `testing/synctest` for deterministic long-poll/concurrency tests;
`os.Root` for traversal-resistant artifact/evidence paths). No 1.26-only feature
is required; the exact release toolchain is pinned separately in CI/release.

## D014 — Greenfield core + evidence harvesting
Every implementation is rederived fresh in Go. No line of the old Python is
transplanted. Reuse enters only as harvested *test vectors / schemas / fixtures*
(see `docs/evidence-harvest.md`): subtle behaviours (git tree identity, process
cancellation, redaction, usage normalization, canonical-JSON determinism) are
pinned by the old tests + counterexamples before they are coded.

## D015 — Platform + filesystem support
`windows/amd64` + `linux/amd64` are first-class (both are release gates);
macOS/arm64 is best-effort. OS primitives live behind tiny capability-split,
build-tagged packages — `internal/oslock`, `internal/atomicfile`,
`internal/proctree`, and (only if telemetry needs it) `internal/pty` — each
integration-tested on real Windows + Linux; Go gives OS access, not OS-uniformity.
Coordinator state runs only on **local filesystems**, classified
`supported-local | known-unsupported | unknown`: SMB/NFS and detectable sync roots
are `known-unsupported`; undetectable third-party sync roots are `unknown` (refuse
by default or explicit acknowledgement). No claim of perfect detection.
Classification is enforced at attach bootstrap; `doctor` reports it.

## D016 — Run input and policy contract
A run's inputs are explicit, versioned, validated, and frozen — never ambient.
The first `attach` (the sole bootstrap, D003) resolves:
- a **versioned task-contract file** (the harvested `TASK_CONTRACT` shape: goal,
  desired behaviour, scope, non-goals, acceptance criteria, required tests,
  relevant files) — required, with a documented default location/flag;
- a **config / run-policy** (from a config file + flags): the mechanical
  `test_command`, the observable caps (turn/fix counts, artifact bytes, wall
  time), timeouts, evidence limits, and base/repo policy.

`attach` validates both, hashes and copies them into the run directory, and
persists the **effective run policy** into run state so later config edits cannot
change a live run (frozen-at-bootstrap). `internal/config` owns parsing,
defaulting, and validation; `internal/state` persists the frozen policy + the
task-contract hash. An `init` command that scaffolds a task/config file is
optional sugar; the input *source* is mandatory. This closes the orphaned
`TASK_CONTRACT` and gives `test_command`/caps/timeouts a definite owner before
subject 01 shapes state.

### Implementation notes (00.4 research — decisions locked for subject 01)
- **Advisory locks with death-semantics** — an OS-held advisory lock is released
  by the kernel when the holding process exits, which *is* the crash-stale reclaim
  signal (no PID-guessing). **POSIX: `flock(LOCK_EX|LOCK_NB)`** (`golang.org/x/sys/unix`),
  chosen over `fcntl` — `flock` is per-open-file-handle with simple whole-file
  semantics and clean release on the last close/exit, and the NFS caveat is moot
  because D015 requires local filesystems. **Windows: `LockFileEx` on a handle**
  (`golang.org/x/sys/windows`), released when the handle/process closes. Both the
  repo-level allocation lock and the per-run mutation lock use this.
- **File replace — POSIX atomic, Windows NOT atomic (see D017 for the state
  root of trust).** `os.Rename` is an atomic replace on **POSIX** (`rename(2)`): a
  reader sees old or new bytes, never torn; a directory fsync adds power-loss
  durability. On **Windows**, `os.Rename` uses `MoveFileEx(MOVEFILE_REPLACE_EXISTING)`,
  which **Go's own contract explicitly does NOT guarantee to be atomic**
  (https://pkg.go.dev/os#Rename), and it does not pass `MOVEFILE_WRITE_THROUGH`, so
  neither torn-write safety nor power-loss durability can be assumed. `internal/atomicfile`
  therefore makes no old-or-new promise on Windows and implements no recovery; it is a
  low-level write primitive only. Windows crash-consistency of the authoritative state
  is provided by the immutable-generation protocol (D017), not by overwrite-in-place.
  Windows rename retries only transient sharing/access errors (`ERROR_SHARING_VIOLATION`/
  `ERROR_ACCESS_DENIED`), bounded, with an injectable backoff.
- **Process-tree kill** — Windows: create-suspended → `AssignProcessToJobObject`
  → resume, then terminate the job (race-free). POSIX: `Setpgid` + `kill(-pgid)`.
- **Filesystem classification** — Windows: `GetDriveType`/volume info to spot
  network drives; Linux: `statfs` `f_type` magics (NFS `0x6969`, SMB/CIFS
  `0xff534d42`) + `/proc/mounts`. Third-party sync roots (OneDrive-style) are not
  universally detectable → `unknown`.
- **Restricted canonical JSON** — a dependency-free canonicalizer (`internal/canonjson`)
  using RFC 8785 (JCS) string escaping and UTF-16 object-key ordering, but a
  restricted numeric domain: only integers in the safe range ±(2^53−1) are
  accepted; non-integer numbers and larger integers are rejected (fractional
  quantities use integer-scaled minor units, full-width identities/counters use
  strings). This avoids reproducing ECMAScript shortest-round-trip float
  formatting and preserves digest identity under JavaScript providers. The parser
  is strict — it rejects duplicate keys, lone surrogates, invalid UTF-8, leading
  zeros, trailing content, and oversize/over-nested input, yielding exactly one
  canonical representation per accepted JSON data-model value (whitespace, key
  order, escape spelling, and a −0 sign all collapse). Digest is `sha256` over the
  canonical bytes.

## D017 — Immutable-generation persistence (state root of trust)
Because file replace is not atomic on Windows (D015), the authoritative run state
is **never overwritten in place**. Instead it is an append-only sequence of
**immutable, self-validating generation records**:

- Each state mutation writes a NEW generation file (e.g. `state/000042.json`)
  containing a version, the payload, and a checksum/framing over the exact bytes.
  A generation file, once written, is never modified.
- A partial or torn new generation (crash mid-write) is **invalid by checksum** and
  ignored; the previous valid generation remains the state. Writing a brand-new
  file cannot corrupt an existing one, so the non-atomic Windows replace problem
  does not touch the root of trust.
- Recovery **enumerates** the generation files and validates a digest-linked chain
  from the current root to the head, selecting the head. A `current` pointer file
  may be written as an optimization, but recovery MUST be able to work without
  trusting it (a torn pointer falls back to enumeration). If no valid chain exists,
  recovery fails closed with remediation.
- The prepared-transaction journal (D006/01.5) is itself persisted as such a
  generation/immutable record, so it never depends on the single replace whose
  failure it is meant to diagnose. Its reconciliation defines every observable
  target/temp/pointer/generation state after a crash cut.

**Amended (M3(B), 2026-07-18) — prunable chain with an immutable structural
certificate root.** Requiring an unbroken chain from generation 1 bricked pruning,
failed closed on old-ancestor bit-rot despite a valid head, and let the per-txn
journal grow without bound (O(n²) re-scan). Recovery is now rooted at a **structural
certificate**, not at generation 1:

- A certificate is a `<cert_seq>.anchor` file in an INDEPENDENT, monotonically
  allocated sequence namespace (zero-padded, framed + checksummed exactly like a
  generation record). Its checksummed body binds `{anchor_format_version,
  certificate_sequence, root_generation, root_digest}`; the root generation is
  PAYLOAD, and must be an ACTUAL retained chain member with that exact digest.
  `cert_seq` is `highest occupied + 1`, skips occupied/torn slots (a torn cert from a
  crashed publish just reserves its sequence; the retry allocates the next and
  re-certifies the same floor), and is NEVER reused; the same checked ceiling/overflow
  rule as generation allocation applies.
- **Prune (`PruneKeep(K)`), run-lock only, one-way order:** advance the certificate
  ONLY when the retained chain exceeds K members (floor = the `(len−K)`-th real chain
  member, gap-safe), publishing the new floor certificate DURABLY (write-through +
  confirm) **while the prior certificate and all generations still exist**; then
  ALWAYS reconcile idempotently against the highest DURABLE certificate — confirm it
  durable and its exact identity BEFORE any deletion, then delete generations below
  its root, then delete obsolete (lower) certificates, then sync. Reconcile runs
  whether or not the advance did, so a crash in a prior prune's deletion is finished
  on re-run without a superfluous certificate.
- **Recovery** selects the HIGHEST self-intact certificate by `cert_seq` and validates
  its `root_generation` binds a present member with digest == `root_digest`, then
  validates `root..head`; records below the root are not required. Taxonomy mirrors
  the generation taxonomy: canonical-torn/oversize certs are skipped (reserving their
  sequence); a non-canonical name / symlink / non-regular entry fails closed; a
  self-intact but UNSUPPORTED `anchor_format_version` fails closed with upgrade
  remediation (never silently ignored); the highest intact supported certificate whose
  bound root is missing/mismatched is corrupt with NO fallback to a lower certificate.
  Recovery does NOT re-compare monotonicity against a (possibly deleted) predecessor —
  root advancement is enforced at PUBLISH under the guard.
- **Generation-number non-reuse:** append still continues at `head+1`; pruned numbers
  are removed and never revisited.
- **Stable lock-free reads:** a lock-free reader brackets the scan by the highest
  certificate's identity — note it, validate `root..head`, re-note it; a concurrent
  prune (a higher certificate, or a root-or-above member deleted mid-scan) triggers a
  bounded retry, returning a TYPED TRANSIENT "concurrent prune" error on exhaustion
  (never corruption). A not-exist strictly below the certified root is ignored.
- **Downgrade contract (fail-closed):** after a prune, generation 1 is gone, so a
  pre-M3 binary (which required a generation-1 root) finds no root and fails closed —
  it never silently mis-recovers.

**Acceptance:** inject crash cuts at write / flush / replace / pointer-update and
prove deterministic recovery — the last valid generation always wins, a torn new
generation or pointer never wins, and an empty/all-invalid set fails closed —
plus the prune fault/race matrix (torn-cert-reserves-sequence + retry, partial-
deletion finished on re-run, `≤K`-with-stragglers reconcile without a new cert,
monotonic root at publish, `K==0` rejected, sequence ceiling, the recovery
certificate taxonomy, and the lock-free reader-race bracket returning the transient
error and never corruption). This is subject 01's state/recovery acceptance matrix;
`internal/atomicfile` writes the individual generation/certificate files but provides
none of this protocol itself.

## D018 — Transport durability and canonicalization posture
The client-facing transport (subject 02) commits to four durable rules, each
chosen to make an unsafe state unrepresentable rather than merely validated:

- **Restricted canonical JSON, not full RFC 8785.** Digests are taken over
  canonical bytes using JCS string escaping and UTF-16 key ordering, but the
  numeric domain is deliberately narrowed to safe integers (±(2^53−1)) with a
  strict parser (rejects duplicate keys, lone surrogates, invalid UTF-8, leading
  zeros, trailing content, oversize, over-nesting). Protocol messages carry only
  integer counters/revisions, so floats buy nothing and cost cross-language
  digest-stability risk. The same embedded schema bytes serve BOTH provider
  instruction and coordinator validation — one source, no divergent validators.
- **Immutable, content-addressed artifacts published no-clobber.** A submit
  artifact is `<turn_id>/<digest>.json`, written to a temp within an `os.Root`,
  fsynced, then **hard-linked** into place (fails if the target exists). Because
  publication is never a replace, two racing publishers cannot overwrite each
  other and a reader sees complete-or-nothing with no sharing-retry — which the
  non-atomic Windows rename (D015) would otherwise require. The store admits only
  the allowlisted submit artifact types and only already-redacted canonical bytes
  (a secret in a control/free-text field is rejected, never rewritten, since a
  rewrite would change the digest).
- **One authoritative acceptance fact.** State persists only the accepted
  `Phase` per turn; role and artifact type are derived from the single
  `TurnSpec(phase)` source, so no redundant facts can disagree or be injected
  through the mirror. Acceptance is bound — symmetrically in state and in the
  submit path — to the exact live, current-revision assigned turn, and a
  stopped/gated/recovering run refuses before any artifact is written.
- **The human-readable mailbox is a rebuildable projection.** The single
  `.claudex/mailbox.md` mirror is re-derived from the accepted-artifact ledger and
  independently re-validates every artifact (recomputed digest, canonical +
  redacted bytes, schema, strictly increasing order) before rendering typed,
  injection-safe summaries. It is atomically replaced, never appended, so it can
  always be reconstructed from durable state.

**Acceptance:** subject 02's transport suite — digest stability across
whitespace/key-order permutations; concurrent publishers/readers never observe a
torn or overwritten artifact (`-race`); non-regular/symlink/oversize reads and
non-canonical/tampered inboxes are refused; a not-live or stale-assignment submit
is rejected before the sink; and a required-field state-shape change bumps the
on-disk schema version (v2) so an older generation fails with clear remediation.

## D019 — Provider usage-window auto-resume
A long, unattended two-TUI pairing loop must survive hitting a provider usage
limit, not die at it. When a provider (Claude or Codex) returns a rolling
usage-limit signal, the run pauses durably in `rate_limited`/`paused_budget`
carrying a **frozen reset/resume-at timestamp**, and auto-continues to the same
turn when the window passes (Claude subscriptions reset ~every 5 hours) with no
human action.

Because the coordinator is processless (D002), "auto-resume" is not a running
timer: the reset time is recorded in durable state, and a scheduled invocation
(or the next `wait`/operator poll) performs the resume transition once the window
has passed. `status` projects the reset time and the exact next action; reads
never resume. The mechanism carries over the intent of the retired Python
reliability engine (`limits.py` rate-limit handling, `lifecycle.py`) as a
requirement, not as ported code (D014). Subject 05 owns the policy and the
auto-resume transition; the managed tier (07) extends it to the two interactive
TUIs.

**Acceptance:** a simulated usage-limit pause records a reset time; a resume
before the reset is refused/no-op; a resume at or after the reset returns the run
to the same turn without a gate or human decision.

## D020 — Verification scope expansion is a convergence blocker
The final VERIFY acceptance check treats `scope_expansion` (work done beyond the
agreed scope) as a convergence **blocker**, alongside an unmet acceptance
criterion, non-meaningful tests, and unsupported claims — not as an
informational field. With no human decision requested: `verdict=fail` plus any
non-empty `scope_expansion` routes to FIX; `verdict=pass` plus a non-empty
`scope_expansion` is a contradiction and is rejected as a semantic error. A
`requires_human_decision` request gates first, after schema, identity, and exact
acceptance-criteria coverage validation. Verification convergence therefore is:
the criteria cover the frozen `TaskContract.AcceptanceCriteria` exactly once in
canonical order; blockers are any unmet criterion, `!tests_meaningful`, non-empty
`unsupported_claims`, or non-empty `scope_expansion`; then `pass && no blockers`
reaches DONE, `fail && blockers` routes to FIX (or the verify-quality gate at the
budget), and `pass && blockers` or `fail && no blockers` fails closed.

**Acceptance:** the pure engine's VERIFY projection rejects partial/reordered
criteria and a pass verdict that carries any blocker (including a scope
expansion); a fail verdict with a scope expansion routes to FIX.
