"""Agent runners: subprocess wrappers around `claude -p` and `codex exec`.

Design rules enforced here, not by prompt discipline:

* ``read_only=True`` maps to `--permission-mode plan` (Claude) and
  `-s read-only` (Codex, OS-level sandbox). Investigation, disagreement
  analysis, plan review, code review, and verification can never edit.
* ``read_only=False`` is only ever used by the phase driver for the single
  implementation owner, inside its dedicated worktree.
* Structured output goes through `--json-schema` (Claude) and
  `--output-schema` + `-o` (Codex), so the coordinator parses validated JSON,
  never prose.
* Prompts are piped via stdin to dodge Windows command-line length limits.

Output shapes were verified against claude 2.1.207 and codex-cli 0.144:
Claude prints one JSON object with ``structured_output`` / ``result`` /
``session_id`` / ``is_error``; Codex prints JSONL events
(``thread.started`` carries ``thread_id``, ``turn.failed`` carries the error)
and writes the final schema-constrained message to the `-o` file.
"""

from __future__ import annotations

import glob
import json
import os
import shutil
import subprocess
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path


class AgentError(RuntimeError):
    pass


@dataclass
class AgentResult:
    ok: bool
    text: str = ""
    structured: dict | None = None
    session_id: str = ""
    exit_code: int = 0
    duration_s: float = 0.0
    error: str = ""
    stdout_path: str = ""
    stderr_path: str = ""

    def require_structured(self) -> dict:
        if not self.ok:
            raise AgentError(self.error or "agent invocation failed")
        if self.structured is None:
            raise AgentError(
                f"agent returned no structured output (see {self.stdout_path})"
            )
        return self.structured


# --------------------------------------------------------------- exec helper
def _windows() -> bool:
    return os.name == "nt"


def _wrap_script(binary: str) -> list[str]:
    """CreateProcess cannot launch .cmd/.bat/.ps1 directly."""
    low = binary.lower()
    if _windows() and low.endswith((".cmd", ".bat")):
        return ["cmd.exe", "/c", binary]
    if _windows() and low.endswith(".ps1"):
        return ["powershell.exe", "-NoProfile", "-File", binary]
    return [binary]


def _kill_tree(proc: subprocess.Popen) -> None:
    if _windows():
        subprocess.run(
            ["taskkill", "/T", "/F", "/PID", str(proc.pid)],
            capture_output=True,
        )
    else:
        proc.kill()


