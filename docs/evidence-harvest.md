# Evidence Harvest — Go test vectors from the retired Python suite

> Provenance: harvested (read-only) from branch `reliability-observability-refactor`
> (`tests/*.py`, `claudex/schemas.py`, `tests/fixtures/**`) during the Go rewrite.
> These are **executable requirements** for the Go implementation — behaviours to
> re-satisfy with fresh Go tests, NOT code to port. The Python source is
> reference-only in git history.

## 1. Test-file → behaviour → Go-subject map

| Old test file | Behaviour pinned | → Go subject | Notable edge vectors |
|---|---|---|---|
| `test_budgets.py` | Usage normalization (no subset double-counting), config policy migration+validation, provider CLI command construction, budget admission (pause before call, overshoot-then-deny), resume, status rendering of unknowns | **05** caps/lifecycle (+**02** usage schema) | codex `cached_input_tokens` ⊂ `input_tokens` (not added); claude `cache_read`+`cache_creation` ARE added; EUR preserved + requires ack; `record_result` idempotent by `attempt_id`; exact-limit pause charges nothing |
| `test_faults.py` | Persistence atomicity, malformed-state rejection, stdout-reader failure non-deadlock, unique per-retry attempt evidence, run-lock contention | **01** state/CAS/journal/locks | failed `replace` on `state.json.tmp` leaves prior bytes byte-identical; malformed JSON raises without overwrite; injected stdout OSError still drains stderr + durable `pipe_error` + exit <5s; 3 retries → 3 unique `attempt_id`; 2nd lock acquire raises in <1s |
| `test_integration.py` | Black-box CLI full protocol (7 calls), human-gate resolve, rate-limit exit 75 + resume, budget pause, restart --fresh-plan identity, dirty/untracked/tracked-runtime preflight, live event visibility, real worktree identity + fast-forward, conflict detection w/o mutating main, Windows quoting | **03** engine, **04** git/evidence, **06** e2e | see §3 tree-identity + dirty-preflight |
| `test_orchestration.py` | Plan engine: bounded 4-fresh-call protocol, final-audit gate, hash-guarded revision replacement, decision-gate precedence, inconclusive-review retries reviewer (not lead), one bounded evidence expansion, budget re-arming, verification pass-label cannot override failed evidence | **03** attach/phase/convergence (+**04** evidence, **02** schema) | wrong `base_plan_sha256` → rejected, round 0 byte-unchanged; revision answers every keyed major finding exactly once; `requires_human_decision`/`decision_question` mismatch = protocol failure |
| `test_recovery.py` | Lifecycle transition table (declared edges only), resume metadata, retryable→exact-phase, evidence bounding/hashing/fail-closed, restart retires identity after verified copy, checkpoint hash-equivalent clone + unsafe-session drop, fresh-plan removes only planning, atomic migration backup | **05** lifecycle, **01** state, **04** evidence | `checkpoint_digest` equal before/after clone+remap+copy; `lead_plan` dropped unsafe, `lead_impl`/`pair_review` kept; over-cap evidence raises; `state.v1.bak.json` preserves original bytes |
| `test_release.py` | Config schema migration (narrow tool allowlist) + rollback backup, custom-broad + future-version fail-closed, read-only load applies defaults w/o writing, export excludes raw/symlinks/backups + manifest flag | **07** distribution (+**05** config, redaction) | export omits `stdout.jsonl`/`stderr.log`/`last-message.txt`, `state.v1.bak.json`, symlink→outside secret; `manifest.raw_attempt_streams_included == false` |
| `test_safety.py` | Process cancel/timeout kills whole descendant tree + retains evidence, git tree identity, mechanical gate stream+retain / timeout→FIX, provider resolution by semver, capability policy (evidence phase denies shell/net/nested), rate-limit durability + injectable cancellable wait, cancel idempotency, redaction across display+persisted, retention prunes only raw | **04** tree-identity, **05** caps/lifecycle, **01/02** process, **07** provider | see §3 cancellation/redaction/tree-identity |
| `test_streaming.py` | Event round-trip ignores future fields, journal replays complete lines / ignores partial tail, codex+claude adapter normalization, status projection from journal, event visible before slow process completes, concurrent large drain, unique attempt paths | **02** transport/schema/canonical-JSON | `to_dict`/`from_dict` drops unknown `future_provider_field`; partial trailing line ignored; claude vs codex event-kind sequences (see below) |
| `test_terminal.py` | Watch readable golden (color-safe, no ANSI), raw redacted JSON, replay-then-tail, agent filter + scriptable exit codes, status next-action golden per stop state, display redaction, read-only watch/status never persist, launcher opens 2 view-only watchers | **07** launcher (+**05** status) | exit codes completed→0, cancelled→130, rate_limited→75; watch/status read-only (legacy bytes unchanged); launcher opens exactly 2 `watch` commands, none with `cancel` |
| `test_live_smoke.py` | Opt-in paid provider compat smoke; both providers must expose required caps; preflight refuses when no native spend-cap | **07** dist / **05** caps | gated by env vars; required caps `stream,schema,sandbox,session,nested_agent_control`; skips if no native `budget` cap; asserts `invocations<=2`, `cost<=0.10` |

