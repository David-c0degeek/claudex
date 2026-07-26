# Review findings — `interactive-pairing` branch

**Date:** 2026-07-17 · **Scope:** full branch review vs `main` (157 files, ~34k insertions; Go coordinator rewrite, subjects 00–02 DONE, 03 in flight) · **Posture:** review only, nothing changed.

**Method.** Ten package-focused review passes (six completed by independent review agents; four areas — OS primitives, canonjson/protocol/redact, transport verbs, attach — re-reviewed directly after agent interruptions), plus independent verification of external claims (RFC 8785, Windows file/lock semantics, Go `os.Root`), plan/doc cross-checks, and mechanical evidence collected fresh on this machine.

**Mechanical evidence (all reproduced during this review):**

| Check | Result |
|---|---|
| `go build ./...` / `go vet ./...` / `gofmt -l .` | clean |
| `go test ./...` (17 packages) | pass |
| `go test -race -count=1 ./...` | pass (~2.5 min) |
| Cross-compile `linux/amd64`, `linux/arm64`, `darwin/arm64` | build clean |
| Transport/attach flake hunt (8× normal + 3× race + full-suite runs) | no flake reproduced |
| `git ls-files '*.py'` | empty — no Python in tracked tree |
| go.mod | `go 1.25.0`, single dep `golang.org/x/sys v0.47.0` |

**Status.** Tracked from 2026-07-26. This is the branch review register: findings are struck through and given a dated resolution note in place rather than deleted, so what was found and what was decided about it both survive the fix. M9 and M10 are resolved below; the majors that drove design changes are cross-referenced from the subject progress logs.

**Overall verdict.** The core engineering is unusually rigorous: CAS discipline, fail-closed validation, injection-safety, and crash-cut reconciliation are real and mostly well-tested, and many subtle claims in commit messages check out under adversarial reading. The serious problems cluster in four places: (1) **Windows durability is weaker than D017 advertises** (no directory fsync / write-through on the commit path, plus a swallowed post-commit sync error on POSIX); (2) **the artifact store's hard-link publish has no filesystem-capability guard** while the classifier approves hardlink-less filesystems; (3) **two invariant tests are vacuous**, leaving validator branches with zero effective coverage; and (4) **docs and plan hygiene have drifted** (present-tense docs for unbuilt subsystems, a stale plan decision log, a DONE subject whose named CLI deliverables don't exist, and pushed plan-referencing commit messages that make a §7 gate item unsatisfiable as written).

---

## Major

### M1. Committed-but-unsynced generation append is silently reported as success
`internal/genstore/genstore.go:287-306`
When `atomicfile` returns a `*PostCommitSyncError` (rename visible, directory fsync **failed**), genstore's reconcile path sees the head matches the candidate and returns `nil` — the durability failure is dropped. On POSIX, a journal-prepare append can hit a dir-fsync EIO, txn proceeds to apply the external effect (eventually the git ref move), power loss unlinks the never-synced directory entry, and restart finds an external effect with no journal record — exactly the orphaned-effect ambiguity D006/D017 exist to remove. The codebase already has the right vehicle (`genstore.PostCommitError` with `Committed()=true`, used for lock-release failure at `genstore.go:222`) but doesn't use it here. Worse, `genstore_test.go:217-234` (`TestWriteCommittedDespitePostCommitSyncError`) pins the swallowing as desired behavior.

### M2. Windows generation commit is not power-loss durable, enabling silent generation-number reuse
`internal/atomicfile/atomicfile_windows.go:68-70` (consumed at `genstore.go:275`)
`syncDir` is a no-op on Windows and `os.Rename` does not request `MOVEFILE_WRITE_THROUGH`, so an acknowledged append can vanish at power loss. The lost slot is then **empty** (not torn-quarantined), so the next append reuses the *same generation number* with a different payload/digest — silent history rewrite relative to anything that recorded the old head (receipts, ledger). D017 is the root of trust whose stated purpose is Windows crash safety; the known fix is `FlushFileBuffers` on the file handle post-rename or `MOVEFILE_WRITE_THROUGH`. (File-*content* fsync is done on both platforms; the gap is the directory entry, Windows only.)

