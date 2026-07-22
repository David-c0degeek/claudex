# InteractivePairing Plan

> Reshapes claudex from a headless subprocess orchestrator into a coordinator
> for two **human-interactive** agent terminals (Claude Code + Codex) that
> pair-program on one plan and one implementation, converging by agreement.
> Disposable plan: this file and `tasks/interactive-pairing/` are deleted after
> §8 sign-off. Shipped code must not reference them (§6.11).

## Collaboration model

| Field | Value |
|---|---|
| Mode | `coordinated` |
| Primary owner | agent (CC as lead for this plan; CX pairs) |
| Coordinator | Primary owner |
| Resume safety | required |
| Parallel branches | `no` |
| Notes | Plan authored by CC (lead) + CX (pair) over the `.mailbox/` file channel — the very pattern being productized. Base `main`, work branch `interactive-pairing` (D009). Remote `origin` exists — push checkpoints. Human sign-off boxes tracked in `manual-actions.md`. |

Mode rules:
- `coordinated` — one primary worker (the active lead agent) owns the plan, with explicit non-agent boxes for base-branch confirmation, product sign-off, and release. Uses `manual-actions.md`; no parallel branch table required.

### Parallel work tracker

| Subject | Owner | Branch | Dependencies | Conflict-risk files | Status | Handoff notes |
|---|---|---|---|---|---|---|
| n/a | | | | | | |

### Checkpoint gate

Before considering a box done:
- Update the box, subject Progress log, §5 tracker, and any affected §4 decisions / lessons / manual actions.
- Run the relevant §2 Verification command, or record the exact blocker + reproduction command.
- **Behaviour verification:** for a box that changes runtime behaviour, exercise the changed flow end-to-end and record observed-vs-expected output — not a green test exit alone. For this plan the canonical drive is: two attached sessions complete a `pull → submit → wait` turn and the coordinator advances state + records evidence.
- **Doc-currency:** update every dependent doc in the same checkpoint (README, `docs/architecture.md`, `docs/decisions.md`, skill docs), or record `n/a` + reason.
- Commit the coherent checkpoint; push if a remote exists, else record the no-remote note in Notes and treat the local commit as the checkpoint.
- Never rewrite pushed checkpoint history.
- Keep commit messages / branch names / PR titles plan-agnostic (§6.11).

---

## 1. Subject

**Implementation substrate.** A greenfield **Go** binary (D013), rederived from scratch — the existing Python code is discarded, reference-only in git history; the refactor branch's schemas/fixtures/tests are harvested as executable requirements (D014). Native `git` is shelled out to, never a library (D013). Targets Windows + Linux first-class, macOS best-effort, with OS primitives behind build-tagged packages and local-filesystem-only state (D015).

**In scope.** Pivot claudex's execution model from "coordinator spawns both agents as headless subprocesses" to "coordinator arbitrates two already-running, fully human-interactive agent terminals over a durable file protocol." Deliver: a **processless durable coordinator** (short-lived CLI + locked durable state + CAS transitions + crash-reconciled git/state transaction journal); a **transport protocol** (`pull` / `submit` / `wait --timeout` / `status`) over a role-addressed file mailbox with idempotent receipts; an **attach + phase engine** (register a terminal to a role, run PLAN→IMPLEMENT→CHECKPOINT→TESTS→VERIFY→DONE firing on `submit`, converge on AGREE + zero blocking/major); **coordinator-owned git** (snapshot-and-commit on submit, immutable review evidence, mechanical test gate, merge gating); **human gates + observable-only caps**; **executable agent-side skills** (the `pull→work→submit→wait` loop for Claude and Codex, making BYO autonomous); and a **managed-interactive launcher** (claudex starts each TUI with inherited stdio, pinned cwd, provider sandbox flags — still fully interactive) with individually-labeled enforcement capabilities.

**Out of scope.** Multi-user / cross-machine auth (same-user filesystem assumed — stated, not solved). Network/RPC transport (files only). Rotating leadership or model-vs-model authority. A second headless orchestration engine (a future headless adapter, if ever built, is just another `pull/submit` client and is not part of this plan). Metered token/cost caps in BYO attach (impossible to observe honestly). Sandboxing an untrusted repository (a worktree is an isolation boundary for commits, not an OS sandbox). **Porting the Python implementation** (D014 — it is reference-only). A git library / go-git (D013 — shell out to native git). Coordinator state on network/sync filesystems (D015). Rust (D013).

### Risks and rollback

