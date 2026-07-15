"""Non-lossy restart checkpoint cloning and equivalence evidence."""

from __future__ import annotations

import copy
import dataclasses
import hashlib
import json
import shutil
import time
from pathlib import Path

from .lifecycle import Lifecycle
from .state import Phase, RunState


_SKIP_NAMES = {"state.json", "state.json.tmp", "run.lock", "attempts"}
_CHECKPOINT_FIELDS = (
    "phase",
    "mode",
    "base_commit",
    "branch",
    "worktree",
    "steps",
    "step_index",
    "plan_round",
    "plan_revisions",
    "checkpoint_round",
    "fix_round",
    "test_round",
    "verify_round",
    "checkpoint_fixes_used",
    "test_fixes_used",
    "verify_fixes_used",
    "plan_cap_extra",
    "checkpoint_cap_extra",
    "test_cap_extra",
    "verify_cap_extra",
    "canonical_plan_round",
    "canonical_plan_sha256",
    "accepted_findings",
    "unresolved_findings",
    "binding_guidance",
    "implementation_checks",
    "budget_overrides",
    "run_policy",
    "provider_usage",
    "provider_costs",
    "provider_cost_usd",
    "provider_invocations",
    "provider_duration_s",
    "provider_tool_calls",
    "accounted_attempt_ids",
    "sessions",
    "last_reviewed_commit",
    "mailbox_turn",
    "findings_file",
    "fix_return",
)


def clone_state(old: RunState, new_run_id: str, *, fresh_plan: bool = False) -> RunState:
    data = copy.deepcopy(dataclasses.asdict(old))
    data["run_id"] = new_run_id
    data["predecessor_run_id"] = old.run_id
    data["driver"] = "headless"
    data["active_attempt_id"] = ""
    data["sessions"] = {
        key: value
        for key, value in old.sessions.items()
        if key in ("lead_impl", "pair_review") and old.worktree
    }
    if fresh_plan:
        data.update(
            {
                "phase": Phase.PLAN_DRAFT.value,
                "lifecycle": Lifecycle.RUNNING.value,
                "sessions": {},
                "steps": [],
                "step_index": 0,
                "plan_round": 0,
                "plan_revisions": 0,
                "plan_cap_extra": 0,
                "canonical_plan_round": -1,
                "canonical_plan_sha256": "",
                "accepted_findings": [],
                "unresolved_findings": [],
                "implementation_checks": [],
                "evidence_requests": [],
                "evidence_expansions": 0,
                "last_evidence_manifest": "",
                "findings_file": "",
                "fix_return": "",
                "gate_reason": "",
                "gate_kind": "",
                "return_phase": "",
                "error": "",
                "failed_phase": "",
            }
        )
    known = {field.name for field in dataclasses.fields(RunState)}
    replacement = RunState(**{key: value for key, value in data.items() if key in known})
    replacement.log(
        f"restart from {old.run_id}"
        + (" with explicit fresh plan" if fresh_plan else " from canonical checkpoint")
    )
    replacement.lifecycle_history.append(
        {
            "version": 1,
            "ts": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
            "from": old.lifecycle,
            "to": replacement.lifecycle,
            "reason": "replacement execution identity created",
            "phase": replacement.phase,
            "attempt_id": "",
            "resume_instruction": "claudex resume",
        }
    )
    return replacement


def copy_checkpoint_artifacts(old_dir: Path, new_dir: Path) -> None:
    new_dir.mkdir(parents=True, exist_ok=True)
    for source in old_dir.iterdir():
        if source.name in _SKIP_NAMES or source.name.startswith("state.v"):
            continue
        target = new_dir / source.name
        if source.is_dir():
            shutil.copytree(source, target, dirs_exist_ok=True)
        else:
            shutil.copy2(source, target)


def discard_plan_artifacts(run_dir: Path) -> None:
    """Remove only known planning artifacts after explicit --fresh-plan."""
    prefixes = (
        "plan-round-",
        "plan-revision-",
        "plan-critique-",
        "evidence-plan-review-",
    )
    for path in run_dir.iterdir():
        if path.name in (
            "agreed-plan.json",
            "agreed-plan.md",
            "implementation-checks.json",
        ) or path.name.startswith(prefixes):
            if path.is_file():
                path.unlink()
            elif path.is_dir():
                shutil.rmtree(path)
    evidence_root = run_dir / "evidence"
    if evidence_root.exists():
        shutil.rmtree(evidence_root)


def remap_run_paths(state: RunState, old_dir: Path, new_dir: Path) -> None:
    if state.findings_file:
        path = Path(state.findings_file)
        try:
            relative = path.resolve().relative_to(old_dir.resolve())
        except (OSError, ValueError):
            return
        state.findings_file = str(new_dir / relative)


def checkpoint_digest(state: RunState, run_dir: Path) -> str:
    payload = {field: getattr(state, field) for field in _CHECKPOINT_FIELDS}
    payload["findings_file"] = (
        Path(state.findings_file).name if state.findings_file else ""
    )
    payload["sessions"] = {
        key: value
        for key, value in state.sessions.items()
        if key in ("lead_impl", "pair_review") and state.worktree
    }
    files: dict[str, str] = {}
    candidates = {
        "task.md",
        "agreed-plan.json",
        "implementation-checks.json",
    }
    if state.canonical_plan_round >= 0:
        candidates.add(f"plan-round-{state.canonical_plan_round}.json")
    if state.findings_file:
        candidates.add(Path(state.findings_file).name)
    for name in sorted(candidates):
        path = run_dir / name
        if path.is_file():
            files[name] = hashlib.sha256(path.read_bytes()).hexdigest()
    payload["files"] = files
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), default=str)
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()