### M3. Recovery demands an unbroken chain from generation 1, contradicting D017 and blocking pruning
`internal/genstore/genstore.go:375-377`
`if valid[0].Generation != 1 || valid[0].PrevDigest != ""` → `ErrCorrupt`. D017 says recovery "selects the highest-numbered generation whose checksum validates" and mandates pruning under the run lock; this implementation (a) makes any future prune that removes gen 1 brick the store permanently, and (b) means single-record bit-rot in an *old* ancestor fails the store closed even though a perfectly valid head exists. With no prune implemented and the journal appending N+1 records per transaction, every run's chain grows without bound — and `internal/txn` re-scans and re-hashes the full chain after every step (`txn.go:292-306`), giving O(n²) cumulative I/O over a run's life.

### M4. Artifact publish requires hard links; nothing checks the filesystem supports them, and fsclass approves ones that don't
`internal/transport/artifactstore.go:84` + `internal/fsclass/fsclass_windows.go:108-109` + `internal/fsclass/fsclass_darwin.go:26-27`
Publication is hard-link no-clobber (`root.Link`). Neither `NewArtifactStore` nor bootstrap probes link support, and the classifier blesses hardlink-less filesystems as `SupportedLocal`: Windows classifies by `GetDriveType` only (any `DRIVE_FIXED` passes — FAT32/exFAT fixed volumes included; no filesystem-name check), and darwin explicitly lists `exfat`/`msdos` as supported-local. A repo on such a volume passes preflight, then **every submit fails permanently** at `Sink.Put` with a raw OS error and no remediation. Fix direction: probe link support at bootstrap (create+link a temp pair) or exclude non-hardlink filesystems in fsclass.

### M5. Run-policy limits have no upper bound; a huge `max_wall_seconds` bricks the run at pair-join
`internal/config/config.go:281-296` → `internal/attach/pair.go:374`
Validation only checks `> 0` for all seven limits. `{"max_wall_seconds": 9223372036854775807}` validates and freezes; `preparePair` computes `DeadlineUnix: req.Now + MaxWallSeconds`, which wraps negative; state validation (`statevalidate.go`) then rejects every `JoinAttach` forever — the bootstrapped run dir must be abandoned. Same unboundedness for `test_timeout_seconds` and the `evidence_max_*` family. Related minor: `max_artifact_bytes_per_submit` may exceed the hard 1 MiB pipeline ceiling (`canonjson.maxSize`, `artifactstore.maxArtifactBytes`), freezing a policy the transport can never honor.

### M6. Two state-invariant tests are vacuous — they pass via CAS conflict, not the validator under test
`internal/state/statev5_invariant_test.go:112, :159`
Both tests mutate from a *stale* revision, so `Mutate` returns `ErrRevisionConflict` before the target validator ever runs, and the `rejects` helper (`:13`) accepts any error. Deleting the cursor upper-bound check (`statev5.go:196-198`) still passes the suite; the `step_index > StepCount` rejection and the VERIFY context-transfer rules (`statev5.go:643-652`) have **zero effective coverage**. The helper should fail on `ErrRevisionConflict` (or assert the rejection reason).

### M7. Shipped docs describe unbuilt subsystems in present tense
`docs/architecture.md:92-109` ("Git transaction") and `:111-121` ("Two tiers")
The git snapshot/commit/ref-CAS/index-sync/evidence-packet pipeline and the enforcement list ("commit ancestry, worktree cleanliness, exact diffs, tests, merge gating") plus managed launch are written as shipped behavior. No `internal/gitx`, `internal/evidence`, `internal/proctree`, `internal/launch` exist; subjects 04/07 have zero boxes ticked. A reader of the shipped doc is materially misled. Mark future sections as planned, or move them to the plan.

### M8. Plan decision log is stale: D018/D019 missing from §4, yet real and load-bearing
`tasks/InteractivePairing-Plan.md:138-139` (jumps D017→D020) vs `docs/decisions.md:201` (D018 — transport durability posture) and `:242` (D019 — usage-window auto-resume)
Code comments citing D019/D020 reference real ADR entries — the **plan table** is the stale side, meaning two decisions bypassed the plan's own checkpoint rule ("update any affected §4 decisions"). §7 gate line 205 still reads "D001–D016 + protocol ADR", also stale. Add D018/D019 rows and fix the gate line.

