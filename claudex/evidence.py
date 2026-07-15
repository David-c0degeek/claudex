"""Size-bounded, manifest-backed evidence packets for fresh reviewers."""

from __future__ import annotations

import hashlib
import json
import shutil
from pathlib import Path

from .security import redact_value, scrub_file


class EvidenceError(ValueError):
    pass


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _safe_repo_file(repo: Path, value: str) -> Path | None:
    relative = Path(value)
    if relative.is_absolute() or ".." in relative.parts:
        return None
    candidate = (repo / value).resolve()
    try:
        candidate.relative_to(repo.resolve())
    except ValueError:
        return None
    return candidate if candidate.is_file() else None


def requested_repo_paths(cfg, values: list[str]) -> list[str]:
    """Accept only explicit existing repo-relative file requests."""
    accepted: list[str] = []
    for raw in values[: cfg.max_evidence_requests]:
        value = str(raw).strip().replace("\\", "/")
        if value and _safe_repo_file(cfg.repo, value) and value not in accepted:
            accepted.append(value)
    return accepted


def build_plan_review_packet(
    cfg, state, run_dir: Path, plan_path: Path, *, round_no: int | None = None
) -> Path:
    round_no = state.plan_round if round_no is None else round_no
    suffix = f"{round_no}-e{state.evidence_expansions}"
    manifest = run_dir / f"evidence-plan-review-{suffix}.json"
    if manifest.exists():
        return manifest
    packet_root = run_dir / "evidence" / f"plan-review-{suffix}"
    packet_root.mkdir(parents=True)

    decisions = packet_root / "decision-ledger.json"
    decisions.write_text(
        json.dumps(redact_value({"binding_guidance": state.binding_guidance}), indent=2),
        encoding="utf-8",
    )
    findings = packet_root / "finding-ledger.json"
    findings.write_text(
        json.dumps(redact_value(
            {
                "accepted": state.accepted_findings,
                "unresolved": state.unresolved_findings,
            },
        ), indent=2,
        ),
        encoding="utf-8",
    )
    repo_facts = packet_root / "repo-facts.json"
    repo_facts.write_text(
        json.dumps(
            {
                "base_commit": state.base_commit,
                "mode": state.mode,
                "requested_paths": state.evidence_requests,
            },
            indent=2,
        ),
        encoding="utf-8",
    )

    plan = json.loads(plan_path.read_text(encoding="utf-8"))
    selected = list(state.evidence_requests)
    for step in plan.get("steps", []):
        for value in step.get("files", []):
            value = str(value).strip().replace("\\", "/")
            if value and value not in selected:
                selected.append(value)

    sources: list[tuple[str, Path, bool]] = [
        ("task", run_dir / "task.md", True),
        ("canonical_plan", plan_path, True),
        ("decision_ledger", decisions, True),
        ("finding_ledger", findings, True),
        ("repo_facts", repo_facts, True),
    ]
    checks = run_dir / "implementation-checks.json"
    if checks.exists():
        sources.append(("implementation_checks", checks, False))

    omissions: list[dict] = []
    for relative in selected:
        source = _safe_repo_file(cfg.repo, relative)
        if not source:
            omissions.append({"path": relative, "reason": "not an existing file"})
            continue
        target = packet_root / "repo" / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
        scrub_file(target)
        sources.append((f"repo:{relative}", target, False))

    entries: list[dict] = []
    total = 0
    for purpose, path, required in sources:
        size = path.stat().st_size
        if size > cfg.max_evidence_file_bytes:
            if required:
                raise EvidenceError(
                    f"required evidence {purpose} is {size} bytes; per-file cap is "
                    f"{cfg.max_evidence_file_bytes}"
                )
            omissions.append({"path": str(path), "reason": "per-file size cap"})
            path.unlink(missing_ok=True)
            continue
        if total + size > cfg.max_evidence_bytes:
            if required:
                raise EvidenceError(
                    f"required evidence exceeds packet cap {cfg.max_evidence_bytes} bytes"
                )
            omissions.append({"path": str(path), "reason": "packet size cap"})
            path.unlink(missing_ok=True)
            continue
        total += size
        entries.append(
            {
                "purpose": purpose,
                "path": str(path.resolve()),
                "bytes": size,
                "sha256": _sha256(path),
                "required": required,
            }
        )

    manifest.write_text(
        json.dumps(
            {
                "schema_version": 1,
                "kind": "plan_review",
                "round": round_no,
                "max_bytes": cfg.max_evidence_bytes,
                "total_bytes": total,
                "entries": entries,
                "omissions": omissions,
                "request_contract": (
                    "If decisive repository evidence is omitted, return only existing "
                    "repo-relative file paths in missing_evidence. Do not browse outside "
                    "this manifest."
                ),
            },
            indent=2,
        ),
        encoding="utf-8",
    )
    return manifest
