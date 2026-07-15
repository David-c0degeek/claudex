"""Provider command builders, streamed adapters, and typed final results.

The subprocess lifecycle lives in :mod:`claudex.processes`; provider JSONL is
normalized by :mod:`claudex.provider_events`.  This module owns only the Claude
and Codex command/result contracts and binary resolution.
"""

from __future__ import annotations

import json
import os
import sys
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Callable

from .budgets import InvocationPolicy, normalize_usage
from .events import AgentEvent
from .limits import classify_limit
from .processes import (
    AttemptPaths,
    ExecResult,
    ProcessExecutionError,
    ProcessCancelledError,
    ProcessTimeoutError,
    atomic_json,
    stream_process,
)
from .provider_events import decode_claude_event, decode_codex_event
from .providers import ProviderError, resolve_provider
from .security import scrub_file


class AgentError(RuntimeError):
    pass


class AgentCancelled(AgentError):
    pass


class AgentTimeout(AgentError):
    pass


def _translate_process_error(exc: ProcessExecutionError) -> AgentError:
    if isinstance(exc, ProcessCancelledError):
        return AgentCancelled(str(exc))
    if isinstance(exc, ProcessTimeoutError):
        return AgentTimeout(str(exc))
    return AgentError(str(exc))


@dataclass
class AgentResult:
    ok: bool
    accounting_schema_version: int = 1
    text: str = ""
    structured: dict | None = None
    session_id: str = ""
    exit_code: int = 0
    duration_s: float = 0.0
    error: str = ""
    stdout_path: str = ""
    stderr_path: str = ""
    attempt_id: str = ""
    events_path: str = ""
    usage: dict = field(default_factory=dict)
    normalized_usage: dict = field(default_factory=dict)
    usage_quality: str = "unknown"
    reported_cost: float | None = None
    cost_usd: float | None = None
    num_turns: int | None = None
    model: str = ""
    tool_calls: int = 0
    tool_calls_observed: bool = False
    usage_source: str = "provider_terminal"
    cost_currency: str = ""
    cost_quality: str = "unknown"

    def require_structured(self) -> dict:
        if not self.ok:
            raise AgentError(self.error or "agent invocation failed")
        if self.structured is None:
            raise AgentError(
                f"agent returned no structured output (see {self.stdout_path})"
            )
        return self.structured


def _windows() -> bool:
    return os.name == "nt"


def _wrap_script(binary: str) -> list[str]:
    """CreateProcess cannot launch .cmd/.bat/.ps1 directly."""
    lowered = binary.lower()
    if _windows() and lowered.endswith((".cmd", ".bat")):
        return ["cmd.exe", "/c", binary]
    if _windows() and lowered.endswith(".ps1"):
        return ["powershell.exe", "-NoProfile", "-File", binary]
    if lowered.endswith(".py"):
        return [sys.executable, binary]
    return [binary]


def _truncate(value: object, limit: int = 500) -> str:
    text = str(value or "").replace("\r", "").strip()
    return text if len(text) <= limit else text[: limit - 1] + "…"


def _finalize_attempt(execution: ExecResult, result: AgentResult) -> AgentResult:
    cost = result.reported_cost
    if cost is None and result.cost_usd is not None:
        cost = result.cost_usd
    result.normalized_usage = normalize_usage(result.usage)
    result.usage_quality = "provider_reported" if result.usage else "unknown"
    result.attempt_id = execution.attempt.attempt_id
    result.events_path = str(execution.attempt.events)
    result.stdout_path = str(execution.attempt.stdout)
    result.stderr_path = str(execution.attempt.stderr)
    scrub_file(execution.attempt.last_message)
    limited, _ = classify_limit(f"{result.error}\n{result.text}")
    execution.emitter.emit(
        "completed" if result.ok else ("rate_limited" if limited else "failed"),
        "provider result accepted" if result.ok else _truncate(result.error),
        provider_type="agent_result",
        usage=result.usage,
        cost=cost,
        currency=result.cost_currency,
        metadata={
            "exit_code": result.exit_code,
            "duration_s": result.duration_s,
            "session_id": result.session_id,
            "tool_calls": result.tool_calls,
        },
    )
    atomic_json(execution.attempt.result, asdict(result))
    atomic_json(
        execution.attempt.summary,
        {
            "attempt_id": result.attempt_id,
            "agent": execution.emitter.agent,
            "phase": execution.emitter.phase,
            "ok": result.ok,
            "exit_code": result.exit_code,
            "duration_s": result.duration_s,
            "session_id": result.session_id,
            "usage": result.usage,
            "normalized_usage": result.normalized_usage,
            "usage_quality": result.usage_quality,
            "cost_usd": result.cost_usd,
            "reported_cost": cost,
            "cost_currency": result.cost_currency,
            "cost_quality": result.cost_quality,
            "usage_source": result.usage_source,
            "accounting_schema_version": result.accounting_schema_version,
            "tool_calls": result.tool_calls,
            "tool_calls_observed": result.tool_calls_observed,
            "error": result.error,
        },
    )
    return result