### M9. ~~Subject 02 is DONE but its named deliverables (CLI verbs) don't exist; unwired surface undisclosed~~ — **RESOLVED 2026-07-26**
`tasks/interactive-pairing/02-transport-protocol.md:27-29` vs `cmd/claudex/main.go:44-68`
Ticked boxes name `submit --file`, `wait --timeout N`, `status` subcommands; the CLI dispatches only `version`/`help`/`inspect-legacy`. `transport.Wait`, `transport.Status`, `transport.BuildAssignment`, `transport.SubmitTestOutcome`, the mailbox mirror, and the session inbox have **zero non-test callers** (only `transport.Submit` is wired, via `internal/coordinator` — itself entirely unwired). Subject 03 slice h disclosed its library-only surface; subject 02's Captain Hindsight did not. The §7 "wired or disclosed" gate item is currently false for a DONE subject. Notably, `.claudex/mailbox.md` is never written in any shipped path.

**Resolution (D021, subjects 02/03.9, 2026-07-26).** The CLI was built over the existing library and every surface listed above now has a non-test caller: `attach`/`pull`/`submit`/`wait`/`status` are dispatched, the mailbox mirror and the session inbox are written on shipped paths, and `pull` — which needed the review-evidence packet to exist and therefore waited on subject 04.3 — landed last. Subject 02's Hindsight was amended in place to disclose that it had closed on an undisclosed library-only surface, rather than being quietly re-ticked, and the lesson is recorded in `tasks/interactive-pairing/lessons.md`. `gates`/`operator` remain deliberately unbuilt and are a subject-05 dependency (D021), which the exact-surface test asserts rather than assumes.

### M10. ~~~12 pushed commit subjects reference the plan, making the §7 "commit messages plan-agnostic" gate unsatisfiable as written~~ — **RESOLVED 2026-07-26 (D022)**
`git log main..HEAD`
Offenders include `3fdd36a`, `8dad0a0`, `4871f29` ("…subject progress log"), `2133c9f`, `e679ced`, `a6a33e2` ("…plan log"), `2ecf499` ("Close the transport-protocol subject" — touches `internal/`), `3f10076`, `0ef2869`, `369f4d4`, `2047b61`, `aa79690`, `dfed322`; `5b05bed`'s body cites D019 and a subject-05 box. The plan forbids rewriting pushed history, so the gate needs either a triage rule (e.g. plan-file-only commits exempt) or an accepted-violation decision row — decide now, not at gate time. (`82e2bfc` "Bind the journal to its plan" is a false positive — txn step-plan concept.)

**Resolution (D022, 2026-07-26).** Decided as this finding demanded — now, not at gate time. Commit SUBJECTS are plan-agnostic going forward; BODIES may cite decisions and boxes, because traceability from code to rationale is worth more than purity and the body is not what `git log --oneline` shows. Already-pushed subjects, including the later 04.5 series, are an **accepted violation**: the plan forbids rewriting pushed history and the peer reviewer has reviewed those exact SHAs. The related shipped-code item turned out to be mostly a **bad pattern rather than dirty code** — the gate's `slices?` matches Go's own `slices` package and `slice` type, and `\d\d\.\d+` matches `git version 2.54.0`; only two genuine references existed and both were removed. The branch name is an accepted violation for the life of the branch, with the PR title and merge commit required to be clean.

---

## Minor

