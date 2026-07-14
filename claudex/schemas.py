"""JSON Schemas for structured agent findings.

All schemas are written in OpenAI strict mode style (``additionalProperties:
false``, every property listed in ``required``) because Codex enforces that
server-side for ``--output-schema``. Claude Code's ``--json-schema`` accepts
the same schemas, so both agents share one set of contracts and the
coordinator never parses free-form prose.

Strict mode forces agents to emit every field, so plan critique and
checkpoint review get separate thin wrappers around one shared FINDING item
instead of a merged schema full of noise fields.
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

# One finding shape for every critique the pair produces, plan or code.
# `file`/`line` are nullable: plan findings reference plan sections, not code.
FINDING = _obj(
    {
        "severity": {
            "type": "string",
            "enum": ["blocking", "major", "minor", "nit"],
        },
        "file": {"type": ["string", "null"]},
        "line": {"type": ["integer", "null"]},
        "problem": _STR,
        "evidence": _STR,
        "suggested_fix": _STR,
    }
)

_PLAN_STEP = _obj(
    {
        "title": _STR,
        "description": {
            "type": "string",
            "description": "Concrete file-level work; independently implementable and committable",
        },
        "files": _arr(_STR),
        "tests": _arr(_STR),
    }
)

# The lead's plan proposal: the single artifact both agents converge on.
PAIR_PLAN_SCHEMA = _obj(
    {
        "plan_markdown": {
            "type": "string",
            "description": "Complete plan, markdown, self-contained",
        },
        "steps": {
            "type": "array",
            "items": _PLAN_STEP,
            "description": "Ordered implementation steps; each becomes one commit + checkpoint review",
        },
        "risks": _arr(_STR),
        "open_questions": _arr(_STR),
    }
)

PLAN_CRITIQUE_SCHEMA = _obj(
    {
        "verdict": {"type": "string", "enum": ["AGREE", "REVISE"]},
        "findings": _arr(FINDING),
        "missing_evidence": _arr(_STR),
        "simpler_alternative": {
            "type": ["string", "null"],
            "description": "A simpler valid plan if one exists, else null",
        },
        "notes": _STR,
    }
)

# Plan revisions re-emit the FULL plan (markdown AND steps), never a delta —
# otherwise state.steps drifts from plan_markdown across rounds.
PLAN_REVISION_SCHEMA = _obj(
    {
        "plan_markdown": {
            "type": "string",
            "description": "Complete REVISED plan, markdown, self-contained",
        },
        "steps": {
            "type": "array",
            "items": _PLAN_STEP,
            "description": "Full revised step list, not a delta",
        },
        "risks": _arr(_STR),
        "open_questions": _arr(_STR),
        "responses": _arr(
            _obj(
                {
                    "finding": _STR,
                    "action": {"type": "string", "enum": ["accepted", "rebutted"]},
                    "rationale": {
                        "type": "string",
                        "description": "For rebuttals: direct repository evidence, or it does not count",
                    },
                }
            )
        ),
    }
)

CHECKPOINT_REVIEW_SCHEMA = _obj(
    {
        "verdict": {"type": "string", "enum": ["AGREE", "REVISE"]},
        "findings": _arr(FINDING),
        "tests_adequate": _BOOL,
        "tests_critique": _STR,
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


def is_converged(critique: dict) -> bool:
    """The stop rule, defined once: AGREE with zero blocking/major findings."""
    if critique.get("verdict") != "AGREE":
        return False
    return not any(
        f.get("severity") in ("blocking", "major")
        for f in critique.get("findings", [])
    )
