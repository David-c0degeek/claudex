"""JSON Schemas for structured agent findings.

All schemas are written in OpenAI strict mode style (``additionalProperties:
false``, every property listed in ``required``) because Codex enforces that
server-side for ``--output-schema``. Claude Code's ``--json-schema`` accepts
the same schemas, so both agents share one set of contracts and the
coordinator never parses free-form prose.
"""

from __future__ import annotations


def _obj(properties: dict, required: list[str] | None = None) -> dict:
    return {
        "type": "object",
        "properties": properties,
        "required": required if required is not None else list(properties),
        "additionalProperties": False,
    }


def _arr(items: dict) -> dict:
    return {"type": "array", "items": items}


_STR = {"type": "string"}
_BOOL = {"type": "boolean"}

TASK_CONTRACT_SCHEMA = _obj(
    {
        "goal": {"type": "string", "description": "What should be true when done"},
        "current_behavior": _STR,
        "desired_behavior": _STR,
        "scope": _STR,
        "non_goals": _arr(_STR),
        "constraints": _arr(_STR),
        "acceptance_criteria": _arr(_STR),
        "required_tests": _arr(_STR),
        "relevant_files": _arr(_STR),
        "open_questions": _arr(_STR),
    }
)

EVIDENCE_ITEM = _obj(
    {
        "file": _STR,
        "symbol": _STR,
        "claim": _STR,
    }
)

ANALYSIS_SCHEMA = _obj(
    {
        "summary": _STR,
        "execution_paths": _arr(_STR),
        "evidence": _arr(EVIDENCE_ITEM),
        "assumptions": _arr(_STR),
        "risks": _arr(_STR),
        "proposed_solution": {
            "type": "string",
            "description": "Minimal justified solution, markdown, concrete file-level steps",
        },
        "rejected_alternatives": _arr(
            _obj({"alternative": _STR, "reason_rejected": _STR})
        ),
        "tests_required": _arr(_STR),
        "open_questions": _arr(_STR),
    }
)

REVIEW_REPORT_SCHEMA = _obj(
    {
        "summary": _STR,
        "report_markdown": {
            "type": "string",
            "description": "The complete review report, markdown, self-contained",
        },
        "findings": _arr(
            _obj(
                {
                    "severity": {
                        "type": "string",
                        "enum": ["blocking", "major", "minor", "info"],
                    },
                    "area": _STR,
                    "finding": _STR,
                    "evidence": _STR,
                }
            )
        ),
        "open_questions": _arr(_STR),
    }
)

DISAGREEMENT_SCHEMA = _obj(
    {
        "agreements": _arr(_STR),
        "conflicts": _arr(
            _obj(
                {
                    "topic": _STR,
                    "claude_position": _STR,
                    "codex_position": _STR,
                    "repo_evidence": {
                        "type": "string",
                        "description": "What the repository itself shows, checked directly",
                    },
                    "recommendation": _STR,
                }
            )
        ),
        "unique_to_claude": _arr(_STR),
        "unique_to_codex": _arr(_STR),
        "recommended_plan": {"type": "string", "enum": ["claude", "codex"]},
        "recommendation_rationale": _STR,
    }
)

PLAN_REVIEW_SCHEMA = _obj(
    {
        "confirmed_claims": _arr(_STR),
        "disputed_claims": _arr(
            _obj({"claim": _STR, "why_disputed": _STR, "repo_evidence": _STR})
        ),
        "missing_evidence": _arr(_STR),
        "blocking_corrections": _arr(_STR),
        "non_blocking_suggestions": _arr(_STR),
        "simpler_alternative": {
            "type": ["string", "null"],
            "description": "A simpler valid plan if one exists, else null",
        },
        "verdict": {"type": "string", "enum": ["approve", "revise"]},
    }
)

FINAL_PLAN_SCHEMA = _obj(
    {
        "final_plan": {
            "type": "string",
            "description": "Complete agreed plan, markdown, self-contained",
        },
        "blocking_corrections_addressed": _arr(
            _obj({"correction": _STR, "resolution": _STR})
        ),
    }
)

IMPLEMENTATION_REPORT_SCHEMA = _obj(
    {
        "commits": _arr(_obj({"sha": _STR, "message": _STR})),
        "files_changed": _arr(_STR),
        "tests_command": _STR,
        "tests_passed": _BOOL,
        "test_output_summary": _STR,
        "deviations_from_plan": _arr(_STR),
        "notes": _STR,
    }
)

CODE_REVIEW_SCHEMA = _obj(
    {
        "findings": _arr(
            _obj(
                {
                    "severity": {
                        "type": "string",
                        "enum": ["blocking", "major", "minor", "nit"],
                    },
                    "file": _STR,
                    "line": {"type": ["integer", "null"]},
                    "problem": _STR,
                    "evidence": _STR,
                    "suggested_fix": _STR,
                }
            )
        ),
        "tests_adequate": _BOOL,
        "tests_critique": _STR,
        "verdict": {"type": "string", "enum": ["approve", "request_changes"]},
    }
)

VERIFICATION_SCHEMA = _obj(
    {
        "criteria": _arr(
            _obj(
                {
                    "criterion": _STR,
                    "met": _BOOL,
                    "evidence": {
                        "type": "string",
                        "description": "Concrete file/test evidence, not agent testimony",
                    },
                }
            )
        ),
        "scope_expansion": _arr(_STR),
        "tests_meaningful": _BOOL,
        "unsupported_claims": _arr(_STR),
        "verdict": {"type": "string", "enum": ["pass", "fail"]},
        "notes": _STR,
    }
)