### Durability / stores
- `genstore.go:275` — generation publication is rename-replace, not O_EXCL/no-clobber; the advisory lock is the *only* defense against slot clobber (`checkGuard` compares only the lock-path string). Transport uses hard-link no-clobber for exactly this class; genstore has no second line.
- `genstore.go:155-157` — the store dir's own creation is never fsynced in its parent, and an absent dir is defined as a healthy empty store; a POSIX power loss shortly after first appends can make "lost run" indistinguishable from "never bootstrapped" (silent re-bootstrap; D017's fail-closed can't trigger).
- `genstore.go:218-225` — on the append-failure path a lock-release failure is silently dropped (stuck lock → later `ErrBusy` with no diagnostic).
- `internal/state/catalog.go:206` — `decodeCatalog` lacks the loose schema-version probe every other store has (no `ErrUnsupportedCatalogSchema`); a future catalog version fails with a vague unknown-field error instead of upgrade remediation, contrary to the documented posture.
- `internal/state/catalog.go:145` vs `:114` — `Lookup` is case-sensitive while allocation dedupes case-insensitively (`EqualFold`); `Lookup("RUN-A")` misses `run-a` that allocation would refuse to duplicate.
- `internal/state/grammar.go:51-55` — "a failure lifecycle requires a failure projection" is documented but enforced only on the transport submit path (`submit.go:710-711`), not in the state validator; any other mutator can persist an absorbing `failed_terminal` with `Failure == nil`, which is then unrepairable.
- `internal/state/statevalidate.go:114` — an outstanding Assignment is not restricted to agent phases / running lifecycle at the state layer (tests rely on the looseness); consumers trusting durable state alone can see a live-looking turn on a dead run.
- No sweeper anywhere for stranded `.claudex-tmp-*` files (crash between temp write and link/rename); unbounded accumulation in artifact/store dirs.

### Transport / protocol
- `internal/transport/submit.go:55-57` — run-lock acquisition budget is ~12.8 ms total (64 × 200 µs), smaller than one slow-disk fsync; a concurrent submit whose winner's guarded section exceeds it gets a spurious `ErrBusy`. Passes today at SSD/tmpfs speed only.
- `internal/transport/assignment.go:234` — `validateSemantics` grammar-checks `session_id` but only non-emptiness for `run_id`/`turn_id`; a tampered inbox with `turn_id: "../x"` fully validates at the read boundary (schema bounds length only).
- `internal/transport/assignment.go:147-173` — `BuildAssignment` never checks lifecycle/recovery/gate; if a generation ever carries a current-revision assignment plus a paused/recovering lifecycle, pull delivers and submit refuses forever (pull→work→reject loop with no signal).
- `internal/state/statevalidate.go:632-645` — `validRunID` (artifact-dir key grammar) permits uppercase, trailing dots, and Windows reserved device names (`NUL`, `COM1`), which `os.Root` does not block on Windows; case-aliased turn ids collide on NTFS (platform-divergent spurious `ErrArtifactCollision`). Contrast `IsSessionID`, which is lowercase-only for exactly this reason.
- `internal/transport/assignment.go:195-203` — the "exactly one of Worktree or Evidence" precondition is silently resolved, not enforced; a driver passing both is masked.
- Mailbox/inbox docs and comments say "atomically replaced" (`mailbox.go:40,76`; D018) while the same repo's atomicfile doc correctly states Windows makes no old-or-new promise — the guarantee is overclaimed on a first-class platform (bounded: both files are rebuildable projections and the inbox read fails closed).
- Test gaps: no Get-side tamper test in `artifactstore_test.go` (dropping `verify` from `Get` passes the suite); duplicate-turn and malformed-ledger branches of `RenderMailbox` uncovered; `Normalize` negatives limited to `{not json` (the redaction-breaks-JSON path at `submit.go:229` is never exercised).