def resolve_claude_bin(configured: str | None = None) -> str:
    try:
        return resolve_provider("claude", configured).path
    except ProviderError as exc:
        raise AgentError(str(exc)) from exc


def resolve_codex_bin(configured: str | None = None) -> str:
    try:
        return resolve_provider("codex", configured).path
    except ProviderError as exc:
        raise AgentError(str(exc)) from exc


@dataclass
class ClaudeAgent:
    binary: str
    model: str = ""
    write_allowed_tools: str = "Read,Glob,Grep,Edit,Write"
    extra_args: list[str] = field(default_factory=list)
    name: str = "claude"

    def run(
        self,
        prompt: str,
        *,
        cwd: Path,
        run_dir: Path,
        label: str,
        read_only: bool = True,
        schema: dict | None = None,
        resume: str = "",
        timeout: int = 3600,
        policy: InvocationPolicy | None = None,
        event_handler: Callable[[AgentEvent], None] | None = None,
    ) -> AgentResult:
        attempt = AttemptPaths.create(run_dir, label)
        cmd = _wrap_script(self.binary)
        cmd += [
            "-p",
            "--output-format",
            "stream-json",
            "--verbose",
            "--include-partial-messages",
        ]
        if schema is not None:
            cmd += ["--json-schema", json.dumps(schema)]
        if resume:
            cmd += ["--resume", resume]
        model = policy.model if policy and policy.model else self.model
        if model:
            cmd += ["--model", model]
        cmd += self.extra_args
        capability = policy.capability if policy else (
            "repo_read" if read_only else "workspace_write"
        )
        if read_only or capability != "workspace_write":
            cmd += [
                "--permission-mode", "plan",
                "--tools", "Read,Glob,Grep",
                "--disallowedTools", "Bash,Agent,Task,WebSearch,WebFetch",
            ]
        else:
            cmd += [
                "--permission-mode", "acceptEdits",
                "--tools", self.write_allowed_tools,
                "--allowedTools", self.write_allowed_tools,
                "--disallowedTools", "Agent,Task,WebSearch,WebFetch",
            ]
        if policy is not None:
            cmd += ["--effort", policy.effort]
            if policy.max_budget_usd is not None:
                cmd += ["--max-budget-usd", str(policy.max_budget_usd)]
            cmd += ["--max-turns", str(policy.max_turns)]
            if policy.disable_nested_agents:
                cmd += ["--disallowedTools", "Agent", "Task"]
        try:
            execution = stream_process(
                cmd,
                cwd=cwd,
                stdin_text=prompt,
                timeout=timeout,
                attempt=attempt,
                agent=self.name,
                phase=label,
                decode_stdout=decode_claude_event,
                event_handler=event_handler,
            )
        except ProcessExecutionError as exc:
            raise _translate_process_error(exc) from exc
        result = self._parse(
            execution.returncode,
            execution.stdout,
            execution.stderr,
            execution.duration_s,
            attempt.stdout,
            attempt.stderr,
        )
        return _finalize_attempt(execution, result)

    @staticmethod
    def _parse(rc, out, err, duration, stdout_path, stderr_path) -> AgentResult:
        result = AgentResult(
            ok=False,
            exit_code=rc,
            duration_s=duration,
            stdout_path=str(stdout_path),
            stderr_path=str(stderr_path),
        )
        payload = None
        tool_calls = 0
        for chunk in reversed(out.strip().splitlines() or [""]):
            if not chunk.startswith("{"):
                continue
            try:
                candidate = json.loads(chunk)
            except json.JSONDecodeError:
                continue
            if candidate.get("type") == "result" or "structured_output" in candidate:
                payload = candidate
                break
        for chunk in out.splitlines():
            try:
                candidate = json.loads(chunk)
            except json.JSONDecodeError:
                continue
            if candidate.get("type") != "assistant":
                continue
            content = (candidate.get("message") or {}).get("content") or []
            tool_calls += sum(
                1 for item in content
                if isinstance(item, dict) and item.get("type") == "tool_use"
            )
        if payload is None:
            result.error = (
                f"claude exited {rc}, unparseable output: "
                f"{_truncate(err) or _truncate(out)}"
            )
            return result
        result.session_id = payload.get("session_id", "")
        result.text = payload.get("result", "") or ""
        result.usage = (
            payload.get("usage") if isinstance(payload.get("usage"), dict) else {}
        )
        result.tool_calls = tool_calls
        result.tool_calls_observed = True
        cost = payload.get("total_cost_usd")
        result.cost_usd = float(cost) if isinstance(cost, (int, float)) else None
        if result.cost_usd is not None:
            result.reported_cost = result.cost_usd
            result.cost_currency = "USD"
            result.cost_quality = "provider_reported"
        turns = payload.get("num_turns")
        result.num_turns = int(turns) if isinstance(turns, int) else None
        model_usage = payload.get("modelUsage") or payload.get("model_usage") or {}
        if isinstance(model_usage, dict) and model_usage:
            result.model = ",".join(str(key) for key in model_usage)
        if payload.get("is_error"):
            detail = result.text or payload.get("errors") or payload.get("subtype")
            result.error = f"claude reported error: {_truncate(detail)}"
            return result
        structured = payload.get("structured_output")
        if structured is None and result.text.strip().startswith("{"):
            try:
                structured = json.loads(result.text)
            except json.JSONDecodeError:
                structured = None
        result.structured = structured
        result.ok = rc == 0
        if not result.ok:
            result.error = f"claude exited {rc}: {_truncate(err)}"
        return result


