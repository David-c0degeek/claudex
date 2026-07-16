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

### Implementation notes (00.4 research)
- **Advisory locks with death-semantics** — an OS-held advisory lock is released
  by the kernel when the holding process exits, which *is* the crash-stale reclaim
  signal (no PID-guessing). Windows: `LockFileEx` on a handle (`golang.org/x/sys/windows`);
  POSIX: `flock`/`fcntl` (`golang.org/x/sys/unix`). The repo-level allocation lock
  and per-run mutation lock both use this.
- **Atomic rename** — `os.Rename` is atomic-replace within a directory (Windows
  uses `MoveFileEx(MOVEFILE_REPLACE_EXISTING)` under the hood); durability needs an
  fsync of the containing directory on POSIX. Sharing-violation retry on Windows.
- **Process-tree kill** — Windows: create-suspended → `AssignProcessToJobObject`
  → resume, then terminate the job (race-free). POSIX: `Setpgid` + `kill(-pgid)`.
- **Filesystem classification** — Windows: `GetDriveType`/volume info to spot
  network drives; Linux: `statfs` `f_type` magics (NFS `0x6969`, SMB/CIFS
  `0xff534d42`) + `/proc/mounts`. Third-party sync roots (OneDrive-style) are not
  universally detectable → `unknown`.
- **Canonical JSON** — RFC 8785 (JCS) for digest/idempotency so receipts are
  whitespace/key-order/platform independent; `json.Decoder.UseNumber()` to preserve
  number semantics. Library-vs-vendored decision is finalised in subject 02.
