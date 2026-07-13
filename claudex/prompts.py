"""Phase prompts.

Every prompt hands the agent the same immutable task contract (task.md) plus
exact artifact file paths — never a paraphrase of what the other agent said.
Structured output is enforced by schema at the CLI layer, so prompts describe
intent, not formatting.
"""

from __future__ import annotations

from pathlib import Path

INDEPENDENCE = """\
You are one of two independent senior engineers assigned to the same task.
The other engineer is investigating in parallel; you cannot see their work
and must not speculate about it. Your value comes from reaching your own
evidence-backed conclusions.
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


def investigation(task_path: Path) -> str:
    return f"""{INDEPENDENCE}
Read the task contract at: {task_path}

Investigate this repository directly and produce a full engineering analysis:
the relevant execution paths, evidence with file and symbol references, your
assumptions, the risks, the minimal justified solution, the competing
solutions you rejected and why, the tests required, and any open questions.

{ANSWERS_ARE_BINDING}
{EVIDENCE_RULES}"""


def review_investigation(task_path: Path) -> str:
    return f"""{INDEPENDENCE}
Read the task contract at: {task_path}

This task's deliverable IS a review. Produce the complete review now — the
actual review, not a plan for reviewing later. Cover every dimension and
acceptance criterion the contract names, run the read-only checks it
requires, and record each material finding (gaps, wrong implementations,
incorrect assumptions, missing pieces) with severity and direct repository
evidence. Put the full self-contained report in report_markdown; findings
are its machine-readable index.

{ANSWERS_ARE_BINDING}
{EVIDENCE_RULES}"""


def disagreement(
    task_path: Path, claude_analysis: Path, codex_analysis: Path, mode: str = "change"
) -> str:
    basis = "the plan" if mode == "change" else "the base of the final report"
    return f"""Two engineers independently analyzed the same task. Your job is to compare
their analyses and surface every material agreement and disagreement, checking
disputed claims directly against the repository — trust neither analysis.

Task contract: {task_path}
Claude's analysis (JSON): {claude_analysis}
Codex's analysis (JSON): {codex_analysis}

For each conflict, state both positions, then state what the repository
itself shows. Recommend which analysis should become {basis}, and why.
Recommend "claude" or "codex" based on evidence quality and solution
minimality, not verbosity.

{EVIDENCE_RULES}"""


def consolidate_report(
    task_path: Path,
    base_author: str,
    base_report: Path,
    other_report: Path,
    disagreement_path: Path,
) -> str:
    return f"""You are consolidating two independent reviews into the final report. You
are in a dedicated git worktree; you may create and edit files here.

Task contract: {task_path}
Base review ({base_author}'s, human-selected): {base_report}
Other review (JSON): {other_report}
Disagreement analysis (JSON): {disagreement_path}

Produce the final report:
- Start from the base review. Fold in every finding from the other review
  that survives the evidence; note material disagreements and how the
  repository resolves them rather than silently dropping either side.
- Where the disagreement analysis showed a claim to be wrong, correct it.
- Honor the contract's decisions about where the report lives (Answer:
  lines are binding); default to REVIEW.md at the repository root if the
  contract does not say.
- Re-run any read-only checks the contract requires and report their real
  output.
- Commit the report file(s) with `git add` and `git commit`. Do not push.
  Do not modify any product code — this task delivers a report only.

{ANSWERS_ARE_BINDING}"""


def plan_review(task_path: Path, plan_path: Path) -> str:
    return f"""Review this proposal as an independent senior engineer.

Do not assume its conclusions are correct.

Task contract: {task_path}
Proposed plan: {plan_path}

Verify every material claim against the repository.

Find:
- incorrect assumptions;
- missing affected paths;
- unnecessary scope;
- architectural inconsistencies;
- compatibility risks;
- insufficient tests;
- simpler valid alternatives.

Do not modify code.

Classify each correction as blocking (the plan is wrong or unsafe without it)
or non-blocking (improvement). Verdict "approve" only if there are zero
blocking corrections.

{EVIDENCE_RULES}"""


def plan_finalize(task_path: Path, plan_path: Path, review_path: Path) -> str:
    return f"""You authored the proposed plan. An independent reviewer has challenged it.

Task contract: {task_path}
Your plan: {plan_path}
Adversarial review (JSON): {review_path}

Produce the final agreed plan. You must address every blocking correction:
either incorporate it, or rebut it with direct repository evidence (a rebuttal
without file-level evidence is not acceptable). Incorporate non-blocking
suggestions where they genuinely improve the plan; drop them otherwise.
The final plan must be self-contained: someone who has read only the task
contract and your final plan can implement the change.