## 2. Schemas / contracts for Go `internal/protocol`

All are OpenAI strict mode: `additionalProperties:false`, every property `required`. One contract set shared by Claude (`--json-schema`) and Codex (`--output-schema`).

- **`ProtocolViolation`** — raised when structured output is internally inconsistent (e.g. `requires_human_decision` flag/question mismatch).
- **`TASK_CONTRACT`** — `goal, current_behavior, desired_behavior, scope, non_goals[], constraints[], acceptance_criteria[], required_tests[], relevant_files[], open_questions[]`.
- **`FINDING`** — `severity∈{blocking,major,minor,nit}, file:string|null, line:integer|null, problem, evidence, suggested_fix` (file/line nullable → plan findings can cite plan sections).
- **`PLAN_FINDING`** — FINDING + `key` (stable lowercase-hyphen id, reused on recurrence), `kind∈{new,repeated,regression,decision}`, `category∈{architecture,scope,sequencing,safety,validation,decision}`. **`content` is deliberately NOT a category** (content facts → implementation_checks).
- **`IMPLEMENTATION_CHECK`** — `key, description, evidence, action∈{add,remove}, target_step:string|null`.
- **`_PLAN_STEP`** — `title, description (file-level, independently committable), files[], tests[]`.
- **`PAIR_PLAN`** — `plan_markdown, steps[], risks[], open_questions[]`.
- **`PLAN_CRITIQUE`** — `verdict∈{AGREE,REVISE}, findings[PLAN_FINDING], implementation_checks[], requires_human_decision:bool, decision_question:string|null, missing_evidence[], simpler_alternative:string|null, notes`.
- **`PLAN_REVISION`** — hash-guarded section replacement: `base_plan_sha256, plan_markdown:string|null, steps:array|null, risks:array|null, open_questions:array|null, responses[{finding_key, finding, action∈{accepted,rebutted}, rationale}]`. Null = preserve canonical baseline section.
- **`CHECKPOINT_REVIEW`** — `verdict∈{AGREE,REVISE}, findings[FINDING], tests_adequate:bool, tests_critique`.
- **`IMPLEMENTATION_REPORT`** — `commits[{sha,message}], files_changed[], tests_command, tests_passed:bool, test_output_summary, deviations_from_plan[], notes`.
- **`VERIFICATION`** — `criteria[{criterion, met:bool, evidence}], scope_expansion[], tests_meaningful:bool, unsupported_claims[], verdict∈{pass,fail}, notes`.

**Convergence / stop rules (port exactly — the phase-engine gate):**
- `is_converged(critique)` = AGREE **and** no `missing_evidence` **and** `tests_adequate` not False **and** no actionable findings. Evidence/test gaps are NOT convergence.
- `has_actionable_findings(review)` = any `blocking`/`major` finding.
- `is_inconclusive_review` = withheld AGREE but no actionable defect and no human-decision → retries the **reviewer**, never rewrites the artifact.
- `requires_human_decision` = true only when explicit flag XOR-consistent with a non-empty question; mismatch **raises `ProtocolViolation`**.

## 3. Subtle-behaviour vectors (quoted edge cases)

### Git tree identity (subject 04)
`worktree_identity()` → `content_sha256, untracked[{path}], status, head_tree, head`.
- Untracked bytes are dirt and change `content_sha256`; `untracked[0].path == "new.txt"`.
- Tracked mod / restore / untracked-add / delete each change identity; delete surfaces path in `status`.
- Committed clean tree: `status` falsy, `head_tree == tree_id(root)`, `diff_text` contains payload.
- Tested worktree tree == fast-forward integration tree: `tested.head_tree == tree_id(worktree)`, HEAD is-ancestor of feature, `tree_id(root,"feature") == tested.head_tree`.
- Conflict detected without mutating main: `git merge-tree --write-tree` nonzero, stdout has `CONFLICT`, `head_commit` unchanged before==after.
- Final identity invariant: `verified_commit == worktree.head`.
- Dirty-start preflight → exit 1 "tracked or untracked changes before run start", no provider call, no run dir.
- Runtime-ignore missing / tracked-runtime → "runtime paths are not ignored" / "runtime state is tracked by Git" + non-destructive `git rm --cached -r .claudex` repair.
- Symlink → outside secret NOT included in export.
- **GAPS to add in Go:** ignored-file bytes beyond `.claudex/`, submodules, CRLF/`.gitattributes` filters, case-only path collisions, staged-index dirt vs worktree dirt, racing edits during capture.

