"""Phase prompts.

Every prompt hands the agent the same immutable task contract (task.md) plus
exact artifact file paths — never a paraphrase of what the other agent said.
Structured output is enforced by schema at the CLI layer, so prompts describe
intent, not formatting. Prompts stay thin (paths, not content) so long
resumed lineages don't bloat.
"""

from __future__ import annotations

from pathlib import Path

TEAM = """\
You are one of two senior engineers pair programming on this task. One of
you is the LEAD (holds the pen: drafts the plan, implements, fixes); the
other is the PAIR (critiques the plan, reviews every checkpoint, verifies).
You are one team converging on one plan and one implementation. Disagree
openly when the evidence demands it, concede when it doesn't — the goal is
the best shippable change, not winning the argument.
"""

EVIDENCE_RULES = """\
Rules of evidence:
- Every material claim must cite a file path and, where possible, a symbol.
- Distinguish clearly between what you verified and what you assume.
- Prefer the minimal justified solution; explain why bigger ones lose.
- Do not edit, create, or delete any files. This phase is read-only.
"""

ANSWERS_ARE_BINDING = """\
In the task contract, lines marked "Answer:" under Open questions are the
human's decisions. They are binding parts of the contract, not suggestions.
"""


def guidance_block(notes: str) -> str:
    if not notes:
        return ""
    return f"""
The human broke a deadlock between you two with this binding guidance:
---
{notes}
---
Treat it like an Answer: line in the contract.
"""


def task_contract(description: str) -> str:
    return f"""A human engineer gave this one-paragraph task description for this
repository:

---
{description}
---

Draft the full task contract by investigating the repository directly. Ground
every section in what the repository actually contains: name real files,
real symbols, real behavior. Capture the human's intent faithfully — do not
invent requirements they did not imply. Where the description leaves a real
decision open, put it in open_questions rather than silently resolving it.
Acceptance criteria must be independently checkable statements; required
tests must be tests that can fail.

The goal must start with the deliverable type in brackets: "[REPORT]" if the
task produces analysis/review output without changing product code,
"[CHANGE]" if it modifies code or docs, "[MIXED]" if both. If the human's
description is ambiguous between reporting and changing (e.g. "review and
find what's wrong" — report the findings, or also fix them?), that ambiguity
is a mandatory open question: pick the narrower reading for the draft and
ask.

This phase is read-only. Do not edit any files.
"""


# ------------------------------------------------------------- plan converge
def plan_draft(task_path: Path) -> str:
    return f"""{TEAM}
You are the LEAD. Read the task contract at: {task_path}

Investigate this repository directly and draft the implementation plan your
pair will critique. The plan must be grounded in what the repository
actually contains and self-contained: someone who has read only the task
contract and this plan can implement the change.

Break the work into ordered steps. Each step must be independently
implementable and committable — it becomes exactly one commit, reviewed by
your pair before the next step starts. Prefer few, coherent steps over many
fragments. Name the files each step touches and the tests it adds or
changes.

{ANSWERS_ARE_BINDING}
{EVIDENCE_RULES}"""


def plan_critique(task_path: Path, plan_path: Path, round_no: int, guidance: str = "") -> str:
    return f"""{TEAM}
You are the PAIR. Critique round {round_no} of your lead's plan.

Task contract: {task_path}
Lead's current plan (JSON: plan_markdown + steps): {plan_path}
{guidance_block(guidance)}
Verify every material claim against the repository — do not assume the plan
is correct. Find: incorrect assumptions, missing affected paths, unnecessary
scope, architectural inconsistencies, compatibility risks, insufficient
tests, steps that are not independently committable, and simpler valid
alternatives.

Severity: "blocking" = the plan is wrong or unsafe without this fix;
"major" = should fix before implementation; "minor"/"nit" = record only.
Verdict "AGREE" only if you have zero blocking and zero major findings —
AGREE means you co-own this plan and will review its implementation.
If the lead rebutted an earlier finding of yours with repository evidence,
verify the rebuttal; concede when it holds, escalate severity when it
doesn't.

{ANSWERS_ARE_BINDING}
{EVIDENCE_RULES}"""


def plan_revise(task_path: Path, critique_path: Path, round_no: int, guidance: str = "") -> str:
    return f"""{TEAM}
You are the LEAD. Your pair critiqued your plan (round {round_no}).

Task contract: {task_path}
Pair's critique (JSON): {critique_path}
{guidance_block(guidance)}
Revise the plan. For every blocking and major finding: either incorporate it
(action "accepted"), or rebut it with direct repository evidence (action
"rebutted" — a rebuttal without file-level evidence is not acceptable).
Incorporate minor findings where they genuinely improve the plan; drop them
otherwise. Do not silently drop any blocking/major finding.

Re-emit the COMPLETE revised plan — full plan_markdown and the full ordered
steps array, not a delta. Keep steps independently implementable and
committable.

This phase is read-only. Do not edit any files.

{EVIDENCE_RULES}"""


