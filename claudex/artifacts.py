"""Render structured agent findings into human-readable markdown artifacts.

The JSON files are the machine contract; these .md files are what the human
reads at the two gates (plan selection, final approval).
"""

from __future__ import annotations

import json
from pathlib import Path


def save_json(path: Path, data: dict) -> None:
    path.write_text(json.dumps(data, indent=2), encoding="utf-8")


def _section(title: str, items: list[str]) -> str:
    if not items:
        return f"## {title}\n\n_None._\n"
    return f"## {title}\n\n" + "\n".join(f"- {i}" for i in items) + "\n"


def render_task_contract(c: dict) -> str:
    return "\n".join(
        [
            f"# Goal\n\n{c.get('goal', '')}\n",
            f"# Current behavior\n\n{c.get('current_behavior', '')}\n",
            f"# Desired behavior\n\n{c.get('desired_behavior', '')}\n",
            f"# Scope\n\n{c.get('scope', '')}\n",
            _section("Explicit non-goals", c.get("non_goals", [])).replace("##", "#", 1),
            _section("Constraints", c.get("constraints", [])).replace("##", "#", 1),
            _section("Acceptance criteria", c.get("acceptance_criteria", [])).replace("##", "#", 1),
            _section("Required tests", c.get("required_tests", [])).replace("##", "#", 1),
            _section("Relevant files", c.get("relevant_files", [])).replace("##", "#", 1),
            _open_questions_section(c.get("open_questions", [])),
        ]
    )


def _open_questions_section(questions: list[str]) -> str:
    """Each question gets an Answer stub — the human fills it in; agents are
    told Answer: lines are binding contract decisions."""
    if not questions:
        return "# Open questions\n\n_None._\n"
    lines = []
    for q in questions:
        lines.append(f"- {q}")
        lines.append("  - Answer: ")
    return "# Open questions\n\n" + "\n".join(lines) + "\n"


def render_analysis(agent: str, a: dict) -> str:
    ev = [
        f"`{e.get('file', '?')}` — {e.get('symbol', '')} — {e.get('claim', '')}".replace(" —  — ", " — ")
        for e in a.get("evidence", [])
    ]
    rejected = [
        f"**{r.get('alternative', '?')}** — {r.get('reason_rejected', '')}"
        for r in a.get("rejected_alternatives", [])
    ]
    return "\n".join(
        [
            f"# {agent.capitalize()} analysis\n",
            f"## Summary\n\n{a.get('summary', '')}\n",
            _section("Execution paths", a.get("execution_paths", [])),
            _section("Evidence", ev),
            _section("Assumptions", a.get("assumptions", [])),
            _section("Risks", a.get("risks", [])),
            f"## Proposed solution\n\n{a.get('proposed_solution', '')}\n",
            _section("Rejected alternatives", rejected),
            _section("Tests required", a.get("tests_required", [])),
            _section("Open questions", a.get("open_questions", [])),
        ]
    )


def render_review_report(agent: str, r: dict) -> str:
    findings = [
        f"**[{f.get('severity', '?')}]** {f.get('area', '?')} — "
        f"{f.get('finding', '')} (evidence: {f.get('evidence', '')})"
        for f in r.get("findings", [])
    ]
    return "\n".join(
        [
            f"# {agent.capitalize()} review\n",
            f"## Summary\n\n{r.get('summary', '')}\n",
            _section("Findings", findings),
            _section("Open questions", r.get("open_questions", [])),
            f"## Full report\n\n{r.get('report_markdown', '')}\n",
        ]
    )


def render_disagreement(d: dict) -> str:
    conflicts = []
    for c in d.get("conflicts", []):
        conflicts.append(
            f"### {c.get('topic', '?')}\n\n"
            f"- **Claude:** {c.get('claude_position', '')}\n"
            f"- **Codex:** {c.get('codex_position', '')}\n"
            f"- **Repository shows:** {c.get('repo_evidence', '')}\n"
            f"- **Recommendation:** {c.get('recommendation', '')}\n"
        )
    return "\n".join(
        [
            "# Disagreement analysis\n",
            _section("Agreements", d.get("agreements", [])),
            "## Conflicts\n",
            "\n".join(conflicts) if conflicts else "_None._\n",
            _section("Unique to Claude", d.get("unique_to_claude", [])),
            _section("Unique to Codex", d.get("unique_to_codex", [])),
            f"## Recommended plan: **{d.get('recommended_plan', '?')}**\n",
            f"{d.get('recommendation_rationale', '')}\n",
        ]
    )