This phase is read-only. Do not edit any files.

{EVIDENCE_RULES}"""


def implement(task_path: Path, plan_path: Path) -> str:
    return f"""You are the implementation owner. You are working in a dedicated git
worktree on a dedicated branch; you may edit files here.

Task contract: {task_path}
Agreed plan: {plan_path}

Implement the agreed plan exactly. If reality forces a deviation, keep it
minimal and record it in your report — do not silently expand scope.

Requirements:
- Write the tests the plan requires. Tests must be able to fail: assert on
  behavior, not on the absence of exceptions.
- Run the test suite (or the closest relevant subset) and record the command
  and outcome truthfully. A failing suite must be reported as failing.
- Commit your work with `git add` and `git commit` in one or more coherent
  commits with descriptive messages. Do not push. Do not create branches.
- Never use `git commit --no-verify`, force flags, or history rewrites.
"""


def code_review(
    task_path: Path,
    plan_path: Path,
    diff_path: Path,
    report_path: Path,
    base_commit: str,
) -> str:
    return f"""Review a completed implementation as an independent senior engineer. You
did not write this code. Do not assume the implementer's report is accurate.

Task contract: {task_path}
Agreed plan: {plan_path}
Exact diff (base {base_commit[:12]} -> HEAD): {diff_path}
Implementer's report (JSON): {report_path}

You are inside the implementation worktree at the implementation commit, so
you can read the final state of every file and run read-only checks.

Evaluate:
- correctness against the task contract and agreed plan;
- unintended scope expansion;
- whether the tests are meaningful (could they fail?) and sufficient;
- error handling, boundary conditions, concurrency, and compatibility where
  relevant;
- whether the implementer's report matches the actual diff.

Severity: "blocking" = must fix before this change can proceed; "major" =
should fix now; "minor"/"nit" = record only. Verdict "approve" only with zero
blocking and zero major findings.

Do not modify code.

{EVIDENCE_RULES}"""


def report_review(
    task_path: Path,
    diff_path: Path,
    report_path: Path,
    base_commit: str,
) -> str:
    return f"""Review a consolidated review report as an independent senior engineer. You
did not write it. Do not assume any of its claims are accurate.

Task contract: {task_path}
Exact diff (base {base_commit[:12]} -> HEAD, the committed report): {diff_path}
Consolidator's own summary (JSON): {report_path}

You are inside the worktree at the commit, so you can read the report and
run read-only checks against the repository it describes.

Evaluate:
- spot-check the report's material claims against the repository: are they
  evidenced and correct?
- does the report cover every dimension and acceptance criterion the
  contract names?
- were findings from either source review silently dropped?
- did the change stay report-only (no product code touched)?

Severity: "blocking" = the report asserts something false or misses a
contract dimension; "major" = should fix now; "minor"/"nit" = record only.
Verdict "approve" only with zero blocking and zero major findings.

Do not modify anything.

{ANSWERS_ARE_BINDING}
{EVIDENCE_RULES}"""


def remediate(review_path: Path, round_no: int) -> str:
    return f"""An independent review of your implementation found problems that must be
fixed (remediation round {round_no}).

Review findings (JSON): {review_path}

Fix every "blocking" and "major" finding. If you believe a finding is wrong,
do not argue — fix what is real and record your evidence-backed rebuttal for
the rest in your report notes. Do not address minor/nit findings unless the
fix is trivial and zero-risk. Do not expand scope.

Re-run the tests, then commit the remediation with `git add` and
`git commit`. Report truthfully.
"""


def verify(
    task_path: Path,
    plan_path: Path | None,
    diff_path: Path,
    base_commit: str,
) -> str:
    plan_line = f"Agreed plan: {plan_path}\n" if plan_path else ""
    return f"""You are the final verifier. You have no history with this change: treat
every prior claim about it as unverified testimony.

Task contract: {task_path}
{plan_line}Full diff (base {base_commit[:12]} -> HEAD): {diff_path}

You are inside the implementation worktree at the final commit.

Independently answer, with direct file or test evidence for each:
- Does the implementation satisfy every acceptance criterion in the task
  contract? Check each one separately.
- Did scope expand beyond the contract and agreed plan?
- Are the tests meaningful rather than merely passing — do they assert real
  behavior and could they fail?
- Are errors, cancellation, concurrency, persistence, and boundary conditions
  covered where the task makes them relevant?
- Are documentation and public contracts consistent with the change?
- Is any claim about this change supported only by agent testimony rather
  than repository evidence? List such claims.

Verdict "pass" only if every acceptance criterion is met with evidence.

Do not modify code.

{EVIDENCE_RULES}"""
