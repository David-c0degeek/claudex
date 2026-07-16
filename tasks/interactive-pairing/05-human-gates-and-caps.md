# 05 — Human Gates And Caps

## Goal
Two levels of human interaction, honest observable-only caps, and operator
lifecycle. Local dialogue (an agent asks its own human inline) never mutates
shared state but is disclosed as `human_context` in the next artifact. A shared
gate (`requires_human_decision`) enters the AWAIT_GUIDANCE phase; either human
answers via `resolve --gate-id`, binding the decision into the guidance ledger
and every later assignment. Caps enforced in attach are only those the
coordinator can observe. Operator commands (`cancel`, `clean`, `export`,
`recover`) keep an abandoned run recoverable; `status` stays read-only.

## Integration analysis
> **Superseded by D013/D014 (Go greenfield):** implement fresh in Go (`internal/gates`, `internal/caps`, `internal/lifecycle`) — the Python `budgets.py`/`limits.py`/`cli.py` names below are reference-only. This subject owns cap *policy/admission/labels* (observable-only); subject 03 owns phase counters. "Reuse" = harvested test vectors from 00.2. Re-fill in Go terms before ticking any box.
> Fill/confirm against the 00.2 harvest before ticking any box.
- **New Go packages** — `internal/gates` (`resolve`/`continue` with `gate_id`, guidance ledger, AWAIT_GUIDANCE phase in `internal/state`), `internal/caps` (observable-only cap policy/admission + honesty labels), `internal/lifecycle` (macro states PAUSED / PAUSED_BUDGET / RATE_LIMITED). Operator commands live in `cmd/claudex`.
- **Requirements harvested (D014)** — gate rules (both `requires_human_decision: true` and a concrete question), budget-exhaustion-is-a-quality-stop, persistent binding guidance, and the exact resume/resolve/continue lifecycle strings come from the old tests as vectors. The Python `budgets.py` (usage/economic accounting) and `limits.py` (rate-limit text parsing) are **reference-only** — neither is a general cap engine; the Go cap policy is written fresh and observable-only.
- **Behaviour to preserve** — gate/guidance semantics; observable-only cap admission; `status` read-only.
- **Ownership split** — subject 03 owns phase counters + the threshold transition; **this subject owns cap policy/admission** (artifact-byte + wall-time checks, extension/gate UX, honesty labels).
- **Do not duplicate** — one guidance store, one cap-policy package, one lifecycle.
- **Vision fit** — realizes D004 (gate identity), D007 (observable-only caps), "human decisions are never silent."
- **Risks** — pretending to enforce token/cost caps in attach (dishonest); two humans answering different gate generations; `status` secretly reconciling instead of projecting `recovery_pending` (must stay read-only, 01.6).

## Boxes
- [ ] **05.1** (agent) `human_context` disclosure: an accepted artifact may carry human context from local dialogue; the coordinator records it in the ledger and threads it into later assignments. Test: disclosed context appears in the next `pull`.
- [ ] **05.2** (agent) Shared gate: a submit with `requires_human_decision: true` + a concrete question issues a `gate_id` (+ revision) and enters AWAIT_GUIDANCE; inconsistent combos are retryable protocol errors, not gates. Test: valid gate opens; label-only does not.
- [ ] **05.3** (agent) `resolve --gate-id --notes`/`--notes-file`: accept-once against the gate's revision so two humans can't answer different generations; bind the decision into the guidance ledger + every later assignment. Test: stale `gate_id` rejected; guidance persists.
- [ ] **05.4** (agent) `continue`: allow one more lead response + fresh audit without inventing a decision, for a pure quality-budget stop. Test: `continue` extends without creating a gate.
- [ ] **05.5** (agent) Observable-only caps: enforce turn/fix counts (from 03's counters), artifact bytes, and wall time at `submit`; explicitly do NOT enforce token/cost/tool-call caps in BYO attach. `status` labels each cap with its backing mechanism; an unobservable usage cap is labeled `requires-telemetry`/`unavailable` (NOT merely `requires-managed` — managed-with-inherited-stdio alone is still unmetered), per D007/§6.20. Test: an observable cap blocks; a usage cap reports `unavailable`, not enforced.
- [ ] **05.6** (agent) Recovery/status UX (read-only): `status` surfaces lifecycle, the exact next action (resume/resolve/continue/recover/merge), current gate, whose turn, and honesty labels; reads never mutate — if a journal is pending it shows `recovery_pending` + the exact action (per 01.6), it does not reconcile. Test: after a crash mid-transaction, `status` shows the reconciled-pending state without side effects.
- [ ] **05.7** (agent) Operator lifecycle, interactive-aware (do NOT carry old headless cancellation semantics into human sessions):
      - **`cancel`** marks the run cancelled and terminates only **coordinator-owned work** — notably the active test process — via an out-of-band cancellation request the lock-free test runner observes (04.5); it must **not** kill a BYO or managed human-interactive TUI. Closing a human session is a separate explicit action (or the human closes their own terminal). Idempotent; keeps partial evidence.
      - **`recover`** reconciles a pending journal under the lock (01.6).
      - **`export`** may run on a live run.
      - **`clean`** refuses a live, pending-transaction, or dirty run (concrete refusal, not merely "safe").
      - Same-role session replacement handoff coordinates with 03.2.
      Test: `cancel` stops the test process but leaves the TUIs alive; `recover` completes a pending transaction; `clean` refuses a live/pending/dirty run; `export` succeeds live.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
