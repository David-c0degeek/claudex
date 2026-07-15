"""Canonical plan hashing and validated structured section replacement."""

from __future__ import annotations

import hashlib
import json


PLAN_FIELDS = ("plan_markdown", "steps", "risks", "open_questions")


class PlanDeltaError(ValueError):
    pass


def canonical_plan(value: dict) -> dict:
    return {key: value.get(key) for key in PLAN_FIELDS}


def plan_digest(value: dict) -> str:
    encoded = json.dumps(
        canonical_plan(value),
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def apply_revision_delta(baseline: dict, delta: dict) -> dict:
    expected = plan_digest(baseline)
    actual = str(delta.get("base_plan_sha256", ""))
    if actual != expected:
        raise PlanDeltaError(
            f"revision baseline hash mismatch: expected {expected}, "
            f"got {actual or '<empty>'}"
        )
    revised = canonical_plan(baseline)
    for key in PLAN_FIELDS:
        replacement = delta.get(key)
        if replacement is not None:
            revised[key] = replacement
    return revised
