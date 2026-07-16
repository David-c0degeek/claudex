from __future__ import annotations

import io
import json
import tempfile
import threading
import time
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from claudex.cli import (
    _active_orchestrator,
    cmd_status,
    main,
    open_watch_terminals,
)
from claudex.config import Config
from claudex.events import AgentEvent, EventJournal
from claudex import gitops
from claudex.agents import AgentError
from claudex.lifecycle import Lifecycle
from claudex.state import Phase, RunState
from claudex.terminal import next_action, render_event, render_state, watch_run


GOLDENS = Path(__file__).parent / "fixtures" / "goldens"


def _event(sequence: int = 0, *, agent: str = "claude", kind: str = "tool_started") -> AgentEvent:
    return AgentEvent(
        run_id="run",
        attempt_id="attempt",
        agent=agent,
        phase="plan-draft",
        sequence=sequence,
        timestamp="2026-07-15T12:00:00+0200",
        elapsed_s=1.25 + sequence,
        kind=kind,
        summary="reading repository" if sequence == 0 else "finished",
        tool_name="Read" if sequence == 0 else "",
        tool_status="started" if sequence == 0 else "",
    )


class WatchGoldenTests(unittest.TestCase):
    def test_readable_event_matches_color_safe_golden(self) -> None:
        expected = (GOLDENS / "watch-readable.txt").read_text(encoding="utf-8").rstrip("\n")
        self.assertEqual(expected, render_event(_event(), color=False))
        self.assertNotIn("\x1b", render_event(_event(), color=False))

    def test_raw_event_is_stable_redacted_json(self) -> None:
        value = json.loads(render_event(_event(), raw=True))
        self.assertEqual("attempt", value["attempt_id"])
        self.assertEqual("tool_started", value["kind"])

    def test_replay_then_tail_observes_new_event_before_terminal_exit(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            state = RunState("run", ".", "claude")
            state.record_lifecycle_start()
            state.save(run_dir)
            journal = EventJournal(run_dir / "attempts" / "attempt" / "events.jsonl")
            journal.append(_event())
            lines: list[str] = []
            first_seen = threading.Event()

            def output(value: str) -> None:
                lines.append(value)
                first_seen.set()

            result: list[int] = []
            thread = threading.Thread(
                target=lambda: result.append(
                    watch_run(
                        run_dir, follow=True, color="never",
                        poll_interval=0.02, output=output,
                    )
                )
            )
            thread.start()
            self.assertTrue(first_seen.wait(2), "watcher did not replay the first event")
            journal.append(_event(1, kind="completed"))
            state.phase = Phase.DONE.value
            state.transition_lifecycle(Lifecycle.COMPLETED, "done", "git merge claudex/run")
            state.save(run_dir)
            thread.join(timeout=3)
            self.assertFalse(thread.is_alive())
            self.assertEqual([0], result)
            self.assertEqual(3, len(lines))
            self.assertIn("[CLAUDEX][STATE] lifecycle=completed", lines[-1])

    def test_agent_filter_and_cancelled_exit_are_scriptable(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            state = RunState(
                "run", ".", "claude", phase=Phase.ABORTED.value,
                lifecycle=Lifecycle.CANCELLED.value,
            )
            state.save(run_dir)
            first = run_dir / "attempts" / "a" / "events.jsonl"
            second = run_dir / "attempts" / "b" / "events.jsonl"
            EventJournal(first).append(_event(agent="claude"))
            EventJournal(second).append(_event(agent="codex"))
            lines: list[str] = []
            code = watch_run(
                run_dir, agent="codex", follow=False, color="never", output=lines.append
            )
            self.assertEqual(130, code)
            self.assertEqual(2, len(lines))
            self.assertIn("[CODEX]", lines[0])
            self.assertIn("lifecycle=cancelled", lines[-1])

    def test_rate_limited_exit_and_raw_state_record_are_stable(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            state = RunState(
                "run", ".", "claude", lifecycle=Lifecycle.RATE_LIMITED.value
            )
            state.save(run_dir)
            lines: list[str] = []
            code = watch_run(run_dir, follow=False, raw=True, output=lines.append)
            self.assertEqual(75, code)
            self.assertEqual("run_state", json.loads(lines[-1])["record_type"])

    def test_read_only_watch_does_not_persist_a_legacy_state_migration(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            legacy = '{"run_id":"run","repo":".","lead":"claude"}'
            (run_dir / "state.json").write_text(legacy, encoding="utf-8")
            watch_run(run_dir, follow=False, color="never", output=lambda _: None)
            self.assertEqual(legacy, (run_dir / "state.json").read_text(encoding="utf-8"))
            self.assertEqual([], list(run_dir.glob("state.v*.bak.json")))


class StatusGoldenTests(unittest.TestCase):
    def test_every_stop_state_has_an_exact_next_action(self) -> None:
        expected = json.loads(
            (GOLDENS / "status-next-actions.json").read_text(encoding="utf-8")
        )
        cases = {}
        for lifecycle in (
            Lifecycle.RUNNING,
            Lifecycle.COMPLETED,
            Lifecycle.CANCELLED,
            Lifecycle.FAILED_RETRYABLE,
            Lifecycle.FAILED_TERMINAL,
            Lifecycle.RATE_LIMITED,
            Lifecycle.PAUSED_BUDGET,
        ):
            state = RunState("run", ".", "claude", lifecycle=lifecycle.value)
            state.branch = "claudex/run"
            cases[lifecycle.value] = next_action(state)
        decision = RunState("run", ".", "claude", lifecycle=Lifecycle.PAUSED.value)
        decision.gate_kind = "decision"
        cases["paused_decision"] = next_action(decision)
        self.assertEqual(expected, cases)

    def test_status_command_renders_lifecycle_reason_and_golden_action(self) -> None:
        expected = json.loads(
            (GOLDENS / "status-next-actions.json").read_text(encoding="utf-8")
        )
        cases = (
            (Lifecycle.RUNNING, Phase.INIT, "running"),
            (Lifecycle.COMPLETED, Phase.DONE, "completed"),
            (Lifecycle.CANCELLED, Phase.ABORTED, "cancelled"),
            (Lifecycle.FAILED_RETRYABLE, Phase.FAILED, "failed_retryable"),
            (Lifecycle.FAILED_TERMINAL, Phase.FAILED, "failed_terminal"),
            (Lifecycle.RATE_LIMITED, Phase.PLAN_DRAFT, "rate_limited"),
            (Lifecycle.PAUSED_BUDGET, Phase.PAUSED_BUDGET, "paused_budget"),
            (Lifecycle.PAUSED, Phase.AWAIT_GUIDANCE, "paused_decision"),
        )
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            for lifecycle, phase, golden_key in cases:
                run_id = lifecycle.value
                state = RunState(
                    run_id, str(repo), "claude",
                    phase=phase.value, lifecycle=lifecycle.value,
                    branch="claudex/run",
                )
                if lifecycle is Lifecycle.PAUSED:
                    state.gate_kind = "decision"
                state.lifecycle_history = [
                    {
                        "reason": f"golden {lifecycle.value}",
                        "resume_instruction": "golden action",
                    }
                ]
                if lifecycle in (
                    Lifecycle.FAILED_RETRYABLE,
                    Lifecycle.FAILED_TERMINAL,
                ):
                    state.error = f"golden {lifecycle.value}"
                run_dir = repo / ".claudex" / "runs" / run_id
                state.save(run_dir)
                output = io.StringIO()
                with redirect_stdout(output):
                    code = cmd_status(SimpleNamespace(repo=str(repo), run_id=run_id))
                self.assertEqual(0, code)
                rendered = output.getvalue()
                self.assertIn(f"lifecycle:{lifecycle.value}", rendered)
                self.assertIn(f"golden {lifecycle.value}", rendered)
                self.assertIn(f"next:     {expected[golden_key]}", rendered)

    def test_state_rendering_redacts_reason(self) -> None:
        state = RunState("run", ".", "claude", lifecycle=Lifecycle.CANCELLED.value)
        state.error = "token=super-secret-value"
        rendered = render_state(state, raw=True)
        self.assertNotIn("super-secret-value", rendered)

    def test_status_is_read_only_but_control_load_retains_migration_backup(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            run_id = "legacy"
            run_dir = repo / ".claudex" / "runs" / run_id
            run_dir.mkdir(parents=True)
            legacy = json.dumps(
                {"run_id": run_id, "repo": str(repo), "lead": "claude"}
            )
            state_path = run_dir / "state.json"
            state_path.write_text(legacy, encoding="utf-8")
            output = io.StringIO()
            with redirect_stdout(output):
                self.assertEqual(
                    0, cmd_status(SimpleNamespace(repo=str(repo), run_id=run_id))
                )
            self.assertEqual(legacy, state_path.read_text(encoding="utf-8"))
            current = repo / ".claudex" / "current"
            current.write_text(run_id, encoding="utf-8")
            with patch("claudex.cli.Orchestrator") as orchestrator:
                _active_orchestrator(Config(repo=repo))
            orchestrator.assert_called_once()
            self.assertTrue((run_dir / "state.v1.bak.json").exists())


class TerminalLauncherTests(unittest.TestCase):
    def test_launcher_opens_two_view_only_commands_without_state_mutation(self) -> None:
        popen = Mock()
        repo = Path(r"D:\repo with spaces")
        launched = open_watch_terminals(
            repo,
            "run-id",
            platform_name="nt",
            which=lambda _name: r"C:\Windows\wt.exe",
            popen=popen,
        )
        self.assertTrue(launched)
        self.assertEqual(2, popen.call_count)
        commands = [call.args[0] for call in popen.call_args_list]
        self.assertTrue(all("watch" in command for command in commands))
        self.assertIn("claude", commands[0])
        self.assertIn("codex", commands[1])
        self.assertFalse(any("cancel" in command for command in commands))

    def test_headless_fallback_prints_equivalent_commands(self) -> None:
        output = io.StringIO()
        with redirect_stdout(output):
            opened = open_watch_terminals(
                Path("/repo"), "run-id", platform_name="posix", which=lambda _: None
            )
        self.assertFalse(opened)
        rendered = output.getvalue()
        self.assertIn("--agent claude", rendered)
        self.assertIn("--agent codex", rendered)

    def test_failed_preflight_does_not_create_run_or_open_terminals(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            gitops.git(repo, "init")
            gitops.git(repo, "config", "user.email", "tests@example.invalid")
            gitops.git(repo, "config", "user.name", "Claudex Tests")
            (repo / ".gitignore").write_text(".claudex/\n", encoding="utf-8")
            gitops.git(repo, "add", ".gitignore")
            gitops.git(repo, "commit", "-m", "base")
            stdout = io.StringIO()
            stderr = io.StringIO()
            with (
                patch("claudex.cli.open_watch_terminals") as launch,
                redirect_stdout(stdout),
                redirect_stderr(stderr),
            ):
                code = main(
                    ["run", "--open-terminals", "--repo", str(repo)]
                )
            self.assertEqual(1, code)
            self.assertIn("task contract missing", stderr.getvalue())
            launch.assert_not_called()
            self.assertFalse((repo / ".claudex" / "current").exists())
            self.assertFalse((repo / ".claudex" / "runs").exists())

    def test_failed_provider_probe_does_not_create_run_or_open_terminals(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            gitops.git(repo, "init")
            gitops.git(repo, "config", "user.email", "tests@example.invalid")
            gitops.git(repo, "config", "user.name", "Claudex Tests")
            (repo / ".gitignore").write_text(".claudex/\n", encoding="utf-8")
            task = repo / ".claudex" / "task.md"
            task.parent.mkdir()
            task.write_text(
                "# Goal\n\n[CHANGE] Exercise provider startup preflight.\n",
                encoding="utf-8",
            )
            gitops.git(repo, "add", ".gitignore")
            gitops.git(repo, "commit", "-m", "base")
            stderr = io.StringIO()
            with (
                patch(
                    "claudex.cli.build_agents",
                    side_effect=AgentError("provider capability probe failed"),
                ),
                patch("claudex.cli.open_watch_terminals") as launch,
                redirect_stderr(stderr),
            ):
                code = main(
                    ["run", "--open-terminals", "--repo", str(repo)]
                )
            self.assertEqual(1, code)
            self.assertIn("provider capability probe failed", stderr.getvalue())
            launch.assert_not_called()
            self.assertFalse((repo / ".claudex" / "current").exists())
            self.assertFalse((repo / ".claudex" / "runs").exists())


if __name__ == "__main__":
    unittest.main()