### Process cancellation / timeout (subjects 01/04)
- Cancel via `<attempt.root>/cancel.requested` → `ProcessCancelledError`, `terminal_reason=="cancelled"`, event kinds include `started`+`cancelled`, descendant PID dead within 5s.
- `timeout=1` → `ProcessTimeoutError`, `terminal_reason=="timeout"`, stdout retained, descendant dead.
- Hung mechanical gate → `terminal_reason=="timeout"`, phase→FIX.
- Rate wait injectable/cancellable: takes injected `clock`/`sleeper` (no real sleep); returns False without sleeping once `cancel.requested` exists.
- Cancel idempotent: two cancels both rc 0, lifecycle CANCELLED.

### Redaction boundaries (subjects 05/07)
- `redact_text("token=sk-ant-...")` must not contain the secret.
- Secret absent from `attempt.stdout/stderr/events/command` after `stream_process`; `atomic_json(result, {...secret...})` strips it (bare token AND `Authorization: Bearer <secret>`).
- `render_state(raw=True)` / `render_event(raw=True)` strip secrets.
- Export omits raw streams, `state.v1.bak.json`, symlinks; `manifest.raw_attempt_streams_included == false`.
- `prune_raw_attempts(max_bytes=0)` removes exactly the 3 raw files (even with non-UTF-8 bytes), keeps events/result/summary; unlink failure non-fatal, reported in `errors`.
- Journal write OSError still returns rc 0, sets `result.emitter.write_errors`, summary still writes.

### Usage normalization (subject 02/05)
- codex `{input:100, cached_input:40, output:30, reasoning:20}` → `total_reported_input==100` (cached is subset), `output==30`.
- claude `{input:60, cache_read:40, cache_creation:5, output:10}` → `total_reported_input==105` (cache read+creation ADD).
- Idempotent: 1st `record_result` True, 2nd (same id) False; `cost_usd=None` → `provider_cost_usd==0.0` + `unknown_cost_attempts==1`.
- Foreign currency `{"EUR":1.25}` → `provider_cost_usd==0.0`, `budget_violations` has `"currency"` until `acknowledged_cost_currencies` includes EUR.

### Canonical-JSON / digest determinism (subject 02/04)
- `plan_digest(plan)` feeds `base_plan_sha256`; wrong baseline → revision rejected, canonical round 0 byte-preserved.
- Evidence manifest: every entry 64-char sha256; `total_bytes <= max_bytes`; purposes `canonical_plan`, `repo:<path>`.
- `checkpoint_digest` equal before clone and after clone+copy+remap.
- Failed `state.json.tmp` replace leaves prior bytes; migration writes `state.v{N-1}.bak.json` with exact original bytes; future schema version rejected "newer than supported".
- Fake-provider call log uses `json.dumps(separators=(",",":"))` + `prompt_sha256 = sha256(prompt)`.
- Event journal: only complete newline-terminated lines replayed; partial tail ignored; `AgentEvent` round-trips + drops unknown future fields.

## 4. Fixtures (`tests/fixtures/`) — reusable scenario shapes
- **`fake_provider.py`** (+ `fake_claude.py`/`fake_codex.py` shims) — scenario-driven subprocess emulating BOTH provider CLIs. Reads `CLAUDEX_FAKE_SCENARIO` (JSON `schema_version:1`), appends `{provider,cwd,prompt_sha256,prompt,argv}` to `CLAUDEX_FAKE_CALL_LOG`. Handles version/help capability probes, `write_commits`, `spawn_descendant`, `ready_file`/`release_file` sync, `hang`, per-call `error`/`exit_code`, usage/cost. Claude emits `result.structured_output`; Codex emits `thread.started`/`item.completed`/`turn.completed`. **→ the Go e2e harness (06.3) reimplements this shape.**
- **`process_tree.py`** — spawns `sleep(120)` descendant, writes PID to argv[1] (descendant-kill target). **`hung_command.py`** = `sleep(120)`. **`noisy_command.py`** = 1.1 MB to each stream.
- **`goldens/status-next-actions.json`** — canonical next-action per stop state (running/completed/cancelled/failed_retryable/failed_terminal/rate_limited/paused_budget/paused_decision).
- **`goldens/watch-readable.txt`** — exact color-safe line format (no ANSI).
- **`scenarios/event-matrix.json`** — `partial-tools-usage` (incl. malformed `"{malformed"` line + `future.unknown` event), `rate-limit`, `hang-with-descendant`.

## 5. Coverage gaps to ADD in the Go suite
Not covered by the old vectors — add when the owning subject lands: ignored-file content beyond `.claudex/`; git submodules; CRLF/`.gitattributes` filter round-trips; case-only path collisions (Windows/macOS); staged-index dirt distinct from worktree dirt; racing edits during identity capture; redaction beyond `sk-ant-`/`Authorization: Bearer`.