### Engine
- `internal/engine/project.go:216,274` — unknown/miscased severities are fail-open in the projection (`"Blocking"` ≠ `"blocking"` → non-blocking); convergence's deciding enum is protected only by upstream schema validation, unlike the fold which re-validates (`materialize.go:141`).
- `internal/engine/engine.go:446-457` — `evalTestsOutcome` ignores `ev.Decision` instead of rejecting; a fabricated `{EvTestsOutcome, Pass:true, Decision:true}` advances to ownerless VERIFY rather than failing closed. Untested.
- `internal/engine/engine.go:659-662` — the `IDGate` branch of `validateApply` omits the reuse checks the `IDAssignment` branch has; only transport's `recheckIssuedIDs` catches a gate id colliding with an accepted turn.
- `internal/engine/engine.go:1045-1104` — Apply's payload validation is shape-only (digests never checked against materialization); a forged well-shaped Decision persists a candidate matching no artifact, after which `verifyCandidateFacts` wedges every later submit — fail-closed but run-bricking, contrary to the "fully validates before any write" comment.
- D010's freshness *check* lives entirely in transport/attach; the engine encodes only the threshold value, and its own tests issue the verifier via a raw `store.Mutate` with no generation validation (`engine_terminal_test.go:71-82`).
- Transition-matrix testing is sampled, not enumerated. Notable untested cases: human-gate Apply+persist from PLAN_DRAFT/PLAN_CRITIQUE/PLAN_REVISE/CHECKPOINT; FIX gate with `FixReturn` TESTS/VERIFY; `ErrBudgetCorrupt` for `TestFixes`/`VerifyFixes`; Apply against a non-live pre-state; `EvTestsOutcome` at a non-TESTS phase; the `RouteGate`-from-non-submit negative the slice-j log *claims* exists but doesn't.

### Attach
- `internal/attach/replace.go:218-229` vs `:240-251` — the same-operation idempotent retry re-verifies the Registry effect but not an activation's RunState effect (the different-operation step-over path does); a tampered/rolled-back state store lets a retry return a `VerifierTurnID` the run never issued.
- `internal/legacy/legacy.go:77` + `cmd/claudex/main.go:102` — unbounded `os.ReadFile` of `state.json` in the bootstrap refusal guard and the inspector; a multi-GB planted file turns the fail-closed guard into an OOM crash (contrast `config.MaxContractBytes`).

### Config / CLI / buildinfo
- `config.go:253-255` — `base_branch` validated only as non-blank; `"--force"` freezes into policy and will reach `git` argv as an option when the D006 shell-out lands (config is the declared owner of base/repo policy). Validate ref-format / reject leading `-` now.
- `config.go:294-296` — no cross-check `test_timeout_seconds ≤ max_wall_seconds`.
- `internal/buildinfo/buildinfo.go:25-32` — an injected commit ignores `vcs.modified`, so a dirty-tree CI build presents a clean commit identity, contradicting the package's own doc; `buildinfo_test.go:39` locks the misleading behavior in.
- `cmd/claudex/main.go:93-98` — `inspect-legacy`'s `UnknownStateError` message is allocation-oriented ("refusing to allocate over it") — wrong verb for a read-only inspect; both directory branches untested.

### Redaction
- `internal/redact/redact.go:44` — the key-value pattern's `\b(...|secret|...)\b` cannot match snake_case compounds: `client_secret`, `secret_key`, `aws_secret_access_key` (underscore is a word character, so no boundary) go **unredacted** in the exact `"key":"value"` JSON form the pipeline handles. Add the common compound keys or match `[A-Za-z0-9_-]*(secret|token|key)[A-Za-z0-9_-]*`.
- `internal/redact/redact.go:27` — `sk-[A-Za-z0-9_-]{16,}` is unanchored, so innocent hyphenated words containing `sk-` over-redact (e.g. `risk-management-plan-v2` → `ri[REDACTED]`). Anchor with a boundary/lookbehind-equivalent.
- No JWT (`eyJ…`), PEM private-key block, Google `AIza…`, or GitLab `glpat-` families. The package honestly documents a heuristic posture, but these are as common as the families it does guarantee.

### Docs / plan hygiene (beyond M7–M10)
- Decision-ID comments in shipped code (`state.go:191`, `statev5.go:122`, `engine/project.go:49`, `engine_terminal_test.go:289`) — the exact class subject 01's close swept, recurred without a lessons entry.
- `docs/evidence-harvest.md` (7 box/subject refs) and `docs/decisions.md:131,188,256` carry plan refs that dangle once `tasks/` is deleted.
- `docs/architecture.md:122-148` package map omits shipped `internal/attach` and `internal/coordinator`; its Subject column cites plan numbers.
- `CHANGELOG.md:43` calls the mailbox mirror "append-only" — contradicts D018 and the code (`ReplaceInRoot`); changelog also ends at subject 02 (~60 subject-03 commits unrecorded) and uses the literal branch/plan name at line 9.
- Plan §5: row 05 says "agent: 7" but the subject has 8 boxes; subject 03 shows "TODO" despite ~28 in-flight slices; `00-…md:6` still says "17 test files" vs the corrected 10+6.
- 00.6 adopted Go fuzzing "for `internal/canonjson`/parsing" — the only fuzz target in the repo is `genstore.FuzzDecode`; the §2 fuzz row will be unmet at gate.
- `.mailbox/` is ignored only via machine-local `.git/info/exclude`, not the committed `.gitignore` — other clones will see untracked noise.

