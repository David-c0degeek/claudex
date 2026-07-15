"""Scenario-driven subprocess fixture for both provider CLI protocols."""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys
import time
from pathlib import Path


CLAUDE_HELP = """
--output-format stream-json --include-partial-messages --json-schema
--max-budget-usd --permission-mode --resume --disallowedTools --tools
"""
CODEX_HELP = """
--json --output-schema --sandbox -s resume --disable
"""


def _provider() -> str:
    name = Path(sys.argv[0]).stem.lower()
    return "claude" if "claude" in name else "codex"


def _scenario() -> dict:
    path = os.environ.get("CLAUDEX_FAKE_SCENARIO", "")
    if not path:
        return {"schema_version": 1, "calls": []}
    value = json.loads(Path(path).read_text(encoding="utf-8"))
    if value.get("schema_version") != 1:
        raise SystemExit("unsupported fake scenario schema")
    return value


def _append_call(value: dict) -> int:
    path = Path(os.environ["CLAUDEX_FAKE_CALL_LOG"])
    path.parent.mkdir(parents=True, exist_ok=True)
    existing = path.read_text(encoding="utf-8").splitlines() if path.exists() else []
    with path.open("a", encoding="utf-8", newline="\n") as stream:
        stream.write(json.dumps(value, separators=(",", ":")) + "\n")
    return len(existing)


def _write_commit(action: dict) -> None:
    path = Path(action["path"])
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(action.get("content", ""), encoding="utf-8")
    subprocess.run(["git", "add", "--", str(path)], check=True)
    subprocess.run(
        ["git", "commit", "-m", action.get("message", "fake implementation")],
        check=True,
        capture_output=True,
    )


def _synchronize(call: dict) -> None:
    if call.get("ready_file"):
        Path(call["ready_file"]).write_text("ready", encoding="utf-8")
    release = call.get("release_file")
    if release:
        while not Path(release).exists():
            time.sleep(0.02)
    if call.get("hang"):
        time.sleep(120)


def _last_message_path(args: list[str]) -> Path | None:
    for flag in ("-o", "--output-last-message"):
        if flag in args:
            return Path(args[args.index(flag) + 1])
    return None


def _emit(provider: str, call: dict) -> int:
    for value in call.get("stderr", []):
        print(value, file=sys.stderr, flush=True)
    for value in call.get("raw_events", []):
        if isinstance(value, str):
            print(value, flush=True)
        else:
            print(json.dumps(value), flush=True)
    response = call.get("response")
    error = call.get("error", "")
    usage = call.get("usage", {"input_tokens": 11, "output_tokens": 7})
    session = call.get("session", f"fake-{provider}-session")
    if provider == "claude":
        payload = {
            "type": "result",
            "session_id": session,
            "result": json.dumps(response) if response is not None else error,
            "structured_output": response,
            "is_error": bool(error),
            "usage": usage,
            "total_cost_usd": call.get("cost_usd", 0.001),
            "num_turns": 1,
        }
        print(json.dumps(payload), flush=True)
    else:
        print(json.dumps({"type": "thread.started", "thread_id": session}), flush=True)
        if error:
            print(json.dumps({"type": "turn.failed", "error": {"message": error}}), flush=True)
        else:
            text = json.dumps(response) if response is not None else call.get("text", "ok")
            print(
                json.dumps(
                    {"type": "item.completed", "item": {"type": "agent_message", "text": text}}
                ),
                flush=True,
            )
            output = _last_message_path(sys.argv[1:])
            if output:
                output.parent.mkdir(parents=True, exist_ok=True)
                output.write_text(text, encoding="utf-8")
            print(json.dumps({"type": "turn.completed", "usage": usage}), flush=True)
    return int(call.get("exit_code", 0 if not error else 1))


def _emit_raw(values: list) -> None:
    for value in values:
        print(value if isinstance(value, str) else json.dumps(value), flush=True)


def main() -> int:
    provider = _provider()
    args = sys.argv[1:]
    scenario = _scenario()
    if "--version" in args or "-V" in args:
        versions = scenario.get("versions", {})
        print(versions.get(provider, "2.1.210" if provider == "claude" else "codex-cli 0.144.4"))
        return 0
    if "--help" in args or "-h" in args:
        help_overrides = scenario.get("help", {})
        print(help_overrides.get(provider, CLAUDE_HELP if provider == "claude" else CODEX_HELP))
        return 0

    prompt = sys.stdin.read()
    index = _append_call(
        {
            "provider": provider,
            "cwd": str(Path.cwd()),
            "prompt_sha256": hashlib.sha256(prompt.encode("utf-8")).hexdigest(),
            "prompt": prompt,
            "argv": args,
        }
    )
    calls = scenario.get("calls", [])
    if index >= len(calls):
        print(f"unexpected fake provider call {index}", file=sys.stderr)
        return 97
    call = calls[index]
    if call.get("provider") and call["provider"] != provider:
        print(
            f"call {index}: expected {call['provider']}, got {provider}",
            file=sys.stderr,
        )
        return 98
    if call.get("prompt_contains") and call["prompt_contains"] not in prompt:
        print(f"call {index}: prompt mismatch", file=sys.stderr)
        return 99
    for action in call.get("write_commits", []):
        _write_commit(action)
    child = None
    if call.get("spawn_descendant"):
        child = subprocess.Popen(
            [sys.executable, "-c", "import time; time.sleep(120)"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        if call.get("child_pid_file"):
            Path(call["child_pid_file"]).write_text(str(child.pid), encoding="utf-8")
    _emit_raw(call.get("pre_events", []))
    _synchronize(call)
    return _emit(provider, call)


if __name__ == "__main__":
    raise SystemExit(main())