@dataclass
class CodexAgent:
    binary: str
    model: str = ""
    extra_args: list[str] = field(default_factory=list)
    name: str = "codex"

    def run(
        self,
        prompt: str,
        *,
        cwd: Path,
        run_dir: Path,
        label: str,
        read_only: bool = True,
        schema: dict | None = None,
        resume: str = "",
        timeout: int = 3600,
        policy: InvocationPolicy | None = None,
        event_handler: Callable[[AgentEvent], None] | None = None,
    ) -> AgentResult:
        attempt = AttemptPaths.create(run_dir, label)
        cmd = _wrap_script(self.binary)
        cmd += ["exec"]
        capability = policy.capability if policy else (
            "repo_read" if read_only else "workspace_write"
        )
        sandbox = "workspace-write" if capability == "workspace_write" else "read-only"
        if resume:
            cmd += ["resume", resume, "-", "--json", "--skip-git-repo-check"]
            cmd += ["-c", f'sandbox_mode="{sandbox}"']
        else:
            cmd += [
                "-",
                "--json",
                "--color",
                "never",
                "--skip-git-repo-check",
                "-C",
                str(cwd),
                "-s",
                sandbox,
            ]
        cmd += ["-o", str(attempt.last_message)]
        if schema is not None:
            attempt.schema.write_text(json.dumps(schema, indent=2), encoding="utf-8")
            cmd += ["--output-schema", str(attempt.schema)]
        model = policy.model if policy and policy.model else self.model
        if model:
            cmd += ["-m", model]
        cmd += self.extra_args
        # Append safety after user extras so a permissive extra cannot win.
        cmd += ["-c", "sandbox_workspace_write.network_access=false"]
        for feature in ("apps", "browser_use", "browser_use_external", "in_app_browser"):
            cmd += ["--disable", feature]
        if resume:
            cmd += ["-c", f'sandbox_mode="{sandbox}"']
        else:
            cmd += ["-s", sandbox]
        if policy is not None:
            cmd += ["-c", f'model_reasoning_effort="{policy.effort}"']
            if policy.disable_nested_agents:
                cmd += ["--disable", "multi_agent"]
        try:
            execution = stream_process(
                cmd,
                cwd=cwd,
                stdin_text=prompt,
                timeout=timeout,
                attempt=attempt,
                agent=self.name,
                phase=label,
                decode_stdout=decode_codex_event,
                event_handler=event_handler,
            )
        except ProcessExecutionError as exc:
            raise _translate_process_error(exc) from exc
        result = self._parse(
            execution.returncode,
            execution.stdout,
            execution.stderr,
            execution.duration_s,
            attempt.stdout,
            attempt.stderr,
            attempt.last_message,
            schema,
        )
        return _finalize_attempt(execution, result)

    @staticmethod
    def _parse(rc, out, err, duration, stdout_path, stderr_path, last_path, schema) -> AgentResult:
        result = AgentResult(
            ok=False,
            exit_code=rc,
            duration_s=duration,
            stdout_path=str(stdout_path),
            stderr_path=str(stderr_path),
        )
        turn_error = ""
        usage: dict = {}
        tool_calls = 0
        for line in out.splitlines():
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            event_type = event.get("type", "")
            if event_type == "thread.started":
                result.session_id = event.get("thread_id", "")
            elif event_type == "turn.failed":
                turn_error = json.dumps(event.get("error", {}))[:500]
            elif event_type == "turn.completed":
                usage = (
                    event.get("usage")
                    if isinstance(event.get("usage"), dict)
                    else usage
                )
            elif event_type == "item.completed":
                item = event.get("item", {})
                if item.get("type") == "agent_message":
                    result.text = item.get("text", "")
                elif item.get("type") not in ("reasoning", "plan"):
                    tool_calls += 1
        if last_path.exists():
            result.text = last_path.read_text(encoding="utf-8").strip() or result.text
        if turn_error:
            result.error = f"codex turn failed: {turn_error}"
            return result
        if rc != 0:
            result.error = f"codex exited {rc}: {_truncate(err)}"
            return result
        if schema is not None and result.text.strip().startswith("{"):
            try:
                result.structured = json.loads(result.text)
            except json.JSONDecodeError:
                result.structured = None
        result.ok = True
        result.usage = usage
        result.tool_calls = tool_calls
        result.tool_calls_observed = True
        return result