def _exec(
    cmd: list[str],
    *,
    cwd: Path,
    stdin_text: str,
    timeout: int,
    log_dir: Path,
    label: str,
) -> tuple[int, str, str, float, Path, Path]:
    log_dir.mkdir(parents=True, exist_ok=True)
    stdout_path = log_dir / f"{label}.stdout.log"
    stderr_path = log_dir / f"{label}.stderr.log"
    (log_dir / f"{label}.cmd.log").write_text(
        " ".join(cmd) + "\n\n--- stdin ---\n" + stdin_text, encoding="utf-8"
    )
    started = time.monotonic()
    proc = subprocess.Popen(
        cmd,
        cwd=str(cwd),
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    try:
        out, err = proc.communicate(input=stdin_text, timeout=timeout)
    except subprocess.TimeoutExpired:
        _kill_tree(proc)
        out, err = proc.communicate()
        stdout_path.write_text(out or "", encoding="utf-8")
        stderr_path.write_text(err or "", encoding="utf-8")
        raise AgentError(
            f"{label}: timed out after {timeout}s (logs: {stdout_path})"
        )
    duration = time.monotonic() - started
    stdout_path.write_text(out or "", encoding="utf-8")
    stderr_path.write_text(err or "", encoding="utf-8")
    return proc.returncode, out or "", err or "", duration, stdout_path, stderr_path


# ------------------------------------------------------------- binary lookup
def resolve_claude_bin(configured: str | None = None) -> str:
    for candidate in (os.environ.get("CLAUDEX_CLAUDE_BIN"), configured):
        if candidate:
            return candidate
    found = shutil.which("claude")
    if not found:
        raise AgentError("claude CLI not found on PATH (set CLAUDEX_CLAUDE_BIN)")
    return found


def resolve_codex_bin(configured: str | None = None) -> str:
    for candidate in (os.environ.get("CLAUDEX_CODEX_BIN"), configured):
        if candidate:
            return candidate
    # Prefer the Codex desktop app binary on Windows: npm-distributed builds
    # can lag behind what the account's configured model requires.
    if _windows():
        localappdata = os.environ.get("LOCALAPPDATA", "")
        if localappdata:
            candidates = glob.glob(
                os.path.join(localappdata, "OpenAI", "Codex", "bin", "*", "codex.exe")
            )
            if candidates:
                return max(candidates, key=os.path.getmtime)
    found = shutil.which("codex")
    if not found:
        raise AgentError("codex CLI not found on PATH (set CLAUDEX_CODEX_BIN)")
    return found


# -------------------------------------------------------------------- agents
@dataclass
class ClaudeAgent:
    binary: str
    model: str = ""
    # Write-phase tool policy. acceptEdits auto-approves file edits; Bash is
    # explicitly allowlisted so the owner can run tests and `git commit`
    # unattended. Tighten via config if your project needs a narrower policy.
    write_allowed_tools: str = "Edit,Write,NotebookEdit,TodoWrite,Bash"
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
    ) -> AgentResult:
        cmd = _wrap_script(self.binary)
        cmd += ["-p", "--output-format", "json"]
        if read_only:
            cmd += ["--permission-mode", "plan"]
        else:
            cmd += [
                "--permission-mode",
                "acceptEdits",
                "--allowedTools",
                self.write_allowed_tools,
            ]
        if schema is not None:
            cmd += ["--json-schema", json.dumps(schema)]
        if resume:
            cmd += ["--resume", resume]
        if self.model:
            cmd += ["--model", self.model]
        cmd += self.extra_args

        rc, out, err, duration, so, se = _exec(
            cmd,
            cwd=cwd,
            stdin_text=prompt,
            timeout=timeout,
            log_dir=run_dir / "logs",
            label=label,
        )
        return self._parse(rc, out, err, duration, so, se)

    @staticmethod
    def _parse(rc, out, err, duration, stdout_path, stderr_path) -> AgentResult:
        res = AgentResult(
            ok=False,
            exit_code=rc,
            duration_s=duration,
            stdout_path=str(stdout_path),
            stderr_path=str(stderr_path),
        )
        payload = None
        for chunk in (out.strip(), *reversed(out.strip().splitlines() or [""])):
            if chunk.startswith("{"):
                try:
                    payload = json.loads(chunk)
                    break
                except json.JSONDecodeError:
                    continue
        if payload is None:
            res.error = f"claude exited {rc}, unparseable output: {err.strip()[:500] or out[:500]}"
            return res
        res.session_id = payload.get("session_id", "")
        res.text = payload.get("result", "") or ""
        if payload.get("is_error"):
            res.error = f"claude reported error: {res.text[:500]}"
            return res
        structured = payload.get("structured_output")
        if structured is None and res.text.strip().startswith("{"):
            try:
                structured = json.loads(res.text)
            except json.JSONDecodeError:
                structured = None
        res.structured = structured
        res.ok = rc == 0
        if not res.ok:
            res.error = f"claude exited {rc}: {err.strip()[:500]}"
        return res


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
    ) -> AgentResult:
        cmd = _wrap_script(self.binary)
        cmd += ["exec"]
        sandbox = "read-only" if read_only else "workspace-write"
        if resume:
            # `exec resume` does not accept -s/-C/--color (verified against
            # codex-cli 0.144): the session keeps its original cwd; sandbox
            # is re-asserted through config override instead.
            cmd += ["resume", resume, "-", "--json", "--skip-git-repo-check"]
            cmd += ["-c", f'sandbox_mode="{sandbox}"']
        else:
            cmd += [
                "-",  # read prompt from stdin
                "--json",
                "--color",
                "never",
                "--skip-git-repo-check",
                "-C",
                str(cwd),
                "-s",
                sandbox,
            ]
        last_path = run_dir / "logs" / f"{label}.last.txt"
        last_path.parent.mkdir(parents=True, exist_ok=True)
        cmd += ["-o", str(last_path)]
        if schema is not None:
            schema_path = run_dir / "logs" / f"{label}.schema.json"
            schema_path.write_text(json.dumps(schema, indent=2), encoding="utf-8")
            cmd += ["--output-schema", str(schema_path)]
        if self.model:
            cmd += ["-m", self.model]
        cmd += self.extra_args

        rc, out, err, duration, so, se = _exec(
            cmd,
            cwd=cwd,
            stdin_text=prompt,
            timeout=timeout,
            log_dir=run_dir / "logs",
            label=label,
        )
        return self._parse(rc, out, err, duration, so, se, last_path, schema)

    @staticmethod
    def _parse(rc, out, err, duration, stdout_path, stderr_path, last_path, schema) -> AgentResult:
        res = AgentResult(
            ok=False,
            exit_code=rc,
            duration_s=duration,
            stdout_path=str(stdout_path),
            stderr_path=str(stderr_path),
        )
        turn_error = ""
        for line in out.splitlines():
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            etype = event.get("type", "")
            if etype == "thread.started":
                res.session_id = event.get("thread_id", "")
            elif etype == "turn.failed":
                turn_error = json.dumps(event.get("error", {}))[:500]
            elif etype == "item.completed":
                item = event.get("item", {})
                if item.get("type") == "agent_message":
                    res.text = item.get("text", "")
        if last_path.exists():
            res.text = last_path.read_text(encoding="utf-8").strip() or res.text
        if turn_error:
            res.error = f"codex turn failed: {turn_error}"
            return res
        if rc != 0:
            res.error = f"codex exited {rc}: {err.strip()[:500]}"
            return res
        if schema is not None and res.text.strip().startswith("{"):
            try:
                res.structured = json.loads(res.text)
            except json.JSONDecodeError:
                res.structured = None
        res.ok = True
        return res
