# 02 — Transport Protocol

## Goal
The client-facing protocol over a role-addressed file mailbox: `pull` (return
the complete authoritative assignment), `submit` (accept-once an artifact and
advance state), `wait --timeout N` (bounded long-poll returning `unchanged` or
the next assignment, holding no lock), and `status`. Atomic artifact writes,
idempotent receipts, and a protocol/schema version on every message.

## Integration analysis
> **Superseded by D013/D014 (Go greenfield):** the Python module names below are *reference-only requirements*, not reuse targets — implement fresh in Go (`internal/transport`, `cmd/claudex`, `internal/schema`). "Reuse" means the harvested test vectors/schemas from 00.2, not ported code. Canonical JSON (RFC 8785) backs digests (§6.25). Re-fill in Go terms before ticking any box.
> Fill/confirm against 00.2 before ticking any box.
- **New Go packages** — `cmd/claudex` (the `pull`/`submit`/`wait`/`status` subcommands), `internal/transport` (role-addressed mailbox under `.claudex/session/`, receipts), `internal/protocol` (versioned JSON schema bytes embedded via `//go:embed`), `internal/canonjson` (RFC 8785 canonicalization + digest), `internal/redact`. Atomic writes come from `internal/atomicfile` (01).
- **Requirements harvested (D014)** — the `README.md` mailbox block format (`===== [ROLE] turn N | phase | STATUS =====`); the `.mailbox/` PoC (TURN sequencing + blocking-poll loop) as the transport shape; the old schemas + redaction *contracts* as vectors. The Python `artifacts.py:save_json`/`mailbox_append` are reference-only (they were non-atomic — the Go writer is atomic by construction).
- **Behaviour to preserve** — human-readable mailbox ledger derived from accepted artifacts; typed JSON artifacts backing every claim; redaction at every persistence/display boundary.
- **Do not duplicate** — one schema source (embedded bytes, used for both provider instruction and coordinator validation); one canonicalizer; one atomic writer.
- **Do not duplicate** — no second artifact writer or schema validator.
- **Integration point + why** — a new `protocol.py` module holds the assignment/result contract; `cli.py` wires commands; state transitions delegate to 01's CAS.
- **Vision fit** — realizes D002 (`wait` holds no lock) + D004 (idempotent receipts).
- **Risks** — Windows atomic rename/file-sharing collisions; long-poll responsiveness; schema-version skew between coordinator and skill client.

## Boxes
- [ ] **02.1** (agent) Define the assignment contract returned by `pull`, **phase/role-specific**: protocol/schema version, session/run/turn IDs, role, phase, expected `state_revision`, binding guidance, artifact schema — plus, *only for the lead's IMPLEMENT/FIX turns*, the mutable worktree path. Planning / review / verify assignments carry **hash-addressed immutable evidence** (an immutable review root), never a live worktree path. `pull` is read-only and can never mint identity — it reads the `turn_id` the preceding transition durably issued (01.2). Test: an IMPLEMENT assignment has a worktree; a CHECKPOINT/VERIFY assignment has only immutable evidence; two pulls are identical.
- [ ] **02.2** (agent) `submit --file result.json`: validate against the assignment's schema, accept-once under the lock via 01 CAS, return a durable receipt (`turn_id` + resulting `state_revision`). Duplicate identity is defined by **canonical artifact digest** (via `internal/canonjson`, RFC 8785 — so it is whitespace/ordering-independent): persist digest + receipt; same `turn_id` + same digest → return the receipt (idempotent); same `turn_id` + different digest → conflict; stale revision → rejected with current status. `internal/canonjson` + embedded schema bytes must be in place before this box relies on them. Test all three paths.
- [ ] **02.3** (agent) `wait --timeout N`: bounded long-poll that returns `unchanged` on timeout, else the next actionable event for this session. It wakes on **any relevant revision** — an actionable assignment (my turn), a gate opening, cancellation, terminal/failure, or session replacement — not only "my turn," so a waiting terminal cannot sleep through STOP/DONE/guidance. Holds no lock; Ctrl-C-safe and idempotent. Test: wakes on turn arrival, on a gate, on cancellation, and returns `unchanged` on timeout.
- [ ] **02.4** (agent) `status`: lock-free read projecting lifecycle, current turn/gate, whose turn, caps remaining, and honesty labels (`protocol-only`/`managed`/capability tags). Test: reflects state after a submit without acquiring the mutation lock.
- [ ] **02.5** (agent) Atomic artifact + mailbox writes: temp→fsync→rename; role-addressed inboxes under `.claudex/session/`; append-only human-readable mailbox mirror derived from accepted artifacts (per 01.3). Windows file-sharing bounded retry. Test: concurrent readers never see a torn artifact.
- [ ] **02.6** (agent) Schema embedding + version negotiation: versioned JSON schema bytes are embedded in the binary (`//go:embed`) and used for BOTH provider instruction and coordinator validation; every message carries a protocol/schema version; a mismatched client version is rejected with clear remediation, not a silent misparse. This + `internal/canonjson` underpin 02.1/02.2 and must land before they rely on digests. Test: old-version submit rejected cleanly; the same embedded schema validates a round-tripped artifact.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
