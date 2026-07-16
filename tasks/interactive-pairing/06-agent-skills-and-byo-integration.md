# 06 — Agent Skills And BYO Integration (MVP milestone)

## Goal
The executable protocol clients that make BYO attach **autonomous** rather than
a human hand-shuttling JSON: a Claude Code skill and a Codex prompt/skill that
each run the `pull → work → submit → wait` loop for both lead and pair roles.
Then prove the whole cross-subject product end-to-end with two attached sessions,
including concurrency and crash/fault scenarios. Reaching `DONE` here is the
MVP (BYO-attach tier).

## Integration analysis
> **Note (D013/D014):** the coordinator is Go, but the agent-side skills are language-neutral — a Claude Code skill (markdown/SKILL.md) + a Codex prompt teaching the `pull→work→submit→wait` loop against the Go CLI. The fake-executable e2e harness is new Go test code (`internal/harness` / `*_test.go`) driving the real protocol; the old Python `tests/test_integration.py` fixtures are harvested as scenarios (00.2), not ported. Re-fill in Go terms before ticking any box.
> Fill/confirm against 00.2 before ticking any box.
- **Requirements harvested (D014)** — the README live-pair skill + `codex-lead-prompt.md` + `.mailbox/` PoC loop-prompt define the *client loop shape*; the old `tests/test_integration.py` + scenario-driven fake executables are harvested as **scenarios/vectors** (per lessons: "Scenario-driven fake executables are the authoritative integration boundary") — not ported.
- **New Go / assets** — one new Go e2e harness (`internal/harness` + `*_test.go`) that drives two simulated attached terminals over the real protocol; a Claude Code skill (`SKILL.md`, markdown) + a Codex prompt — both language-neutral, teaching the `attach → pull → work → submit → wait` loop.
- **Behaviour to preserve** — run pair turns in the background; worktree is a sibling dir (grant `--add-dir`); the fake-executable *approach* as the honest e2e boundary.
- **Do not duplicate** — build ONE Go harness; do not port or wrap the Python fakes — harvest their scenarios as Go test vectors.
- **Integration point + why** — skills are the client half of the 02 protocol; the Go harness is the e2e boundary over the real CLI.
- **Vision fit** — realizes D008 (skills are executable clients = the MVP) + the human-in-both-terminals product.
- **Risks** — skill telling the agent to commit (must forbid — D006); the loop not disclosing `human_context`; flaky long-poll timing in tests.

## Boxes
- [ ] **06.1** (agent) Claude Code skill: teach the `attach → pull → act → submit → wait` loop for lead and pair with **phase-specific permissions** — the lead edits the worktree only on IMPLEMENT/FIX; on planning turns the lead is read-only; the pair is always read-only and reviews the **immutable evidence packet** (04.3), never the lead worktree. Forbid `git commit` (coordinator commits, D006); require `human_context` disclosure for requirement-changing dialogue. Ships as the installed skill.
- [ ] **06.2** (agent) Codex prompt/skill: the mirror client for a codex-led or codex-pair session; same loop, same phase-specific permissions, same commit prohibition, same disclosure rule, same immutable-evidence review; respects codex binary quirks (`[[codex-cli-binary-quirks]]`).
- [ ] **06.3** (agent) Build one Go e2e harness (`internal/harness`) simulating two attached interactive terminals exchanging assignments/results over the real protocol; seed it with scenarios harvested from the old Python integration fakes (00.2). Test scaffold only.
- [ ] **06.4** (agent) End-to-end BYO run: two fake sessions complete PLAN→…→DONE on a toy change; coordinator advances state, commits snapshots, syncs the index, runs the test gate outside the lock, and the pair reviews the evidence packet. **Behaviour verification** (§ checkpoint): observed-vs-expected recorded, not a green exit alone.
- [ ] **06.5** (agent) Concurrency tests: **concurrent first attach** yields one run (01.4); simultaneous submits; `wait` not blocking `status`; stale `turn_id`/digest-conflict rejection; two-human gate contention on one `gate_id`; **same-role session replacement** mid-run. Test: each resolves per D004/03.2.
- [ ] **06.6** (agent) Crash/fault tests covering **every transaction cut** (01.5/04.2): crash at prepared-journal / commit / ref CAS / index sync / state CAS / receipt / ledger / completion each reconciles idempotently; interrupted `wait` resumes; torn-artifact never observed; **long-test cancellation** works while the lock is released (04.5); a **reviewer operating while the lead worktree is later dirtied** still sees stable evidence (04.3); post-snapshot edit blocks the next submit (04.4). Test: each scenario reproducible via the harness.
- [ ] **06.7** (agent) MVP acceptance: `status` honestly labels the run `protocol-only`; the §2 attach e2e gate passes; README/skill docs updated for the BYO flow. Milestone recorded.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