def render_selected_plan(author: str, analysis: dict, notes: str) -> str:
    parts = [
        f"# Proposed plan (author: {author})\n",
        f"## Solution\n\n{analysis.get('proposed_solution', '')}\n",
        _section("Tests required", analysis.get("tests_required", [])),
        _section("Assumptions", analysis.get("assumptions", [])),
        _section("Risks", analysis.get("risks", [])),
    ]
    if notes:
        parts.append(f"## Selection notes (human)\n\n{notes}\n")
    return "\n".join(parts)


def render_plan_review(reviewer: str, r: dict) -> str:
    disputed = [
        f"**{d.get('claim', '?')}** — {d.get('why_disputed', '')} "
        f"(repo: {d.get('repo_evidence', '')})"
        for d in r.get("disputed_claims", [])
    ]
    parts = [
        f"# Adversarial plan review (reviewer: {reviewer})\n",
        f"**Verdict: {r.get('verdict', '?')}**\n",
        _section("Confirmed claims", r.get("confirmed_claims", [])),
        _section("Disputed claims", disputed),
        _section("Missing evidence", r.get("missing_evidence", [])),
        _section("Blocking corrections", r.get("blocking_corrections", [])),
        _section("Non-blocking suggestions", r.get("non_blocking_suggestions", [])),
    ]
    if r.get("simpler_alternative"):
        parts.append(f"## Simpler alternative\n\n{r['simpler_alternative']}\n")
    return "\n".join(parts)


def render_code_review(reviewer: str, round_no: int, r: dict) -> str:
    rows = []
    for f in r.get("findings", []):
        loc = f.get("file", "?")
        if f.get("line"):
            loc += f":{f['line']}"
        rows.append(
            f"- **[{f.get('severity', '?')}]** `{loc}` — {f.get('problem', '')}\n"
            f"  - evidence: {f.get('evidence', '')}\n"
            f"  - fix: {f.get('suggested_fix', '')}"
        )
    return "\n".join(
        [
            f"# Code review — round {round_no} (reviewer: {reviewer})\n",
            f"**Verdict: {r.get('verdict', '?')}** · tests adequate: "
            f"{r.get('tests_adequate', '?')}\n",
            "## Findings\n",
            "\n".join(rows) if rows else "_None._\n",
            f"## Tests critique\n\n{r.get('tests_critique', '')}\n",
        ]
    )


def render_verification(verifier: str, v: dict) -> str:
    crit = [
        f"{'✅' if c.get('met') else '❌'} {c.get('criterion', '?')} — "
        f"{c.get('evidence', '')}"
        for c in v.get("criteria", [])
    ]
    return "\n".join(
        [
            f"# Final verification (verifier: {verifier}, fresh context)\n",
            f"**Verdict: {v.get('verdict', '?')}** · tests meaningful: "
            f"{v.get('tests_meaningful', '?')}\n",
            _section("Acceptance criteria", crit),
            _section("Scope expansion", v.get("scope_expansion", [])),
            _section(
                "Claims supported only by agent testimony",
                v.get("unsupported_claims", []),
            ),
            f"## Notes\n\n{v.get('notes', '')}\n",
        ]
    )


def render_implementation_report(owner: str, r: dict) -> str:
    commits = [
        f"`{c.get('sha', '')[:12]}` {c.get('message', '')}" for c in r.get("commits", [])
    ]
    return "\n".join(
        [
            f"# Implementation report (owner: {owner})\n",
            f"**Tests passed: {r.get('tests_passed', '?')}** · command: "
            f"`{r.get('tests_command', '')}`\n",
            _section("Commits", commits),
            _section("Files changed", r.get("files_changed", [])),
            _section("Deviations from plan", r.get("deviations_from_plan", [])),
            f"## Test output summary\n\n{r.get('test_output_summary', '')}\n",
            f"## Notes\n\n{r.get('notes', '')}\n",
        ]
    )