| Risk | Impact | Mitigation / rollback |
|---|---|---|
| Git ref update and state CAS cannot commit atomically; crash between them corrupts run | High — orphaned commit or lost turn | Prepared-transaction journal written before ref move; startup reconciliation replays or rolls back the incomplete transaction; review evidence is the committed tree, never the live worktree (D006, subject 01/04) |
| BYO attach cannot detect out-of-worktree writes; false sense of enforcement | Med — user trusts a boundary that doesn't exist | Label BYO capability `protocol-only`; individually-labeled managed capabilities; docs state the threat model plainly (D005/D007, subject 05/07) |
| Coordinator state placed on a network/sync filesystem (OneDrive/SMB/NFS) | High — advisory locks + file replace silently don't hold → corruption | Preflight/`doctor` detects and rejects or labels non-local state locations; local-filesystem-only is a documented constraint (D015) |
| Go OS-primitive layer assumed uniform across Windows/POSIX | High — Windows process-tree kill / lock death-semantics differ from Linux | Build-tagged per-OS packages + real Windows + Linux integration tests; Windows kill uses create-suspended→assign-job→resume, not best-effort (D015) |
| Rederiving subtle logic (git tree identity, cancellation, redaction, usage) reintroduces solved bugs | Med — known failure modes rediscovered | Harvest the refactor branch's tests/fixtures/counterexamples as Go test vectors first, implement against them (D014, subject 00.2) |
| Distribution/trust for a shipped binary (Windows SmartScreen/signing, checksums, provenance/SBOM) unaddressed | Med — users can't safely install | Release subject (07) owns signing/checksums/SBOM + the cross-build target matrix |
| Two humans submit/resolve concurrently against stale generations | Med — wrong turn/gate answered | Unique immutable `turn_id`/`gate_id` + `state_revision`, accept-once under lock; stale/conflicting rejected with current status (D004, subject 01/03) |
| Managed launch promises telemetry it can't deliver | Med — dishonest caps | Research metering channel in 00; label `sandboxed` vs `telemetry-observed` separately; never a blanket "enforced" (D007, subject 07) |
| Interactive agent commits or edits after snapshot | Med — review sees wrong tree | Skill instructs agents never to `git commit`; post-snapshot edits are uncommitted protocol violations that block the next lead submit and are surfaced, never absorbed (subject 04/06) |
| Pre-pivot in-flight headless run reinterpreted under attach semantics | High — silent state corruption of an existing run | State major-version bump; `resume` fails closed with remediation; inspect/export only; converter is opt-in and proven (D011, subject 00.8/01.7) |
| Coordinator commit leaves the checked-out index describing old HEAD → worktree always looks dirty → the next-submit block trips forever | High — MVP deadlocks after the first commit | 04.2 synchronizes the real index to the new tree (safe mixed reset, no file overwrite) and re-verifies clean; the index step is in the journal crash-cut matrix (subject 04) |
| Reviewer evidence sourced from the live worktree instead of the committed object | High — pair reviews mutable, wrong, or racing content | The evidence builder materializes files from the committed Git object (`git cat-file`/`archive` off the commit) + hashes the packet; never reads the live worktree; skills point the pair at the packet, never the lead worktree (subject 04/06) |
| Two simultaneous first-attaches race before the run dir exists | Med — split-brain run allocation | Repository-level nonce-based allocation/current-pointer lock+CAS, not only a run-dir lock; release only the acquired lock instance (subject 01.4) |
| A crashed short-lived CLI leaves a lock that no PID-only check can safely reclaim → repo bricked | Med — no run can ever start again | 00.4 chooses + 01.4 implements a proven liveness/reclaim mechanism (OS-held advisory lock, or nonce + process-start identity), with crash/reclaim tests; "no PID-only detection" must not mean "never reclaimable" (subject 01.4) |

---

## 2. Authoritative inputs

| Source | Contribution |
|---|---|
| `main` branch (base, confirmed) | Merge base / target this plan branches from and merges back into; work happens on `interactive-pairing` (D009) |
| `reliability-observability-refactor` branch (git history) | **Evidence harvest only** (D014): its 10 `test_*.py` modules + 6 fixture executables, golden outputs, JSON schemas, and crash-cut scenarios become Go test vectors. The Python modules are NOT ported — reference-only |
| Python code (main v2 + refactor), git history | Reference-only spec: reveals the problems solved (tree identity incl. untracked bytes, usage-at-boundary, cancellation via process-group, redaction boundaries, resume-vs-restart) — the *requirements*, not the implementation |
| `README.md` | Documents the intended pair-programming loop, convergence semantics, artifact/mailbox formats — the vision the Go code must realize |
| `docs/architecture.md`, `docs/decisions.md` (refactor-branch history only — absent on `main`) | Read via `git show reliability-observability-refactor:<path>` as historical input; the Go-era versions are **created fresh** in 00.8/07.5, not "updated" |
| `CHANGELOG.md`, `tasks/lessons.md` (refactor-branch history only — absent on `main`) | Historical input via `git show`; created fresh for the Go era |
| `.mailbox/` proof-of-concept (this session) | Working reference implementation of the attach transport (`to-codex.md`/`to-claude.md`, TURN sequencing, blocking-poll loop) |
| `tasks/claudex-refactor/lessons.md` + `tasks/lessons.md` | Prior-run lessons (streaming safety, usage-at-boundary, lifecycle vs phase, evidence manifests, resume-vs-restart, cancellation, tree identity) — reuse as requirements |
| CC↔CX design exchange (mailbox TURN 3–8) | The converged architecture + Go/greenfield decision — source of the §4 decisions |
| RFC 8785 (JSON Canonicalization) | Canonical-JSON standard for whitespace-independent digests/idempotency (D014); or a deliberately restricted equivalent |
| `git update-ref <ref> <newvalue> <oldvalue>` | Native old-OID compare-and-swap for the run-branch move (D006) — https://git-scm.com/docs/git-update-ref |
| `golang.org/x/sys/windows` | Windows OS primitives: `LockFileEx`, job objects, `CreateProcess`, ConPTY (D015) — https://pkg.go.dev/golang.org/x/sys/windows |

