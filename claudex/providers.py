"""Deterministic provider discovery and capability preflight."""

from __future__ import annotations

import glob
import os
import re
import shutil
import subprocess
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Callable


class ProviderError(RuntimeError):
    pass


@dataclass(frozen=True)
class ProviderInfo:
    provider: str
    path: str
    source: str
    version: str
    version_tuple: tuple[int, ...]
    capabilities: dict[str, bool]
    compatible: bool
    missing: tuple[str, ...]

    def to_dict(self) -> dict:
        value = asdict(self)
        value["version_tuple"] = list(self.version_tuple)
        return value


Runner = Callable[..., subprocess.CompletedProcess]


def _wrap(binary: str) -> list[str]:
    lowered = binary.lower()
    if os.name == "nt" and lowered.endswith((".cmd", ".bat")):
        return ["cmd.exe", "/d", "/s", "/c", binary]
    if os.name == "nt" and lowered.endswith(".ps1"):
        return ["powershell.exe", "-NoProfile", "-File", binary]
    return [binary]


def _invoke(runner: Runner, cmd: list[str]) -> subprocess.CompletedProcess:
    return runner(
        cmd,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=60,
    )


def _version_tuple(text: str) -> tuple[int, ...]:
    match = re.search(r"(?<!\d)(\d+)\.(\d+)(?:\.(\d+))?", text)
    if not match:
        return ()
    return tuple(int(value or 0) for value in match.groups())


def probe_provider(
    provider: str,
    path: str,
    *,
    source: str = "candidate",
    runner: Runner = subprocess.run,
) -> ProviderInfo:
    if provider not in ("claude", "codex"):
        raise ValueError(f"unknown provider: {provider}")
    try:
        version_proc = _invoke(runner, [*_wrap(path), "--version"])
        help_commands = [[*_wrap(path), "--help"]]
        if provider == "codex":
            help_commands.append([*_wrap(path), "exec", "--help"])
        help_text = ""
        for command in help_commands:
            proc = _invoke(runner, command)
            help_text += "\n" + (proc.stdout or "") + "\n" + (proc.stderr or "")
        version_text = (version_proc.stdout or version_proc.stderr or "").strip()
    except (OSError, subprocess.SubprocessError) as exc:
        raise ProviderError(f"could not probe {provider} at {path}: {exc}") from exc
    version = _version_tuple(version_text)
    if not version:
        raise ProviderError(
            f"{provider} at {path} returned an unparseable version: {version_text!r}"
        )
    if provider == "claude":
        capabilities = {
            "stream": "stream-json" in help_text and "--output-format" in help_text,
            "schema": "--json-schema" in help_text,
            "budget": "--max-budget-usd" in help_text,
            "sandbox": "--permission-mode" in help_text,
            "session": "--resume" in help_text,
            "nested_agent_control": "--disallowedTools".lower() in help_text.lower(),
        }
        required = tuple(capabilities)
    else:
        capabilities = {
            "stream": "--json" in help_text,
            "schema": "--output-schema" in help_text,
            # Codex has no native dollar cap; the coordinator performs admission.
            "budget": False,
            "sandbox": "--sandbox" in help_text or "-s," in help_text,
            "session": "resume" in help_text,
            "nested_agent_control": "--disable" in help_text,
        }
        required = ("stream", "schema", "sandbox", "session", "nested_agent_control")
    missing = tuple(name for name in required if not capabilities[name])
    return ProviderInfo(
        provider=provider,
        path=path,
        source=source,
        version=version_text,
        version_tuple=version,
        capabilities=capabilities,
        compatible=not missing,
        missing=missing,
    )


def provider_candidates(provider: str) -> list[tuple[str, str]]:
    candidates: list[tuple[str, str]] = []
    found = shutil.which(provider)
    if found:
        candidates.append((found, "PATH"))
    if os.name == "nt":
        local = os.environ.get("LOCALAPPDATA", "")
        if local and provider == "codex":
            for path in glob.glob(
                os.path.join(local, "OpenAI", "Codex", "bin", "*", "codex.exe")
            ):
                candidates.append((path, "desktop"))
        if local and provider == "claude":
            for path in glob.glob(os.path.join(local, "Programs", "claude*", "claude.exe")):
                candidates.append((path, "desktop"))
    unique: dict[str, tuple[str, str]] = {}
    for path, source in candidates:
        key = os.path.normcase(os.path.abspath(path))
        unique.setdefault(key, (path, source))
    return list(unique.values())


def resolve_provider(
    provider: str,
    configured: str | None = None,
    *,
    runner: Runner = subprocess.run,
    candidates: list[tuple[str, str]] | None = None,
) -> ProviderInfo:
    env_name = f"CLAUDEX_{provider.upper()}_BIN"
    explicit = os.environ.get(env_name) or configured
    if explicit:
        info = probe_provider(
            provider,
            explicit,
            source="environment" if os.environ.get(env_name) else "config",
            runner=runner,
        )
        if not info.compatible:
            raise ProviderError(
                f"explicit {provider} executable {explicit} is incompatible; missing "
                f"capabilities: {', '.join(info.missing)}. Upgrade it or change {env_name}."
            )
        return info

    discovered = candidates if candidates is not None else provider_candidates(provider)
    if not discovered:
        raise ProviderError(
            f"{provider} CLI not found (install it, add it to PATH, or set {env_name})"
        )
    compatible: list[ProviderInfo] = []
    failures: list[str] = []
    for path, source in discovered:
        try:
            info = probe_provider(provider, path, source=source, runner=runner)
        except ProviderError as exc:
            failures.append(str(exc))
            continue
        if info.compatible:
            compatible.append(info)
        else:
            failures.append(f"{path}: missing {', '.join(info.missing)}")
    if not compatible:
        detail = "; ".join(failures) or "no candidates were probeable"
        raise ProviderError(f"no compatible {provider} executable: {detail}")
    # Semantic version wins. PATH wins an exact-version tie, then stable path.
    compatible.sort(
        key=lambda item: (
            item.version_tuple,
            item.source == "PATH",
            os.path.normcase(str(Path(item.path))),
        ),
        reverse=True,
    )
    return compatible[0]

