"""Secret redaction and raw-attempt retention without touching recovery state."""

from __future__ import annotations

import re
import time
from pathlib import Path


_PATTERNS = (
    re.compile(r"(?i)\b(authorization\s*:\s*bearer\s+)[^\s\"']+"),
    re.compile(r"(?i)\b((?:api[_-]?key|token|secret|password)\s*[=:]\s*)[^\s,;\"']+"),
    re.compile(r"\b(?:sk|sk-ant|ghp|github_pat|xox[baprs])[-_A-Za-z0-9]{12,}\b"),
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
)


def redact_text(value: str) -> str:
    text = value
    for pattern in _PATTERNS:
        if pattern.groups:
            text = pattern.sub(r"\1[REDACTED]", text)
        else:
            text = pattern.sub("[REDACTED]", text)
    return text


def redact_value(value):
    if isinstance(value, str):
        return redact_text(value)
    if isinstance(value, dict):
        redacted = {}
        for key, item in value.items():
            normalized = re.sub(r"[^a-z]", "", str(key).lower())
            if normalized in {
                "apikey", "token", "accesstoken", "authtoken", "secret",
                "clientsecret", "password", "authorization",
            }:
                redacted[key] = "[REDACTED]"
            else:
                redacted[key] = redact_value(item)
        return redacted
    if isinstance(value, list):
        return [redact_value(item) for item in value]
    if isinstance(value, tuple):
        return tuple(redact_value(item) for item in value)
    return value


def write_redacted_text(path: Path, value: str) -> None:
    path.write_text(redact_text(value), encoding="utf-8")


def scrub_file(path: Path) -> None:
    if not path.exists() or not path.is_file():
        return
    try:
        original = path.read_text(encoding="utf-8", errors="replace")
        scrubbed = redact_text(original)
        if scrubbed != original:
            path.write_text(scrubbed, encoding="utf-8")
    except OSError:
        return


def prune_raw_attempts(
    claudex_root: Path, *, max_age_days: int, max_bytes: int
) -> dict[str, object]:
    """Prune only raw streams; normalized events/results/summaries remain."""
    raw_names = {"stdout.jsonl", "stderr.log", "last-message.txt"}
    files: list[Path] = []
    errors: list[str] = []
    candidates = list(claudex_root.glob("runs/*/attempts/*/*"))
    candidates.extend(claudex_root.glob("attempts/*/*"))
    for path in candidates:
        try:
            if path.is_file() and path.name in raw_names:
                files.append(path)
        except OSError as exc:
            errors.append(f"{path}: {exc}")
    def modified(path: Path) -> float:
        try:
            return path.stat().st_mtime
        except OSError as exc:
            errors.append(f"{path}: {exc}")
            return float("inf")
    files.sort(key=lambda path: (modified(path), str(path)))
    removed: list[str] = []
    cutoff = time.time() - max_age_days * 86400
    for path in list(files):
        try:
            if path.stat().st_mtime < cutoff:
                path.unlink()
                files.remove(path)
                removed.append(str(path))
        except OSError as exc:
            errors.append(f"{path}: {exc}")
    sizes: dict[Path, int] = {}
    for path in files:
        try:
            if path.exists():
                sizes[path] = path.stat().st_size
        except OSError as exc:
            errors.append(f"{path}: {exc}")
    total = sum(sizes.values())
    for path in files:
        if total <= max_bytes:
            break
        try:
            size = sizes.get(path)
            if size is None:
                size = path.stat().st_size
            path.unlink()
            total -= size
            removed.append(str(path))
        except OSError as exc:
            errors.append(f"{path}: {exc}")
    return {"removed": removed, "errors": errors, "remaining_raw_bytes": total}