### Verification commands

| Purpose | Command | Notes |
|---|---|---|
| Build | `go build ./...` | Cross-build matrix (`GOOS=windows/linux/darwin`) confirmed in subject 00 / 07 |
| Test | `go test ./...` | Table-driven + harvested test vectors (D014); confirm module path in 00 |
| Vet | `go vet ./...` | Static checks |
| Race | `go test -race ./...` | CI-only where a C toolchain exists (race detector on Windows needs one) — not a universal local gate (D015) |
| Fuzz | `go test -run=^$ -fuzz=... -fuzztime=...` | Seed corpus from harvested counterexamples for canonical-JSON/parsing |
| Lint/format | `gofmt -l .` (+ optional `golangci-lint`) | `gofmt` clean is mandatory; `golangci-lint` if adopted in 00.6 |
| Platform integration | build-tagged OS tests on real Windows + Linux | Locks, file replace, process-tree kill (unconditional); PTY/ConPTY only **if** `internal/pty` is built by 07.3 — not provable in unit tests (D015) |
| Plan-specific gate | Two-session attach e2e (subject 06) | Scenario-driven fake TUIs drive `pull/submit/wait`; asserts state advance + evidence |

---

## 3. Subject file index

| # | File | Subject | Depends on |
|---|---|---|---|
| 00 | `tasks/interactive-pairing/00-tooling-research-and-readiness.md` | Evidence harvest, Go toolchain + skeleton, protocol ADR, readiness | — |
| 01 | `tasks/interactive-pairing/01-durable-state-core.md` | Durable state, CAS transitions, git/state transaction journal, crash reconciliation | 00 |
| 02 | `tasks/interactive-pairing/02-transport-protocol.md` | `pull`/`submit`/`wait`/`status` CLI, role-addressed mailbox, atomic artifacts, idempotent receipts | 01 |
| 03 | `tasks/interactive-pairing/03-attach-and-phase-engine.md` | `attach`/registration, assignment issuance, phase graph, convergence | 02 |
| 04 | `tasks/interactive-pairing/04-git-transaction-and-review-evidence.md` | Coordinator snapshot/commit, immutable review evidence, test + merge gate | 03 |
| 05 | `tasks/interactive-pairing/05-human-gates-and-caps.md` | Human gates (`resolve`/`continue`), observable-only caps, operator lifecycle, status/recovery UX | 04 |
| 06 | `tasks/interactive-pairing/06-agent-skills-and-byo-integration.md` | Executable Claude/Codex skill clients + BYO end-to-end, concurrency, crash/fault tests (**MVP milestone**) | 05 |
| 07 | `tasks/interactive-pairing/07-managed-launcher-and-release.md` | Managed-interactive launcher, enforcement capability matrix, cross-build/signing distribution, docs/release | 06 |

---

## 4. Decision log

