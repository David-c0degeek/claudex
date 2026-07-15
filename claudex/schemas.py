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


class ProtocolViolation(ValueError):
    """Structured provider output is internally inconsistent."""


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

PLAN_FINDING = _obj(
    {
        "key": {
            "type": "string",
            "description": "Stable lowercase-hyphen identifier; reuse it when the same issue recurs",
        },
        "kind": {
            "type": "string",
            "enum": ["new", "repeated", "regression", "decision"],
        },
        "category": {
            "type": "string",
            "enum": [
                "architecture",
                "scope",
                "sequencing",
                "safety",
                "validation",
                "decision",
            ],
            "description": "The plan-level concern; content facts belong in implementation_checks",
        },
        **FINDING["properties"],
    }
)

IMPLEMENTATION_CHECK = _obj(
    {
        "key": {
            "type": "string",
            "description": "Stable lowercase-hyphen identifier",
        },
        "description": {
            "type": "string",
            "description": "Concrete fact, edge case, or content requirement to verify while implementing",
        },
        "evidence": _STR,
        "action": {
            "type": "string",
            "enum": ["add", "remove"],
            "description": "Add/update this obligation, or retract it with evidence",
        },
        "target_step": {
            "type": ["string", "null"],
            "description": "Exact plan step title, or null when cross-cutting",
        },
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
            "description": "Architecture, decisions, delivery contract, and rationale; do not duplicate structured steps, risks, or open questions",
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
        "findings": _arr(PLAN_FINDING),
        "implementation_checks": {
            "type": "array",
            "items": IMPLEMENTATION_CHECK,
            "description": "Non-plan-blocking obligations carried into implementation and verification",
        },
        "requires_human_decision": {
            "type": "boolean",
            "description": "True only for an unresolved value choice or repeated evidence-backed disagreement",
        },
        "decision_question": {
            "type": ["string", "null"],
            "description": "The concrete question for the human, else null",
        },
        "missing_evidence": {
            "type": "array",
            "items": _STR,
            "description": "Existing repo-relative file paths omitted from the evidence manifest; empty when review is conclusive",
        },
        "simpler_alternative": {
            "type": ["string", "null"],
            "description": "A simpler valid plan if one exists, else null",
        },
        "notes": _STR,
    }
)

# Plan revisions use hash-guarded section replacement. Null preserves the
# canonical baseline section.
PLAN_REVISION_SCHEMA = _obj(
    {
        "base_plan_sha256": _STR,
        "plan_markdown": {
            "type": ["string", "null"],
            "description": "Replacement architecture/decisions, or null to preserve the baseline section",
        },
        "steps": {
            "type": ["array", "null"],
            "items": _PLAN_STEP,
            "description": "Replacement full step list, or null to preserve it",
        },
        "risks": {"type": ["array", "null"], "items": _STR},
        "open_questions": {"type": ["array", "null"], "items": _STR},
        "responses": _arr(
            _obj(
                {
                    "finding_key": _STR,
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
    """The stop rule: an internally supported AGREE with no actionable
    findings. Evidence or test-assessment gaps are not convergence."""
    if critique.get("verdict") != "AGREE":
        return False
    if critique.get("missing_evidence"):
        return False
    if critique.get("tests_adequate") is False:
        return False
    return not has_actionable_findings(critique)


def has_actionable_findings(review: dict) -> bool:
    return any(
        f.get("severity") in ("blocking", "major")
        for f in review.get("findings", [])
    )


def is_inconclusive_review(review: dict) -> bool:
    """True when the reviewer refused agreement but supplied no defect the
    lead can act on, or admitted it lacked the evidence needed to review.
    Such a turn must retry the reviewer, not rewrite/fix the artifact."""
    if requires_human_decision(review) or has_actionable_findings(review):
        return False
    return bool(
        review.get("verdict") != "AGREE"
        or review.get("missing_evidence")
        or review.get("tests_adequate") is False
    )


def requires_human_decision(critique: dict) -> bool:
    """Accept only an explicit flag paired with one concrete question.

    Finding labels never synthesize human intent. A mismatched flag/question
    pair is a provider protocol failure so it cannot become either a false gate
    or an unanswerable gate.
    """
    requested = critique.get("requires_human_decision") is True
    question = critique.get("decision_question")
    has_question = isinstance(question, str) and bool(question.strip())
    if requested != has_question:
        raise ProtocolViolation(
            "requires_human_decision and decision_question are inconsistent: "
            "a human gate requires explicit true plus a concrete non-empty question"
        )
    return requested
