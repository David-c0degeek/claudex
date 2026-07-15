"""Deadlock-safe streamed subprocess execution and immutable attempt paths."""

from __future__ import annotations

import hashlib
import json
import os
import re
import secrets
import subprocess
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from .events import AgentEvent, EventEmitter, EventJournal


class ProcessExecutionError(RuntimeError):
    pass


@dataclass(frozen=True)
class AttemptPaths:
    attempt_id: str
    root: Path
    command: Path
    stdout: Path
    stderr: Path
    events: Path
    result: Path
    summary: Path
    last_message: Path
    schema: Path

    @classmethod
    def create(cls, run_dir: Path, label: str) -> "AttemptPaths":
        safe = re.sub(r"[^A-Za-z0-9_.-]+", "-", label).strip("-.") or "agent"
        millis = int((time.time() % 1) * 1000)
        attempt_id = f"{time.strftime('%Y%m%d-%H%M%S')}{millis:03d}-{safe}-{secrets.token_hex(2)}"
        root = run_dir / "attempts" / attempt_id
        root.mkdir(parents=True, exist_ok=False)
        return cls(
            attempt_id=attempt_id,
            root=root,
            command=root / "command.json",
            stdout=root / "stdout.jsonl",
            stderr=root / "stderr.log",
            events=root / "events.jsonl",
            result=root / "result.json",
            summary=root / "summary.json",
            last_message=root / "last-message.txt",
            schema=root / "schema.json",
        )


@dataclass
class ExecResult:
    returncode: int
    stdout: str
    stderr: str
    duration_s: float
    attempt: AttemptPaths
    emitter: EventEmitter


LineDecoder = Callable[[str, int, EventEmitter], None]


def atomic_json(path: Path, value: dict) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(value, indent=2, ensure_ascii=False), encoding="utf-8")
    tmp.replace(path)


def _kill_tree(proc: subprocess.Popen) -> None:
    if os.name == "nt":
        subprocess.run(
            ["taskkill", "/T", "/F", "/PID", str(proc.pid)],
            capture_output=True,
        )
    else:
        try:
            os.killpg(proc.pid, 9)
        except (ProcessLookupError, PermissionError):
            proc.kill()


def stream_process(
    cmd: list[str],
    *,
    cwd: Path,
    stdin_text: str,
    timeout: int,
    attempt: AttemptPaths,
    agent: str,
    phase: str,
    decode_stdout: LineDecoder | None = None,
    event_handler: Callable[[AgentEvent], None] | None = None,
) -> ExecResult:
    """Run a command while concurrently draining and journaling both pipes."""
    started = time.monotonic()
    atomic_json(
        attempt.command,
        {
            "attempt_id": attempt.attempt_id,
            "agent": agent,
            "phase": phase,
            "cwd": str(cwd),
            "command": cmd,
            "stdin_bytes": len(stdin_text.encode("utf-8")),
            "stdin_sha256": hashlib.sha256(stdin_text.encode("utf-8")).hexdigest(),
        },
    )
    emitter = EventEmitter(
        journal=EventJournal(attempt.events),
        run_id=attempt.root.parent.parent.name,
        attempt_id=attempt.attempt_id,
        agent=agent,
        phase=phase,
        started_monotonic=started,
        handler=event_handler,
    )
    emitter.emit("started", f"{agent}: {phase}", metadata={"cwd": str(cwd)})
    popen_options: dict = {}
    if os.name == "nt":
        popen_options["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
    else:
        popen_options["start_new_session"] = True
    try:
        proc = subprocess.Popen(
            cmd,
            cwd=str(cwd),
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
            errors="replace",
            bufsize=1,
            **popen_options,
        )
    except OSError as exc:
        emitter.emit("failed", f"process start failed: {exc}", provider_type="process_start")
        atomic_json(
            attempt.summary,
            {
                "attempt_id": attempt.attempt_id,
                "agent": agent,
                "phase": phase,
                "ok": False,
                "error": f"process start failed: {exc}",
            },
        )
        raise ProcessExecutionError(f"{phase}: could not start process: {exc}") from exc

    stdout_parts: list[str] = []
    stderr_parts: list[str] = []
    reader_errors: list[str] = []

    def drain(pipe, path: Path, stream_name: str, target: list[str]) -> None:
        try:
            with path.open("w", encoding="utf-8", newline="\n") as log:
                for line_no, line in enumerate(iter(pipe.readline, ""), start=1):
                    target.append(line)
                    log.write(line)
                    log.flush()
                    raw_ref = f"{path.name}:{line_no}"
                    if stream_name == "stdout" and decode_stdout:
                        try:
                            decode_stdout(line, line_no, emitter)
                        except Exception as exc:
                            emitter.emit(
                                "warning",
                                f"could not decode provider event: {exc}",
                                provider_type="decode_error",
                                raw_ref=raw_ref,
                            )
                    elif stream_name == "stderr" and line.strip():
                        summary = line.replace("\r", "").strip()
                        emitter.emit(
                            "stderr",
                            summary[:1000] + ("…" if len(summary) > 1000 else ""),
                            provider_type="stderr",
                            raw_ref=raw_ref,
                        )
        except Exception as exc:
            reader_errors.append(f"{stream_name}: {exc}")
        finally:
            try:
                pipe.close()
            except Exception:
                pass

    def write_stdin() -> None:
        try:
            if proc.stdin:
                proc.stdin.write(stdin_text)
                proc.stdin.flush()
        except (BrokenPipeError, OSError):
            pass
        finally:
            if proc.stdin:
                try:
                    proc.stdin.close()
                except OSError:
                    pass

    readers = (
        threading.Thread(
            target=drain,
            args=(proc.stdout, attempt.stdout, "stdout", stdout_parts),
            name=f"{attempt.attempt_id}-stdout",
            daemon=True,
        ),
        threading.Thread(
            target=drain,
            args=(proc.stderr, attempt.stderr, "stderr", stderr_parts),
            name=f"{attempt.attempt_id}-stderr",
            daemon=True,
        ),
    )
    writer = threading.Thread(
        target=write_stdin,
        name=f"{attempt.attempt_id}-stdin",
        daemon=True,
    )
    for thread in readers:
        thread.start()
    writer.start()
    timed_out = False
    try:
        proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        timed_out = True
        _kill_tree(proc)
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
    writer.join(timeout=5)
    for thread in readers:
        thread.join(timeout=10)
    duration = time.monotonic() - started
    stdout = "".join(stdout_parts)
    stderr = "".join(stderr_parts)
    if reader_errors:
        emitter.emit("warning", "; ".join(reader_errors), provider_type="pipe_error")
    if timed_out:
        emitter.emit(
            "failed",
            f"timed out after {timeout}s",
            provider_type="timeout",
            metadata={"exit_code": proc.returncode},
        )
        atomic_json(
            attempt.summary,
            {
                "attempt_id": attempt.attempt_id,
                "agent": agent,
                "phase": phase,
                "ok": False,
                "exit_code": proc.returncode,
                "duration_s": duration,
                "error": f"timed out after {timeout}s",
            },
        )
        raise ProcessExecutionError(
            f"{phase}: timed out after {timeout}s (attempt: {attempt.root})"
        )
    emitter.emit(
        "process_exited",
        f"exit {proc.returncode}",
        provider_type="process_exited",
        metadata={"exit_code": proc.returncode},
    )
    return ExecResult(
        returncode=proc.returncode,
        stdout=stdout,
        stderr=stderr,
        duration_s=duration,
        attempt=attempt,
        emitter=emitter,
    )