| ID | Date | Title | Decision | Rationale | Refs (box IDs / files / ADR#) |
|---|---|---|---|---|---|
| D001 | 2026-07-16 | Attach over subprocess driving | Coordinator arbitrates two already-running human-interactive TUIs over a file protocol; it does not spawn agents as headless children | The product is human-in-both-terminals pair programming; the subprocess driver is the wrong shape (confirmed: `processes.py` Popen pipes; `pair` still spawns the pair headlessly) | `processes.py`, `agents.py`, `providers.py`; ADR TBD in 00 |
| D002 | 2026-07-16 | Processless durable coordinator | Short-lived CLI + durable state + per-mutation lock; every transition is an atomic CAS on `(run_id, state_revision, turn_id)`; ledger is a projection of accepted artifacts; `wait` holds no lock | A daemon adds lifecycle/recovery failure modes without adding correctness | subject 01; `state.py` |
| D003 | 2026-07-16 | One orchestration protocol | Drop the public headless `run` driver; a future headless adapter is just another `pull/submit` client, not a second engine | Keeping it doubles the driver/state/recovery/test matrix and preserves what we're replacing | subject 00 (retirement plan), `cli.py cmd_run` |
| D004 | 2026-07-16 | Identity, not leases | Unique immutable `turn_id`/assignment ID + `gate_id`, each with `state_revision`, accepted once under lock; duplicate identical submit → prior receipt; stale/conflicting → rejected with current status | Lease expiry creates ambiguity during long human/model turns; tokens guard misrouting, not security (shared FS) | subject 01/03 |
| D005 | 2026-07-16 | Two tiers, honestly labeled | (1) BYO attach = cooperative, `protocol-only`; (2) managed-interactive launch = inherited stdio + pinned cwd + provider sandbox flags, capabilities labeled individually | Interactive BYO cannot observe out-of-worktree writes or usage; managed recovers *some* enforcement but not all | subject 05/07 |
| D006 | 2026-07-16 | Coordinator-owned git commit | Agents edit but never commit; `submit` builds the snapshot in a temp index, commits from that tree, CAS-updates the run branch from expected old HEAD, journals a prepared transaction, reconciles on restart; review uses the committed tree only | Keeps git truly coordinator-owned + authoritative tree identity; git-ref and state-CAS cannot be atomic, so a journal + reconciliation is required | subject 01/04; `gitops.py` |
| D007 | 2026-07-16 | Observable-only caps | Attach enforces turn/fix counts, artifact bytes, wall time; token/cost/tool-call metering requires managed launch with a provider telemetry side-channel (or PTY/ConPTY proxy); never accept agent self-report as enforcement | Attach cannot observe a user's existing TUI usage | subject 05/07 |
| D008 | 2026-07-16 | MVP = 00–06 | BYO-attach is the MVP tier, milestone at subject 06 (skills are executable protocol clients, not docs); managed launch + release is 07 | Without the skill clients, BYO is a human manually shuttling JSON, not autonomous | §3, subject 06 |
| D009 | 2026-07-16 | Base branch | RESOLVED: fresh branch `interactive-pairing` from `main`. The prior `reliability-observability-refactor` WIP is stashed/parked (not carried forward); its committed tests/schemas/fixtures are **harvested as evidence** (D014) from git history in subject 00 — no code reuse | Human chose a clean base over continuing the throwaway-driver branch; with the Go pivot (D013) there are no Python modules to reuse — only language-neutral test vectors | 00.1/00.2 + `manual-actions.md` |
| D010 | 2026-07-16 | Verifier context independence | VERIFY **blocks until a new same-role session generation attaches** (single locked behavior — no silent proceed-with-downgrade). The incumbent pair generation is invalid for VERIFY. BYO reports `fresh-session-declared` (session generation enforced; model-context freshness is not); managed reports `fresh-process` only when it truly launches one. Allowing a downgrade requires a later explicit decision/gate | A long-lived attached pair terminal is not context-fresh; the README's fresh-verifier promise cannot silently carry over to attach, and a nondeterministic "refuse-or-label" outcome is not a spec | subject 03.8, 06 skills, 07 labels |
| D011 | 2026-07-16 | Legacy in-flight run compatibility | Pre-pivot active `state.json`/`current` runs are NOT silently migrated into attach semantics. Bump the state major version; behavior is: inspect/export allowed, `resume` fails closed with remediation, unless a proven converter is written | 01.7-style field migration could otherwise reinterpret an in-flight headless run as an attach run and corrupt it | 00.8, 01.7, §1 risks |
| D012 | 2026-07-16 | Reuse posture — SUPERSEDED by D014 | Original: cherry-pick Python modules by evidence from `main` + refactor history. Superseded once the language changed to Go (D013): there are no Python modules to port into a Go binary. Retained for history | Superseded same day by D013/D014 | see D013, D014 |
| D013 | 2026-07-16 | Language: Go | Implement claudex in Go, not Python. The existing Python implementation (main v2 + refactor branch) is discarded — reference-only in git history. Targets: `windows/amd64`, `linux/amd64`, `linux/arm64`, `darwin/arm64`+`amd64` from one codebase | Human wants a distributable multi-machine tool (incl. Linux), is not a Python maintainer (the app was AI-generated), and the tool's shape — a processless coordinator around files/git/child-processes — fits Go's single-binary distribution, typed contracts, concurrency, and native-git shell-out. Rust rejected as over-ceremony for an I/O-bound state machine + Windows MSVC-linker friction. Startup latency was NOT a deciding factor (once-per-turn commands) | all subjects; §2; §6 |
| D014 | 2026-07-16 | Greenfield core + evidence harvesting | Rederive every implementation fresh in Go. Do NOT transplant old code. Reuse the *language-neutral assets* from the refactor-branch history as **executable requirements**: JSON schemas, adversarial fixtures, golden outputs, and the 10 test modules + 6 fixtures / crash-cut scenarios — turned into Go test vectors. Subtle areas (git tree identity, process cancellation, redaction, usage normalization) are pinned by the OLD TESTS + counterexamples, not the old code | Human's concern: porting broken code loops into fixing it instead of building right. The tests/schemas encode the hard-won failure modes without the broken implementation; harvesting them prevents rediscovery of known bugs while keeping the rebuild clean | 00.2 (harvest), all subject IAs |
| D015 | 2026-07-16 | Platform + filesystem support | Support `windows/amd64` + `linux/amd64` **first-class** (both are release gates, not availability deferrals; macOS/arm64 best-effort). OS primitives (advisory locks that die with the process, file replace — atomic on POSIX, recoverable-via-immutable-generations on Windows per D017 — process-tree kill; PTY/ConPTY only if telemetry needs it) live behind tiny **capability-split** build-tagged packages (`internal/oslock`, `internal/atomicfile`, `internal/proctree`, optional `internal/pty`), integration-tested on real Windows + Linux — Go does not unify them. Coordinator state runs only on **local filesystems**, classified `supported-local | known-unsupported | unknown`: SMB/NFS and detectable sync roots are `known-unsupported`; arbitrary third-party sync roots (OneDrive-like) that can't be detected are `unknown` (policy: refuse by default or explicit acknowledgement) — **no claim of perfect detection**. Classification is implemented with the state substrate (01.8) and enforced at bootstrap (03.1); `doctor` (07) only reports it | Codex flagged that Go gives OS access not OS-uniformity, that lock/rename silently break on network/sync FS, that detection can't be perfect, and that enforcement must precede the MVP not follow it | 00 (research), 01.8 (classifier), 03.1 (enforce), 04 (proctree), 07 (report/launch); §1 risks |
| D016 | 2026-07-16 | Run input + policy contract | First `attach` resolves a versioned **task-contract file** + a **config/run-policy** (test_command, observable caps, timeouts, evidence limits, base/repo policy), validates them, hashes/copies them into the run dir, and freezes the effective policy into run state so live edits can't change a running run. `internal/config` owns parse/validate; `internal/state` persists. `init` is optional sugar; the input source is mandatory | CX checkpoint flagged the harvested `TASK_CONTRACT` was orphaned and `test_command`/caps/timeouts had no owner before state was shaped | 01 (config+state), 03.1 (bootstrap freeze); `docs/decisions.md` |
| D017 | 2026-07-16 | Immutable-generation state persistence | Authoritative run state is never overwritten in place; it is an append-only sequence of immutable, checksummed generation files. A torn new generation is invalid-by-checksum and ignored; recovery enumerates generations and picks the highest valid (a `current` pointer is an untrusted optimization). The journal is itself a generation record, so it never depends on the one replace whose failure it diagnoses. Fail closed if no valid generation | CX flagged that "the journal recovers Windows torn writes" is circular — the journal must itself be persisted non-circularly, and Windows replace is not atomic (D015) | 01.1/01.5/01.6; `docs/decisions.md` (**amended M3(B), 2026-07-18**: recovery is rooted on a prunable immutable structural certificate, not generation 1 — run-lock `PruneKeep` bounds the chain; see the D017 amendment) |
| D018 | 2026-07-17 | Transport durability + canonicalization posture | Restricted canonical JSON (safe-integer JCS + strict parser), ONE embedded-schema source for both provider instruction and coordinator validation; immutable content-addressed artifacts published NO-CLOBBER via hard-link (never a replace, so racing publishers can't overwrite and the non-atomic Windows rename is avoided); ONE authoritative acceptance fact (accepted `Phase` per turn; role/type derived from `TurnSpec`; bound to the exact live current-revision turn); the `.claudex/mailbox.md` mirror is a rebuildable, re-validated projection (atomically replaced, never appended) | Make an unsafe transport state unrepresentable rather than merely validated — no cross-language digest instability, no artifact clobber race, no redundant-fact disagreement, no unreconstructable mailbox | subject 02 (transport); `docs/decisions.md` |
| D019 | 2026-07-17 | Provider usage-window auto-resume | On a provider rolling-usage-limit signal, pause durably in `rate_limited`/`paused_budget` with a FROZEN reset/resume-at timestamp and auto-continue to the same turn when the window passes (no human action); processless (D002), so resume is a scheduled/next-poll transition, not a running timer; `status` projects the reset time, reads never resume | A long unattended two-TUI loop must survive hitting a usage limit, not die at it; carries the retired Python reliability engine's intent forward as a requirement, not ported code (D014) | subject 05 (policy + auto-resume transition), 07 (managed tier); `docs/decisions.md` |
| D020 | 2026-07-17 | Verification scope expansion is a convergence blocker | VERIFY treats `scope_expansion` as a convergence blocker (alongside an unmet criterion, non-meaningful tests, and unsupported claims), not an informational field. No human decision: `fail`+scope → FIX; `pass`+scope → contradictory (`ErrSemantic`). Human decision gates first, after schema/identity/exact acceptance-criteria coverage. Convergence: criteria cover `TaskContract.AcceptanceCriteria` exactly once in canonical order; `pass && no blockers` → DONE, `fail && blockers` → FIX/gate, `pass && blockers`/`fail && no blockers` fail closed | CX design-review of checkpoint 4: scope expansion is neither informational nor an unconditional semantic reject; it is a blocker with the human-decision XOR preserved | 03 (engine terminal graph); `docs/decisions.md` |
| D021 | 2026-07-22 | CLI command surface — 5 BYO verbs now, gates/operator deferred | The `attach`/`pull`/`submit`/`wait`/`status` CLI (named deliverable of subjects 02+03, built LIBRARY-ONLY with zero callers — `findings.md` M9, subject-03 03.9) is wired now over the existing library, closing M9 and 03.9's command-surface test. `gates`/`operator` are NOT built in this effort — they have no library API (`internal/gates` does not exist) and remain a **subject-05 closure dependency**; 03.9's exact-surface test is amended to "attach/pull/submit/wait/status + inspect-legacy, gates/operator on 05." Design pins (CX mailbox TURN 200): ONE coordinator-owned aggregate read authority (lock-free bracketed coherent State+Registry snapshot classifying pair/replacement/commit-txn journal heads; recovery-required on any nonterminal aggregate txn — no torn cross-store view); caller-stable `--operation-id` for mutating attach modes (idempotent lost-response recovery); `pull` delivers to the role-addressed session inbox + `submit` rebuilds the `.claudex` mailbox mirror (closes the `SessionStore`/`MailboxStore` zero-caller half of M9); a schema-valid wire receipt (`receipt.v1.json`, not raw `state.Receipt`); `--task`/`--config` through `internal/config` (D016) | David chose to build the real BYO product surface now rather than defer it; the Hindsight requires an explicit decision to re-scope 03.9, and CX ruled the read path must be a single aggregate authority, not independent store reads | 02 (M9/CLI verbs), 03.9 (command surface), 05 (gates/operator), 06 (skills consume this CLI); `cmd/claudex`, `internal/session`(new)/`coordinator`, `internal/transport` mailbox; `docs/decisions.md` |

---

## 5. Master progress tracker

| Done | # | File | Status | Owner summary | Human actions mirrored? |
|---|---|---|---|---|---|
| [x] | 00 | `tasks/interactive-pairing/00-tooling-research-and-readiness.md` | DONE | agent: 8; product-owner: 1 | yes |
| [x] | 01 | `tasks/interactive-pairing/01-durable-state-core.md` | DONE | agent: 10 | n/a |
| [x] | 02 | `tasks/interactive-pairing/02-transport-protocol.md` | DONE | agent: 6 | n/a |
| [ ] | 03 | `tasks/interactive-pairing/03-attach-and-phase-engine.md` | TODO | agent: 9 | n/a |
| [ ] | 04 | `tasks/interactive-pairing/04-git-transaction-and-review-evidence.md` | TODO | agent: 6 | n/a |
| [ ] | 05 | `tasks/interactive-pairing/05-human-gates-and-caps.md` | TODO | agent: 7 | n/a |
| [ ] | 06 | `tasks/interactive-pairing/06-agent-skills-and-byo-integration.md` | TODO | agent: 7 | n/a |
| [ ] | 07 | `tasks/interactive-pairing/07-managed-launcher-and-release.md` | TODO | agent: 7; release-engineer: 3 | yes |

---

## 6. Cross-cutting principles

1. KISS · YAGNI · CLEAN · SOLID · DRY (in that order when conflicting).
2. *(Go)* `gofmt`-clean is mandatory; package names are lower-case, no stutter (`state.New`, not `state.StateNew`).
3. *(Go)* Errors are wrapped with context (`%w`) and handled explicitly; no silent drops. Exported symbols are documented.
4. *(Go)* `go vet ./...` clean; `go test -race ./...` clean in capable CI (no data races in the concurrent wait/lock paths).
5. **Keep code modular and locally understandable.** Small cohesive modules with explicit boundaries; split files before they become dumping grounds; split functions when branching/nesting/mixed responsibility hurts scanning.
6. **Cyclomatic complexity stays low.** Prefer guard clauses, extracted decision helpers, table-driven cases over deep nesting. Double-digit complexity is design pressure to simplify.
7. **Spec at the contract level, not the SDK level.** State testable contracts on observable behaviour (state transitions, artifacts, git tree identity), not internal call shapes.
8. **Coverage % is a smell-detector, not a goal.** Each test pins observable behaviour; if you can't name the bug it prevents, delete it.
9. **Every plan box has an owner and a stable ID** from the §5 enum.
10. **Lessons land in `tasks/interactive-pairing/lessons.md` as they happen**, migrating durable ones to `tasks/lessons.md` at §7.
11. **Code and public metadata are plan-agnostic.** No plan/box/decision refs in comments, identifiers, commits, branches, PRs. Design rationale goes in an ADR, not a plan ref.
12. **Captain Hindsight review before subject close.**
13. **Tooling research before implementation** (subject 00) unless waived by a §4 decision.
14. **All plans are resume-safe** — a ticked box is backed by a durable checkpoint (plan update + verification/blocker + commit + push, no-remote exception).
15. **Parallelism is opt-in** — this plan is `coordinated`, not `parallel`.
16. **Documentation ships with the change** — README (rewrite for Go/attach), `docs/architecture.md` + `docs/decisions.md` (**created** for the Go era — they do not exist on `main`, only in refactor history), CHANGELOG (**created**), and skill docs, in the same checkpoint or `n/a` + reason. Never leave a broken Python install command in README after 00.3 removes Python.
17. **Reuse contracts + vectors, integrate within the Go module (D014).** Greenfield rebuild — no old *code* to extend. "Reuse" = satisfy the harvested schemas/fixtures/test vectors (00.2) instead of re-inventing behaviour, and land each change in the correct existing `internal/*` package rather than a duplicate parallel one. Each subject's `## Integration analysis` names its Go package + the vectors it satisfies before any box. New code is expected (nothing to port); duplicated Go packages/logic are not. Wire or delete new public surface.
18. **No transition without CAS.** Every state mutation is an atomic compare-and-swap against the expected `state_revision`; torn or unguarded writes are blockers. Artifacts are written temp → fsync → rename before the state/ledger advances.
19. **Immutable evidence, never the live worktree.** Reviews, diffs, and convergence checks read committed/immutable artifacts identified by hash. The mutable worktree is never a review input; post-snapshot edits are surfaced as protocol violations, never silently absorbed.
20. **Honesty labels — never label a capability enforced without a mechanism.** BYO attach is `protocol-only`; managed capabilities are labeled individually (`sandboxed`, `telemetry-observed`, …). Agent self-report is never enforcement. Every cap/label in `status` names the mechanism that backs it.
21. **Two-store operations are journaled + reconciled.** Any operation spanning git and coordinator state writes a prepared-transaction record first and is replayed or rolled back on startup; the code never assumes the two stores committed atomically.
22. **Greenfield, not ported (D014).** No line of the old Python is transplanted. Reuse enters only as harvested *test vectors / schemas / fixtures*; every implementation is rederived in Go against those vectors. A subtle behaviour is pinned by a harvested counterexample test before it is coded.
23. **OS primitives are build-tagged and integration-tested (D015).** Locks, file replace, and process-tree kill live in tiny per-OS packages (`_windows.go` / `_unix.go`) behind one interface, each proven on real Windows + Linux, never assumed uniform (Windows process-tree kill uses create-suspended → assign-job → resume, not best-effort). PTY/ConPTY (`internal/pty`) is the same discipline **but conditional** — built and integration-tested only if 07.3 adopts a telemetry proxy; if not built, it has no gate.
24. **Shell out to native git, never a library (D013).** Git operations use `exec.CommandContext` with argv (never a shell string), parse only machine formats (`-z`, object IDs), preflight a minimum git version, and use plumbing (`read-tree` + temp `GIT_INDEX_FILE`, `write-tree`, `commit-tree`, `update-ref <new> <old>` for CAS). No go-git.
25. **Canonical bytes for identity (D014).** Any digest/idempotency/receipt is computed over canonical JSON (RFC 8785 or a restricted equivalent) — never a serializer's incidental map ordering or whitespace. The same versioned schema bytes are embedded in the binary and used for both provider instruction and coordinator validation.
26. **Local filesystem only (D015).** Coordinator state requires a local filesystem, classified `supported-local | known-unsupported | unknown`. **Known-unsupported** (SMB/NFS + detectable sync roots) is rejected; **unknown** (undetectable third-party sync roots) follows the explicit acknowledgement policy — no claim of perfect detection. Never silently trust a non-local location.
27. *(plan-specific principles above are 18–26; keep numbering stable)*

---

## 7. Gate review (run last; tick everything)

- [ ] All §5 subjects done (or explicitly `ABANDONED` with a §4 row)
- [ ] Subject 00 completed, or explicitly waived/abandoned with a §4 row
- [ ] `go build ./...`, `go vet ./...`, and `gofmt -l .` (clean) from §2 pass on the target matrix
- [ ] `go test ./...` passes; `go test -race ./...` passes in capable CI; the two-session attach e2e (subject 06) passes
- [ ] Platform integration tests (locks, file replace, process-tree kill) pass on real Windows + Linux; PTY/ConPTY tests too **only if** `internal/pty` was built by 07.3 (D015)
- [ ] Behaviour verification done for every runtime-behaviour box — the changed flow driven end-to-end with observed-vs-expected output recorded (canonical: two attached sessions complete a real turn)
- [ ] Remaining §2 rows (lint/format, plan-specific gates) pass or recorded `n/a`
- [ ] §1 Risks-and-rollback table reviewed; rollback steps still accurate for what shipped
- [ ] §6 principles reviewed; CAS/immutable-evidence/honesty-label/journal rules hold in shipped code
- [ ] Every implementation subject recorded an `## Integration analysis`; no unjustified duplication
- [ ] New public surface wired to a non-test caller, or disclosed library-only/unwired
- [ ] Every non-abandoned subject has a Captain Hindsight verdict `CLOSE`
- [ ] Every ticked box has a Progress-log entry + pushed checkpoint commit (or no-remote note)
- [ ] Durable architecture decisions (D001–D020, incl. the D017 M3(B) amendment, + protocol ADR) promoted to `docs/decisions.md`/ADR
- [ ] Documentation-impact review done — README + architecture/decision docs + skill docs match shipped behaviour, or `n/a` + reason
- [ ] Current branch pushed (or no-remote note); `git status --short` clean except deferred/ignored
- [ ] Shipped code/tests/comments/identifiers plan-agnostic — grep excluding `tasks/` for box IDs (`\b\d\d\.\d+\b`), decision IDs (`\bD\d{3}\b`), `tasks/interactive-pairing/`, `InteractivePairing-Plan.md`, `\bslices?\b`; zero hits after triage
- [ ] Commit messages plan-agnostic — `git log <base>..HEAD`
- [ ] Branch names / PR titles plan-agnostic if used
- [ ] `manual-actions.md` — every human-owned box resolved or explicitly deferred
- [ ] No Python remains in the shipped tree (the old implementation is git-history reference only, D014); the repo builds as a pure Go module
- [ ] Distribution addressed (07): cross-build matrix (windows/amd64, linux/amd64, linux/arm64, darwin) + checksums + signing/SmartScreen decision recorded; local-filesystem-only preflight shipped (D015)
- [ ] Managed-launch capabilities are individually labeled in `status`; no blanket "enforced" bit shipped
- [ ] `cleanup-audit` teardown review run before sign-off, or waived by a §4 row; findings triaged (fix-now / new plan / won't-fix)
- [ ] `tasks/interactive-pairing/lessons.md` reconciled; lasting lessons migrated to `tasks/lessons.md`
- [ ] Plan handed to reviewer for §8 sign-off

---

## 8. Acceptance / sign-off

| Date | Reviewer | Result | Notes |
|---|---|---|---|
| | | | |

---

## Appendix: Captain Hindsight Prompt

```text
You are now Captain Hindsight.

Review the completed subject, phase, box, or major plan section with hindsight.
Assume the work is already done, then identify what is clearer now than it was
before the work started.

Check specifically for:
- Scope drift or missed requirements.
- Spec deviations that need a Decision-log row (and whether durable enough to promote to an ADR / docs/decisions.md).
- Lessons that should be recorded before context is lost.
- Tests that pin implementation details instead of observable behavior.
- Complexity, duplication, brittle design, or awkward naming that should be fixed now.
- Behaviour closed on a green build alone: a box marked done without the changed flow exercised end-to-end (two attached sessions completing a real turn).
- Reuse missed or vision drift: a harvested test vector/schema (00.2) ignored and behaviour re-invented; a duplicate `internal/*` package where an existing one should have been extended; or Python code transplanted rather than rederived in Go (D014 violation).
- A capability or cap labeled "enforced" without a real mechanism backing it (honesty-label violation).
- A git+state operation that assumed atomicity instead of journaling + reconciliation.
- Human-owned actions that need to be mirrored or resolved.
- Docs that drifted from shipped behaviour (README, docs/architecture.md, docs/decisions.md, skill docs).
- Plan references that leaked into shipped code, tests, comments, identifiers, commit messages, branch names, PR titles/descriptions.

Before returning a verdict, confirm the claimed verification against the
artefacts: rerun the relevant verification command or cite its recorded output.

Return exactly these sections:
1. Keep: what was correct and should remain.
2. Fix before closing: concrete issues, missing tests, spec drift, plan hygiene, or design problems — cite the file, test, or commit for each item.
3. Record: decisions or lessons that must be added to the plan files.
4. Risk: anything still uncertain after verification.
5. Verdict: CLOSE or DO NOT CLOSE.

If the verdict is DO NOT CLOSE, list the smallest concrete actions needed before
the work can be closed.
```
