# 06 — Agent Skills And BYO Integration (MVP milestone)

## Goal
The executable protocol clients that make BYO attach **autonomous** rather than
a human hand-shuttling JSON: a Claude Code skill and a Codex prompt/skill that
each run the `pull → work → submit → wait` loop for both lead and pair roles.
Then prove the whole cross-subject product end-to-end with two attached sessions,
including concurrency and crash/fault scenarios. Reaching `DONE` here is the
MVP (BYO-attach tier).

## Integration analysis
> Fill/confirm against 00.2 before ticking any box.
- **Existing code found** — README "Two ways to run it" live-pair skill (`.claude/skills/claudex-pair/SKILL.md`) + `codex-lead-prompt.md`; `claudex/templates/SKILL.md` (per project memory); the `.mailbox/` PoC loop-prompt from this session (the exact behaviour to formalize); existing `tests/test_integration.py` + scenario-driven fake executables (per lessons: "Scenario-driven fake executables are the authoritative integration boundary").
- **Behaviour to preserve** — the fake-executable integration harness; "run pair turns in the background"; worktree is a sibling dir (grant `--add-dir`).
- **Reuse / extend** — rewrite the shipped skill to teach the `pull/submit/wait` client loop instead of the old `pair plan/checkpoint/verify` subprocess calls; reuse the fake-executable harness to drive two simulated attached terminals.
- **Do not duplicate** — no second integration harness; extend the fakes.
- **Integration point + why** — skills are the client half of the 02 protocol; the fake-exec harness is the honest e2e boundary.
- **Vision fit** — realizes D008 (skills are executable clients = the MVP), and the human-in-both-terminals product.
- **Risks** — skill telling the agent to commit (must forbid — D006); the loop not disclosing `human_context`; flaky long-poll timing in tests.

## Boxes
- [ ] **06.1** (agent) Claude Code skill: teach the `attach → pull → act → submit → wait` loop for lead and pair with **phase-specific permissions** — the lead edits the worktree only on IMPLEMENT/FIX; on planning turns the lead is read-only; the pair is always read-only and reviews the **immutable evidence packet** (04.3), never the lead worktree. Forbid `git commit` (coordinator commits, D006); require `human_context` disclosure for requirement-changing dialogue. Ships as the installed skill.
- [ ] **06.2** (agent) Codex prompt/skill: the mirror client for a codex-led or codex-pair session; same loop, same phase-specific permissions, same commit prohibition, same disclosure rule, same immutable-evidence review; respects codex binary quirks (`[[codex-cli-binary-quirks]]`).
- [ ] **06.3** (agent) Extend the fake-executable harness to simulate two attached interactive terminals exchanging assignments/results over the real protocol. Test scaffold only.
- [ ] **06.4** (agent) End-to-end BYO run: two fake sessions complete PLAN→…→DONE on a toy change; coordinator advances state, commits snapshots, syncs the index, runs the test gate outside the lock, and the pair reviews the evidence packet. **Behaviour verification** (§ checkpoint): observed-vs-expected recorded, not a green exit alone.
- [ ] **06.5** (agent) Concurrency tests: **concurrent first attach** yields one run (01.4); simultaneous submits; `wait` not blocking `status`; stale `turn_id`/digest-conflict rejection; two-human gate contention on one `gate_id`; **same-role session replacement** mid-run. Test: each resolves per D004/03.2.
- [ ] **06.6** (agent) Crash/fault tests covering **every transaction cut** (01.5/04.2): crash at prepared-journal / commit / ref CAS / index sync / state CAS / receipt / ledger / completion each reconciles idempotently; interrupted `wait` resumes; torn-artifact never observed; **long-test cancellation** works while the lock is released (04.5); a **reviewer operating while the lead worktree is later dirtied** still sees stable evidence (04.3); post-snapshot edit blocks the next submit (04.4). Test: each scenario reproducible via the harness.
- [ ] **06.7** (agent) MVP acceptance: `status` honestly labels the run `protocol-only`; the §2 attach e2e gate passes; README/skill docs updated for the BYO flow. Milestone recorded.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
