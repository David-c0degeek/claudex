"""Render structured agent findings into human-readable markdown artifacts.

The JSON files are the machine contract; the .md files are what the human
reads. mailbox.md is the append-only pairing transcript — every turn both
sides take, in the codex-collab block format, written only by the
coordinator (the pair is sandboxed read-only during critiques, and all its
output flows through the coordinator as schema-validated JSON anyway).
"""

from __future__ import annotations

import json
from pathlib import Path

from .security import redact_text, redact_value


def save_json(path: Path, data: dict) -> None:
    path.write_text(json.dumps(redact_value(data), indent=2), encoding="utf-8")


def _section(title: str, items: list[str]) -> str:
    if not items:
        return f"## {title}\n\n_None._\n"
    return f"## {title}\n\n" + "\n".join(f"- {i}" for i in items) + "\n"


# ------------------------------------------------------------------- mailbox
def mailbox_path(run_dir: Path) -> Path:
    return run_dir / "mailbox.md"


MAILBOX_HEADER = """\
# mailbox — pairing transcript
# protocol: append-only turn blocks, written by the coordinator.
# [LEAD] holds the pen (plan, implement, fix); [PAIR] critiques and verifies.
# turn: ===== [ROLE] turn <n> | <stage> | STATUS: <...> =====
#       <body>
#       ----- end [ROLE] turn <n> -----
# convergence: PAIR posts AGREE with zero blocking/major findings.
"""


def mailbox_append(
    run_dir: Path, role: str, turn: int, stage: str, status: str, body: str
) -> None:
    mb = mailbox_path(run_dir)
    if not mb.exists():
        mb.write_text(MAILBOX_HEADER, encoding="utf-8")
    header = f"===== [{role}] turn {turn} |"
    # Idempotent: turn numbers are unique, so a crash between the mailbox
    # append and the state save must not duplicate the block on retry.
    if header in mb.read_text(encoding="utf-8"):
        return
    block = (
        f"\n{header} {stage} | STATUS: {status} =====\n"
        f"{redact_text(body).rstrip()}\n"
        f"----- end [{role}] turn {turn} -----\n"
    )
    with mb.open("a", encoding="utf-8") as f:
        f.write(block)


def findings_digest(findings: list[dict]) -> str:
    """Short per-finding lines for mailbox turn bodies."""
    if not findings:
        return "no findings"
    lines = []
    for f in findings:
        loc = f.get("file") or ""
        if loc and f.get("line"):
            loc += f":{f['line']}"
        loc = f" `{loc}`" if loc else ""
        key = f" {f.get('key')}" if f.get("key") else ""
        kind = f"/{f.get('kind')}" if f.get("kind") else ""
        category = f"/{f.get('category')}" if f.get("category") else ""
        lines.append(
            f"[{f.get('severity', '?')}{kind}{category}{key}]{loc} {f.get('problem', '')}"
        )
    return "\n".join(lines)


# ------------------------------------------------------------- task contract
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


# --------------------------------------------------------------------- plans
def render_plan(lead: str, round_no: int, p: dict) -> str:
    steps = []
    for i, s in enumerate(p.get("steps", [])):
        files = ", ".join(s.get("files", []))
        tests = "; ".join(s.get("tests", []))
        steps.append(
            f"### Step {i + 1}: {s.get('title', '?')}\n\n"
            f"{s.get('description', '')}\n\n"
            f"- files: {files or '_unspecified_'}\n"
            f"- tests: {tests or '_unspecified_'}\n"
        )
    parts = [
        f"# Plan — round {round_no} (lead: {lead})\n",
        f"{p.get('plan_markdown', '')}\n",
        "## Steps\n",
        "\n".join(steps) if steps else "_None._\n",
        _section("Risks", p.get("risks", [])),
        _section("Open questions", p.get("open_questions", [])),
    ]
    responses = []
    for response in p.get("responses", []):
        key = (
            f" `{response.get('finding_key')}`"
            if response.get("finding_key")
            else ""
        )
        responses.append(
            f"**{response.get('action', '?')}**{key}: "
            f"{response.get('finding', '?')} — {response.get('rationale', '')}"
        )
    if responses:
        parts.append(_section("Responses to pair critique", responses))
    return "\n".join(parts)