# ---------------------------------------------------------------- implement
def implement_step(
    task_path: Path, plan_path: Path, step_index: int, total: int, step: dict
) -> str:
    files = ", ".join(step.get("files", [])) or "(see plan)"
    tests = "; ".join(step.get("tests", [])) or "(see plan)"
    return f"""You are the LEAD, implementing the agreed plan step by step in a dedicated
git worktree on a dedicated branch; you may edit files here.

Task contract: {task_path}
Agreed plan: {plan_path}

Implement ONLY step {step_index + 1} of {total}: {step.get('title', '')}

{step.get('description', '')}

Files: {files}
Tests: {tests}

Do not start later steps — your pair reviews this step's commit before the
next step begins. If reality forces a deviation from the plan, keep it
minimal and record it in your report — do not silently expand scope.

Requirements:
- Write the tests this step requires. Tests must be able to fail: assert on
  behavior, not on the absence of exceptions.
- Run the test suite (or the closest relevant subset) and record the command
  and outcome truthfully. A failing suite must be reported as failing.
- Commit this step's work with `git add` and `git commit` with a descriptive
  message. Do not push. Do not create branches.
- Never use `git commit --no-verify`, force flags, or history rewrites.
"""


def draft_report(task_path: Path) -> str:
    return f"""You are the LEAD. This task's deliverable IS a report, not a code change.
You are in a dedicated git worktree; you may create and edit files here.

Task contract: {task_path}

Investigate the repository and write the complete report now — the actual
deliverable, not a plan for one. Cover every dimension and acceptance
criterion the contract names, run the read-only checks it requires, and
record each material finding with severity and direct repository evidence.
Honor the contract's decisions about where the report lives (Answer: lines
are binding); default to REVIEW.md at the repository root if the contract
does not say.

Commit the report file(s) with `git add` and `git commit`. Do not push. Do
not modify any product code — this task delivers a report only. Your pair
reviews the committed report next; expect to revise it.

{ANSWERS_ARE_BINDING}"""


# --------------------------------------------------------------- checkpoints
def checkpoint_review(
    task_path: Path,
    plan_path: Path | None,
    step_label: str,
    diff_path: Path,
    base: str,
    round_no: int,
    guidance: str = "",
) -> str:
    plan_line = f"Agreed plan: {plan_path}\n" if plan_path else ""
    return f"""{TEAM}
You are the PAIR. Checkpoint review, round {round_no}, for: {step_label}

Task contract: {task_path}
{plan_line}Exact diff under review ({base[:12]}..HEAD): {diff_path}
{guidance_block(guidance)}
You are inside the implementation worktree at the current commit, so you can
read the final state of every file and run read-only checks. Review the
diff, not the lead's account of it.

Evaluate:
- correctness against the task contract{' and agreed plan step' if plan_path else ''};
- unintended scope expansion beyond this step;
- whether the tests are meaningful (could they fail?) and sufficient;
- error handling, boundary conditions, concurrency, and compatibility where
  relevant.

Severity: "blocking" = must fix before the next step; "major" = fix now;
"minor"/"nit" = record only. Verdict "AGREE" only with zero blocking and
zero major findings — AGREE means this step ships as-is and you co-own it.

Do not modify code.

{ANSWERS_ARE_BINDING}
{EVIDENCE_RULES}"""


def fix(findings_path: Path, round_no: int, source: str) -> str:
    return f"""You are the LEAD. {source} found problems that must be fixed
(fix round {round_no}).

Findings (JSON): {findings_path}

Fix every "blocking" and "major" finding. If you believe a finding is wrong,
do not argue — fix what is real and record your evidence-backed rebuttal for
the rest in your report notes. Do not address minor/nit findings unless the
fix is trivial and zero-risk. Do not expand scope.

Re-run the tests, then commit the fix with `git add` and `git commit`.
Report truthfully.
"""


# -------------------------------------------------------------------- verify
def verify(
    task_path: Path,
    plan_path: Path | None,
    diff_path: Path,
    base_commit: str,
    test_gate_summary: str = "",
) -> str:
    plan_line = f"Agreed plan: {plan_path}\n" if plan_path else ""
    tests_line = (
        f"Mechanical test gate result (coordinator-run): {test_gate_summary}\n"
        if test_gate_summary
        else ""
    )
    return f"""You are the final verifier. You have no history with this change: treat
every prior claim about it as unverified testimony.

Task contract: {task_path}
{plan_line}Full diff (base {base_commit[:12]} -> HEAD): {diff_path}
{tests_line}
You are inside the implementation worktree at the final commit.

Independently answer, with direct file or test evidence for each:
- Does the implementation satisfy every acceptance criterion in the task
  contract? Check each one separately.
- Did scope expand beyond the contract{' and agreed plan' if plan_path else ''}?
- Are the tests meaningful rather than merely passing — do they assert real
  behavior and could they fail?
- Are errors, cancellation, concurrency, persistence, and boundary conditions
  covered where the task makes them relevant?
- Are documentation and public contracts consistent with the change?
- Is any claim about this change supported only by agent testimony rather
  than repository evidence? List such claims.

Verdict "pass" only if every acceptance criterion is met with evidence.

Do not modify code.

{ANSWERS_ARE_BINDING}
{EVIDENCE_RULES}"""