---

## Info (selected)

- `docs/architecture.md:15` names `PAUSED_BUDGET` as a loop state; the code has a single `AWAIT_GUIDANCE` discriminated by `PauseKind` (and `LifecyclePausedBudget` means the *unrelated* D019 usage-window pause). Rename in docs to avoid the trap the `state.go:191` comment defuses.
- AWAIT_GUIDANCE is a dead end inside the engine (gate resolution is subject 05); the "sole legal phase-edge table" framing will need a caveat when the resume authority lands.
- D019 lifecycles (`paused_budget`, `rate_limited`) validate as bare labels — v5 has no resume-at field yet, so `status` cannot project the reset time D019 promises (subject 05 schema bump expected).
- "Strict" JSON decode across stores is only Go-strict: case-insensitive field matching and last-duplicate-wins survive `DisallowUnknownFields` (hand-edited generations only; low risk under the same-user threat model).
- `registryvalidate.go:107` — decode-time slot validation allows equal issued revisions (`<` vs the documented strictly-increase).
- `coordinator.go` — test-only `submitHooks` compiled into the production Submit path (inert); a failed `store.Close()` still marks the run closed (root handle leaks till exit); tests assert the exact 32 RNG bytes (implementation detail); `(*Run).RunLock` exported for tests that don't need it.
- `config.Hash` covers only the raw source document — fine today, but the planned CLI-override wiring would change `EffectivePolicy` without changing `PolicyDigest` (provenance mismatch to design around).
- `txn.go:320-326` — TxnID uniqueness only vs the immediately preceding record (historical id re-run possible; replay protection rests on step CAS semantics, which are sound). `txn.go:466-468` — tail decode error misreported as "trailing content" without `%w`.
- `mustMarshalPair` / `mustMarshalReplace` / `genstore.go:415` swallow marshal errors (unreachable for these structs).
- Windows: case-variant `.GEN` files are silently skipped as "outside the namespace" rather than treated as corruption (`genstore.go:325`).
- One review agent reported (before being interrupted) a "confirmed flake" in transport tests; **14 subsequent runs (normal + race) could not reproduce it**. Most plausible cause was concurrent `go test` processes from parallel review agents colliding in one working tree. Unverified — if a flake resurfaces, look at timing-sensitive lock-budget tests first (`submitlock_test.go` spawns one goroutine; the 12.8 ms budget above is the mechanism most sensitive to load).

---

## Verified sound (load-bearing claims independently checked)

