---
name: claudex-pair
description: Live pair programming with the other AI (codex) via claudex. You are the LEAD — you plan, implement, and fix; claudex runs your PAIR's critique turns and enforces convergence. Trigger when the user says "pair with codex", "claudex pair", "start a pair session", or asks to build something with a second AI reviewing live.
---

# claudex-pair (live mode, you = LEAD)

You are the LEAD in a two-AI pairing session. Your pair (the other model)
critiques your plan, reviews every step's commits, and verifies the result —
all through `claudex pair` subcommands that the coordinator enforces
deterministically: response budgets, phase order, session lineage, and the
append-only `mailbox.md` transcript.

Ground rules the coordinator enforces (do not fight them):
- Convergence = pair verdict AGREE with zero blocking/major findings.
- Every loop has a lead-response budget followed by a final fresh review.
  Budget exhaustion is not a model dispute: run `claudex continue` to allow
  one more response followed by a fresh audit (`--responses N` is explicit).
  Only a concrete unresolved choice goes to the
  human, then `claudex resolve --notes "<their decision>"` (or use
  `--notes-file <path>` for multiline text).
- DONE comes only from `claudex pair verify` — never declare it yourself.
- The pair reviews exact commits. Uncommitted changes are refused.

## Protocol

**0. Preconditions.** `claudex doctor` passes; `.claudex/task.md` is filled
(draft with `claudex task "..."` if not). Read the task contract first.

**1. Start.**
```
claudex pair start            # you lead as claude; --lead codex if you are codex
```
Prints the run dir. Change mode → you draft a plan next. Report mode → a
worktree is printed; write and COMMIT the report there, then go to step 4.

**2. Draft the plan.** Investigate the repo, then write plan JSON:
```json
{
  "plan_markdown": "<compact architecture, contracts, and rationale>",
  "steps": [{"title": "...", "description": "...", "files": ["..."], "tests": ["..."]}],
  "risks": ["..."],
  "open_questions": []
}
```
Each step must be independently implementable and committable — it becomes
exactly one commit, reviewed before the next step starts. Few coherent
steps beat many fragments.
The plan describes how deliverables will be produced and verified; it must
not pre-author documentation, research tables, code, schemas, or test bodies.
Claudex carries content-level discoveries forward as implementation checks.

**3. Converge the plan.**
```
claudex pair plan --file <your-plan.json>
```
IMPORTANT: pair turns invoke the other model and can take many minutes
(plus usage-limit waits). Run the command in the background and poll
`claudex status`, or raise your shell timeout — do not let a foreground
timeout kill the turn.

Read the verdict. On REVISE: address every blocking/major finding — accept
it, or rebut it with file-level repository evidence in your revised plan's
`responses`. Re-emit the FULL plan (markdown + all steps), then submit
again. Loop until AGREE. Concede when the evidence is against you; this is
convergence, not a debate to win.

Each response object must contain the critique's exact stable key:
`{"finding_key":"...","finding":"...","action":"accepted|rebutted","rationale":"..."}`.
The structured `steps`, `risks`, and `open_questions` fields are canonical;
do not duplicate them in `plan_markdown`.

**4. Implement step by step.** On plan agreement the worktree path is
printed — a SIBLING directory of the repo. You need access to it
(`--add-dir`, or work from a shell). For each step:
- implement ONLY that step, in the worktree;
- run the relevant tests honestly;
- COMMIT with a descriptive message (never `--no-verify`, never rewrite
  history);
- then request review (background execution rule applies):
```
claudex pair checkpoint --notes "<one-line summary of the step>"
```
On REVISE: fix blocking/major findings, commit, run checkpoint again. On
AGREE: the coordinator advances you to the next step automatically — check
the printed hint or `claudex status`.

**5. Verify.** After the last step converges:
```
claudex pair verify
```
Runs the configured mechanical test gate (coordinator-run, exit code
decides), then a FRESH-context verification by your pair. Failures come
back as findings: fix, commit, `claudex pair verify` again. On pass the
run is DONE — tell the human to `git merge <branch>` and `claudex clean`.

## When stuck

- `claudex status` — phase, step k/N, response budgets, next command.
- Budget gate (`await_guidance`): run `claudex continue` for one response and
  a fresh audit; do not invent a human decision just to resume.
- Decision gate (`await_guidance`): summarize BOTH positions for the human
  honestly, then `claudex resolve --notes "<their decision>"`, or place a
  multiline answer in a file and use `--notes-file <path>`. The decision is
  persistent and binding — treat it like a contract Answer: line.
- Crash/error mid-turn: `claudex status`; `claudex retry` re-attempts a
  failed phase. Round counters and artifacts survive restarts.
- `mailbox.md` in the run dir is the full transcript — cite it when
  reporting to the human.