def _findings_rows(findings: list[dict]) -> list[str]:
    rows = []
    for f in findings:
        loc = f.get("file") or "?"
        if f.get("line"):
            loc += f":{f['line']}"
        key = f" · `{f.get('key')}`" if f.get("key") else ""
        kind = f"/{f.get('kind')}" if f.get("kind") else ""
        category = f"/{f.get('category')}" if f.get("category") else ""
        rows.append(
            f"- **[{f.get('severity', '?')}{kind}{category}]**{key} `{loc}` — {f.get('problem', '')}\n"
            f"  - evidence: {f.get('evidence', '')}\n"
            f"  - fix: {f.get('suggested_fix', '')}"
        )
    return rows


def render_critique(pair: str, stage: str, round_no: int, c: dict) -> str:
    """Shared renderer for plan critiques and checkpoint reviews — the
    fields differ per schema; absent ones are simply not rendered."""
    rows = _findings_rows(c.get("findings", []))
    parts = [
        f"# {stage} — round {round_no} (pair: {pair})\n",
        f"**Verdict: {c.get('verdict', '?')}**"
        + (
            f" · tests adequate: {c.get('tests_adequate')}"
            if "tests_adequate" in c
            else ""
        )
        + "\n",
        "## Findings\n",
        "\n".join(rows) if rows else "_None._\n",
    ]
    if c.get("missing_evidence"):
        parts.append(_section("Missing evidence", c["missing_evidence"]))
    if c.get("implementation_checks"):
        checks = [
            f"**{item.get('action', 'add')}** `{item.get('key', '?')}`"
            + (
                f" (step: {item.get('target_step')})"
                if item.get("target_step")
                else " (cross-cutting)"
            )
            + f": {item.get('description', '')} — {item.get('evidence', '')}"
            for item in c["implementation_checks"]
        ]
        parts.append(_section("Implementation checks captured", checks))
    if c.get("simpler_alternative"):
        parts.append(f"## Simpler alternative\n\n{c['simpler_alternative']}\n")
    if c.get("requires_human_decision"):
        parts.append(
            "## Human decision required\n\n"
            f"{c.get('decision_question') or 'Unspecified decision.'}\n"
        )
    if c.get("tests_critique"):
        parts.append(f"## Tests critique\n\n{c['tests_critique']}\n")
    if c.get("notes"):
        parts.append(f"## Notes\n\n{c['notes']}\n")
    return "\n".join(parts)


# ------------------------------------------------------------ implementation
def render_implementation_report(lead: str, r: dict) -> str:
    commits = [
        f"`{c.get('sha', '')[:12]}` {c.get('message', '')}" for c in r.get("commits", [])
    ]
    return "\n".join(
        [
            f"# Implementation report (lead: {lead})\n",
            f"**Tests passed: {r.get('tests_passed', '?')}** · command: "
            f"`{r.get('tests_command', '')}`\n",
            _section("Commits", commits),
            _section("Files changed", r.get("files_changed", [])),
            _section("Deviations from plan", r.get("deviations_from_plan", [])),
            f"## Test output summary\n\n{r.get('test_output_summary', '')}\n",
            f"## Notes\n\n{r.get('notes', '')}\n",
        ]
    )


def render_verification(pair: str, v: dict) -> str:
    crit = [
        f"{'✅' if c.get('met') else '❌'} {c.get('criterion', '?')} — "
        f"{c.get('evidence', '')}"
        for c in v.get("criteria", [])
    ]
    return "\n".join(
        [
            f"# Final verification (verifier: {pair}, fresh context)\n",
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