- **canonjson vs RFC 8785 (checked against the RFC):** UTF-16 code-unit key sorting (`utf16.Encode` compare — including the supplementary-plane vs U+FFFF distinguishing case, which the tests pin), minimal escaping with the six two-char escapes + lowercase `\u00xx` for remaining controls, duplicate-key rejection post-unescape, lone-surrogate rejection (raw and escaped), integer-only ±(2^53−1) with leading-zero/exponent/float rejection, −0 collapse, 1 MiB/64-depth bounds, and HTML-escape non-leakage. The restricted profile is delivered as documented.
- **Protocol schema compiler is genuinely strict:** unknown keywords fail compilation (nothing silently ignored), objects must declare `properties` + `additionalProperties:false` + exact `required`, no `pattern` keyword (no ReDoS surface), `number` type excluded, null-as-keyword-value rejected, enum/const compared by canonical bytes (floats fail closed), instances decoded with `UseNumber`, schema identity (filename ↔ `message_type`/`protocol_version` consts) enforced at startup, version preflight bounded with duplicate-key ambiguity handling.
- **Submit accept path (walked line-by-line):** one guard covers authorize→publish→accept; journal head checked before registry; replay is role-authorized before digest compare; staleness→liveness→turn-identity→role→pair-generation→VERIFY-threshold ordering is coherent; both raw and redacted bytes schema-validated; decision XOR complete; issued-ID recheck + post-apply binding + `requireLiveOwner` + `checkEnterVerifyThreshold` (with uint64-overflow guard) close the forged-Prepare shapes; post-`Put` append deliberately completes under cancellation so no half-accepted turn exists.
- **Test-outcome transition is exact as claimed:** closed three-outcome shape (VERIFY/FIX/quality-gate), per-outcome exact counter deltas, before/after ledger equality, gate source bound to the accepted evidence digest, ambiguous-append classified as typed outcome-unknown.
- **Wait is lock-free and bounded:** (0, 1h] timeout, 25→250 ms backoff, ctx-safe, wakes classified with fail-closed contradictions (qualifying-generation-at-ownerless-VERIFY, ownership/current-pair mismatch), closed event-kind validation, every event schema-validated before return.
- **Status honesty labels are enforced, not asserted:** tier↔mechanism binding, `fresh-process` reserved to managed, `fresh-session-declared` anti-laundering, BYO must declare exactly one enforced `verify-fresh-session`, caps arithmetic re-verified, stop fields must already be redacted.
- **State CAS discipline:** all four stores funnel through guarded `AppendLocked` with double CAS; no unguarded write path; rejected mutations write nothing; `Revision == Generation` both ways; builder-poisoning guard; version probes (state/registry/currentrun) fail old/new versions with remediation; legacy Python state rejected (D011); redaction runs before validation so errors never echo secrets.
- **txn journal:** non-circular (persists only via genstore fresh-file appends), Status-observed-before-and-after-Apply reconciliation, idempotent forward repair, fail-closed ambiguity, guard validation before callbacks, intent deep-equal binding on abort.
- **Attach:** fixed repo→run lock order with release-only-acquired-instance discipline; first-attach split-brain closed by repo lock + journaled bootstrap + terminal-pointer clear only inside the new journaled bootstrap; FS classify-and-refuse happens before any mutation and is revalidated under the guard; pair-join revalidates the pristine-INIT shape under both guards; replacement recover-first, head fully bound (envelope reconstruction, step-list match), different-op step-over requires Applied registry effect + intact activation lineage; activation Applied/NotApplied classified by whole-state canonical digest with normalization; reattach uses a deterministic bracketed double-read protocol (injectable reader — race tests are deterministic, not sleep-based); cross-slot session-id uniqueness and distinct lead/pair agents enforced at registry validation.
- **OS primitives:** `flock`/`LockFileEx` locks are kernel-released on crash (correct reclaim story), lock files never unlinked (no unlink race), Go's O_CLOEXEC prevents child inheritance; hard-link no-clobber error mapping correct on both OSes (`fs.ErrExist` never retried into a clobber); `ReadInRoot` refuses symlinks/FIFOs/devices/oversize with Lstat + O_NOFOLLOW + post-open re-Stat; POSIX rename durability ordering (file fsync → rename → parent dir fsync) is correct.
- **Repo hygiene:** no tracked Python; build artifacts ignored; branch fully pushed; module surface minimal (stdlib + x/sys).

---

## Suggested priorities

1. **Fix M1/M2 together** (durability of the root of trust on both platforms) — they're the foundation everything else cites.
2. **M4 + M5** — both are "preflight approves a configuration that then permanently fails"; cheap to fix at bootstrap/validation.
3. **M6** — two-line test-helper fix restores real coverage of already-written validators.
4. **M3** — decide the D017 recovery/pruning semantics now (either implement prune-safe root re-anchoring or amend D017), before the chain-growth cost compounds.
5. **Docs/plan (M7–M10)** — one hygiene checkpoint: future-tense the unbuilt architecture sections, add D018/D019 rows, disclose subject 02's library-only surface, and record a commit-message triage decision.
