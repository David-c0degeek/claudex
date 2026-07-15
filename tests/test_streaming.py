from __future__ import annotations

import json
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest.mock import patch

from claudex.agents import (
    AgentResult,
    AttemptPaths,
    ClaudeAgent,
    CodexAgent,
    ExecResult,
    _finalize_attempt,
    decode_claude_event,
    decode_codex_event,
)
from claudex.events import AgentEvent, EventEmitter, EventJournal, project_attempts
from claudex.processes import stream_process


def emitter_for(root: Path, *, agent: str = "codex") -> EventEmitter:
    import time

    return EventEmitter(
        journal=EventJournal(root / "events.jsonl"),
        run_id="run",
        attempt_id="attempt",
        agent=agent,
        phase="test",
        started_monotonic=time.monotonic(),
    )


class EventContractTests(unittest.TestCase):
    def test_event_round_trip_ignores_future_fields(self) -> None:
        event = AgentEvent(
            run_id="run",
            attempt_id="attempt",
            agent="codex",
            phase="plan",
            sequence=3,
            timestamp="2026-07-15T21:00:00+0200",
            elapsed_s=0.25,
            kind="usage",
            summary="usage update",
            usage={"input_tokens": 10},
        )
        payload = event.to_dict()
        payload["future_provider_field"] = {"safe": True}

        loaded = AgentEvent.from_dict(payload)

        self.assertEqual(event, loaded)

    def test_journal_replays_complete_lines_and_ignores_partial_tail(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            journal = EventJournal(Path(temp) / "events.jsonl")
            emitter = emitter_for(Path(temp))
            emitter.emit("started", "ready")
            with journal.path.open("a", encoding="utf-8") as stream:
                stream.write('{"sequence": 99')

            events = journal.read()

            self.assertEqual(["started"], [event.kind for event in events])
            self.assertEqual([], journal.read(after_sequence=0))

    def test_codex_adapter_normalizes_tools_message_and_usage(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            emitter = emitter_for(root)
            lines = [
                {"type": "thread.started", "thread_id": "thread-1"},
                {
                    "type": "item.started",
                    "item": {
                        "type": "command_execution",
                        "command": "python -m unittest",
                        "status": "in_progress",
                    },
                },
                {
                    "type": "item.completed",
                    "item": {"type": "agent_message", "text": "done"},
                },
                {
                    "type": "turn.completed",
                    "usage": {
                        "input_tokens": 20,
                        "cached_input_tokens": 10,
                        "output_tokens": 5,
                    },
                },
            ]
            for number, payload in enumerate(lines, start=1):
                decode_codex_event(json.dumps(payload), number, emitter)

            events = EventJournal(root / "events.jsonl").read()

            self.assertEqual(
                ["provider_started", "tool_started", "message", "usage"],
                [event.kind for event in events],
            )
            self.assertEqual("command_execution", events[1].tool_name)
            self.assertEqual("done", events[2].summary)
            self.assertEqual(20, events[3].usage["input_tokens"])

    def test_claude_adapter_normalizes_partial_tool_retry_and_result(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            emitter = emitter_for(root, agent="claude")
            lines = [
                {
                    "type": "system",
                    "subtype": "init",
                    "session_id": "session-1",
                    "model": "claude-test",
                    "capabilities": ["interrupt_receipt_v1"],
                },
                {
                    "type": "stream_event",
                    "event": {
                        "type": "content_block_delta",
                        "delta": {"type": "text_delta", "text": "hello"},
                    },
                },
                {
                    "type": "assistant",
                    "message": {
                        "usage": {"input_tokens": 3},
                        "content": [{"type": "tool_use", "name": "Read"}],
                    },
                },
                {
                    "type": "system",
                    "subtype": "api_retry",
                    "attempt": 1,
                    "max_retries": 3,
                    "retry_delay_ms": 250,
                    "error": "rate_limit",
                },
                {
                    "type": "result",
                    "subtype": "success",
                    "session_id": "session-1",
                    "result": "hello",
                    "usage": {"output_tokens": 2},
                    "total_cost_usd": 0.01,
                },
            ]
            for number, payload in enumerate(lines, start=1):
                decode_claude_event(json.dumps(payload), number, emitter)

            events = EventJournal(root / "events.jsonl").read()
            kinds = [event.kind for event in events]

            self.assertEqual(
                [
                    "provider_started",
                    "text_delta",
                    "usage",
                    "tool_started",
                    "rate_limited",
                    "provider_completed",
                ],
                kinds,
            )
            self.assertEqual("hello", events[1].summary)
            self.assertEqual("Read", events[3].tool_name)
            self.assertEqual(0.01, events[-1].cost)

    def test_status_projection_is_derived_from_journal_events(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            emitter = emitter_for(root)
            emitter.emit("started", "start")
            emitter.emit("tool_started", "command", tool_name="command")
            emitter.emit("usage", "usage", usage={"input_tokens": 4})
            emitter.emit("completed", "done", cost=0.1, currency="USD")

            projected = project_attempts(EventJournal(root / "events.jsonl").read())

            self.assertEqual(1, len(projected))
            self.assertEqual("completed", projected[0]["status"])
            self.assertEqual(1, projected[0]["tool_calls"])
            self.assertEqual({"input_tokens": 4}, projected[0]["usage"])
            self.assertEqual(0.1, projected[0]["cost"])


class StreamingProcessTests(unittest.TestCase):
    def test_event_is_visible_before_slow_process_completes(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            run_dir = root / "run"
            release = root / "release"
            attempt = AttemptPaths.create(run_dir, "slow-codex")
            seen = threading.Event()
            holder: dict = {}
            script = (
                "import json, os, sys, time\n"
                "print(json.dumps({'type':'thread.started','thread_id':'t'}), flush=True)\n"
                f"release = {str(release)!r}\n"
                "while not os.path.exists(release): time.sleep(0.01)\n"
                "print(json.dumps({'type':'turn.completed','usage':{'input_tokens':1}}), flush=True)\n"
            )

            def on_event(event: AgentEvent) -> None:
                if event.kind == "provider_started":
                    seen.set()

            def run() -> None:
                holder["execution"] = stream_process(
                    [sys.executable, "-u", "-c", script],
                    cwd=root,
                    stdin_text="",
                    timeout=10,
                    attempt=attempt,
                    agent="codex",
                    phase="slow-test",
                    decode_stdout=decode_codex_event,
                    event_handler=on_event,
                )

            worker = threading.Thread(target=run)
            worker.start()
            self.assertTrue(seen.wait(5), "stream event was not delivered promptly")
            self.assertTrue(worker.is_alive(), "process completed before event was observed")
            release.write_text("go", encoding="utf-8")
            worker.join(10)
            self.assertFalse(worker.is_alive())

            execution = holder["execution"]
            result = _finalize_attempt(
                execution,
                AgentResult(ok=True, exit_code=0, duration_s=execution.duration_s),
            )
            self.assertTrue(attempt.stdout.exists())
            self.assertTrue(attempt.stderr.exists())
            self.assertTrue(attempt.command.exists())
            self.assertTrue(attempt.events.exists())
            self.assertTrue(attempt.result.exists())
            self.assertTrue(attempt.summary.exists())
            self.assertEqual(attempt.attempt_id, result.attempt_id)

    def test_concurrent_readers_drain_large_stdout_and_stderr(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            attempt = AttemptPaths.create(root / "run", "noisy")
            script = (
                "import sys\n"
                "chunk='x'*8192+'\\n'\n"
                "for _ in range(200):\n"
                " sys.stdout.write(chunk); sys.stdout.flush()\n"
                " sys.stderr.write(chunk); sys.stderr.flush()\n"
            )

            execution = stream_process(
                [sys.executable, "-u", "-c", script],
                cwd=root,
                stdin_text="",
                timeout=20,
                attempt=attempt,
                agent="fake",
                phase="noisy-test",
            )

            self.assertEqual(0, execution.returncode)
            self.assertGreater(len(execution.stdout), 1_000_000)
            self.assertGreater(len(execution.stderr), 1_000_000)
            self.assertIn("process_exited", [event.kind for event in EventJournal(attempt.events).read()])

    def test_attempt_paths_are_unique_for_repeated_phase_labels(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp) / "run"
            first = AttemptPaths.create(run_dir, "plan-critique-0")
            second = AttemptPaths.create(run_dir, "plan-critique-0")

            self.assertNotEqual(first.attempt_id, second.attempt_id)
            self.assertNotEqual(first.root, second.root)


class StreamResultParserTests(unittest.TestCase):
    def test_claude_runner_requests_streamed_partial_json(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            captured: dict = {}

            def fake_exec(cmd, **kwargs):
                captured["cmd"] = cmd
                attempt = kwargs["attempt"]
                payload = {
                    "type": "result",
                    "subtype": "success",
                    "session_id": "session",
                    "result": "{}",
                    "structured_output": {},
                }
                return ExecResult(
                    returncode=0,
                    stdout=json.dumps(payload) + "\n",
                    stderr="",
                    duration_s=0.1,
                    attempt=attempt,
                    emitter=emitter_for(attempt.root, agent="claude"),
                )

            with patch("claudex.agents.stream_process", side_effect=fake_exec):
                result = ClaudeAgent(binary="claude").run(
                    "prompt",
                    cwd=root,
                    run_dir=root / "run",
                    label="plan",
                    schema={"type": "object"},
                )

            self.assertTrue(result.ok)
            self.assertIn("stream-json", captured["cmd"])
            self.assertIn("--include-partial-messages", captured["cmd"])
            self.assertIn("--verbose", captured["cmd"])

    def test_codex_runner_requests_jsonl_and_final_schema_file(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            captured: dict = {}

            def fake_exec(cmd, **kwargs):
                captured["cmd"] = cmd
                attempt = kwargs["attempt"]
                attempt.last_message.write_text("{}", encoding="utf-8")
                output = "\n".join(
                    (
                        json.dumps({"type": "thread.started", "thread_id": "thread"}),
                        json.dumps({"type": "turn.completed", "usage": {}}),
                    )
                )
                return ExecResult(
                    returncode=0,
                    stdout=output,
                    stderr="",
                    duration_s=0.1,
                    attempt=attempt,
                    emitter=emitter_for(attempt.root),
                )

            with patch("claudex.agents.stream_process", side_effect=fake_exec):
                result = CodexAgent(binary="codex").run(
                    "prompt",
                    cwd=root,
                    run_dir=root / "run",
                    label="review",
                    schema={"type": "object"},
                )

            self.assertTrue(result.ok)
            self.assertIn("--json", captured["cmd"])
            self.assertIn("--output-schema", captured["cmd"])
            self.assertIn("-o", captured["cmd"])

    def test_claude_parser_uses_terminal_stream_result(self) -> None:
        terminal = {
            "type": "result",
            "subtype": "success",
            "session_id": "s",
            "result": '{"ok":true}',
            "structured_output": {"ok": True},
            "usage": {"input_tokens": 4},
            "total_cost_usd": 0.02,
            "num_turns": 2,
        }
        result = ClaudeAgent._parse(
            0,
            json.dumps({"type": "system", "subtype": "init"})
            + "\n"
            + json.dumps(terminal)
            + "\n",
            "",
            1.0,
            Path("stdout"),
            Path("stderr"),
        )

        self.assertTrue(result.ok)
        self.assertEqual({"ok": True}, result.structured)
        self.assertEqual({"input_tokens": 4}, result.usage)
        self.assertEqual(0.02, result.cost_usd)
        self.assertEqual(2, result.num_turns)

    def test_codex_parser_captures_terminal_usage_and_tool_count(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            last = Path(temp) / "last.txt"
            last.write_text('{"ok":true}', encoding="utf-8")
            output = "\n".join(
                json.dumps(value)
                for value in (
                    {"type": "thread.started", "thread_id": "thread"},
                    {
                        "type": "item.completed",
                        "item": {"type": "command_execution", "status": "completed"},
                    },
                    {
                        "type": "turn.completed",
                        "usage": {"input_tokens": 9, "output_tokens": 2},
                    },
                )
            )
            result = CodexAgent._parse(
                0,
                output,
                "",
                1.0,
                Path("stdout"),
                Path("stderr"),
                last,
                {"type": "object"},
            )

            self.assertTrue(result.ok)
            self.assertEqual("thread", result.session_id)
            self.assertEqual({"input_tokens": 9, "output_tokens": 2}, result.usage)
            self.assertEqual(1, result.tool_calls)
            self.assertEqual({"ok": True}, result.structured)


if __name__ == "__main__":
    unittest.main()
