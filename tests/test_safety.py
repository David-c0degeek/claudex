from __future__ import annotations

import io
import json
import os
import shlex
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch

from claudex import budgets, gitops
from claudex.agents import ClaudeAgent, CodexAgent
from claudex.cli import _wait_until_rate_reset, build_parser, cmd_cancel
from claudex.config import Config
from claudex.events import EventJournal
from claudex.lifecycle import Lifecycle
from claudex.phases import Orchestrator
from claudex.processes import (
    AttemptPaths,
    ExecResult,
    ProcessCancelledError,
    ProcessTimeoutError,
    atomic_json,
    stream_process,
)
from claudex.providers import ProviderError, resolve_provider
from claudex.security import prune_raw_attempts, redact_text
from claudex.state import Phase, RunState, run_dir_for, set_current_run


FIXTURES = Path(__file__).parent / "fixtures"


def _pid_alive(pid: int) -> bool:
    if os.name == "nt":
        proc = subprocess.run(
            ["tasklist", "/FI", f"PID eq {pid}", "/NH", "/FO", "CSV"],
            capture_output=True,
            text=True,
        )
        return str(pid) in proc.stdout
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


def _wait_for(path: Path, timeout: float = 5.0) -> None:
    deadline = time.monotonic() + timeout
    while not path.exists() and time.monotonic() < deadline:
        time.sleep(0.02)
    if not path.exists():
        raise AssertionError(f"timed out waiting for {path}")


def _init_repo(root: Path) -> None:
    gitops.git(root, "init")
    gitops.git(root, "config", "user.email", "tests@example.invalid")
    gitops.git(root, "config", "user.name", "Claudex Tests")
    (root / ".gitignore").write_text(".claudex/\n", encoding="utf-8")
    (root / "tracked.txt").write_text("base\n", encoding="utf-8")
    gitops.git(root, "add", ".gitignore", "tracked.txt")
    gitops.git(root, "commit", "-m", "base")


def _fixture_command(name: str) -> str:
    values = [sys.executable, str(FIXTURES / name)]
    return subprocess.list2cmdline(values) if os.name == "nt" else shlex.join(values)


class ProcessSafetyTests(unittest.TestCase):
    def _run_tree(self, root: Path, timeout: int, cancel: bool):
        attempt = AttemptPaths.create(root / "run", "tree")
        ready = root / "ready.pid"
        observed: dict[str, BaseException | None] = {"error": None}

        def target() -> None:
            try:
                stream_process(
                    [sys.executable, str(FIXTURES / "process_tree.py"), str(ready)],
                    cwd=root,
                    stdin_text="",
                    timeout=timeout,
                    attempt=attempt,
                    agent="fake",
                    phase="tree",
                )
            except BaseException as exc:  # captured for the test thread
                observed["error"] = exc

        thread = threading.Thread(target=target)
        thread.start()
        _wait_for(ready)
        child_pid = int(ready.read_text(encoding="utf-8"))
        if cancel:
            (attempt.root / "cancel.requested").touch()
        thread.join(timeout=15)
        self.assertFalse(thread.is_alive())
        deadline = time.monotonic() + 5
        while _pid_alive(child_pid) and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertFalse(_pid_alive(child_pid), f"descendant {child_pid} survived")
        return attempt, observed["error"]

    def test_cancel_terminates_descendant_and_retains_partial_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            attempt, error = self._run_tree(Path(temp), timeout=30, cancel=True)
            self.assertIsInstance(error, ProcessCancelledError)
            summary = json.loads(attempt.summary.read_text(encoding="utf-8"))
            self.assertEqual("cancelled", summary["terminal_reason"])
            kinds = [event.kind for event in EventJournal(attempt.events).read()]
            self.assertIn("started", kinds)
            self.assertIn("cancelled", kinds)

    def test_timeout_terminates_descendant_and_retains_partial_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            attempt, error = self._run_tree(Path(temp), timeout=1, cancel=False)
            self.assertIsInstance(error, ProcessTimeoutError)
            summary = json.loads(attempt.summary.read_text(encoding="utf-8"))
            self.assertEqual("timeout", summary["terminal_reason"])
            self.assertTrue(attempt.stdout.exists())


class GitIntegrityTests(unittest.TestCase):
    def test_untracked_content_is_dirty_and_changes_exact_identity(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            _init_repo(root)
            clean = gitops.worktree_identity(root)
            (root / "new.txt").write_text("payload\n", encoding="utf-8")
            dirty = gitops.worktree_identity(root)
            self.assertTrue(gitops.has_uncommitted_changes(root))
            self.assertNotEqual(clean["content_sha256"], dirty["content_sha256"])
            self.assertEqual("new.txt", dirty["untracked"][0]["path"])

    def test_committed_tree_and_diff_include_exact_content(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            _init_repo(root)
            base = gitops.head_commit(root)
            (root / "new.txt").write_text("payload\n", encoding="utf-8")
            gitops.git(root, "add", "new.txt")
            gitops.git(root, "commit", "-m", "add payload")
            identity = gitops.worktree_identity(root)
            self.assertFalse(identity["status"])
            self.assertEqual(gitops.tree_id(root), identity["head_tree"])
            self.assertIn("payload", gitops.diff_text(root, base))


class MechanicalGateTests(unittest.TestCase):
    def _orchestrator(self, root: Path, command: str, timeout: int = 10) -> Orchestrator:
        cfg = Config(repo=root, test_command=command, agent_timeout=timeout)
        state = RunState(
            "run", str(root), "claude", phase=Phase.TESTS.value,
            worktree=str(root),
        )
        state.record_lifecycle_start()
        with patch("claudex.phases.build_agents", return_value={}):
            orch = Orchestrator(cfg, state)
        orch.run_dir.mkdir(parents=True, exist_ok=True)
        return orch

    def test_noisy_mechanical_gate_streams_and_retains_full_raw_logs(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            _init_repo(root)
            orch = self._orchestrator(root, _fixture_command("noisy_command.py"))
            with redirect_stdout(io.StringIO()):
                self.assertTrue(orch.run_test_gate())
            attempts = list((orch.run_dir / "attempts").iterdir())
            self.assertEqual(1, len(attempts))
            self.assertGreater((attempts[0] / "stdout.jsonl").stat().st_size, 1_000_000)
            self.assertGreater((attempts[0] / "stderr.log").stat().st_size, 1_000_000)
            self.assertEqual(Phase.VERIFY.value, orch.state.phase)

    def test_hung_mechanical_gate_times_out_through_shared_lifecycle(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            _init_repo(root)
            orch = self._orchestrator(
                root, _fixture_command("hung_command.py"), timeout=1
            )
            with redirect_stdout(io.StringIO()):
                self.assertFalse(orch.run_test_gate())
            attempt = next((orch.run_dir / "attempts").iterdir())
            summary = json.loads((attempt / "summary.json").read_text(encoding="utf-8"))
            self.assertEqual("timeout", summary["terminal_reason"])
            self.assertEqual(Phase.FIX.value, orch.state.phase)


class ProviderResolutionTests(unittest.TestCase):
    CLAUDE_HELP = "--output-format stream-json --json-schema --max-budget-usd --permission-mode --resume --disallowedTools"
    CODEX_HELP = "--json --output-schema --sandbox resume --disable"

    @staticmethod
    def runner(versions: dict[str, str], helps: dict[str, str]):
        def run(cmd, **_kw):
            binary = str(cmd[0])
            output = versions[binary] if "--version" in cmd else helps[binary]
            return subprocess.CompletedProcess(cmd, 0, stdout=output, stderr="")
        return run

    def test_semantic_version_beats_candidate_order_and_mtime(self) -> None:
        runner = self.runner(
            {"older": "codex-cli 0.99.9", "newer": "codex-cli 1.2.0"},
            {"older": self.CODEX_HELP, "newer": self.CODEX_HELP},
        )
        info = resolve_provider(
            "codex",
            runner=runner,
            candidates=[("older", "desktop"), ("newer", "PATH")],
        )
        self.assertEqual("newer", info.path)

    def test_explicit_incompatible_provider_fails_closed(self) -> None:
        runner = self.runner({"broken": "2.1.0"}, {"broken": "--resume"})
        with patch.dict(os.environ, {"CLAUDEX_CLAUDE_BIN": "broken"}):
            with self.assertRaisesRegex(ProviderError, "missing capabilities"):
                resolve_provider("claude", runner=runner)


class CapabilityPolicyTests(unittest.TestCase):
    def setUp(self) -> None:
        self.policy = budgets.InvocationPolicy(
            profile="verification",
            model="",
            requested_effort="high",
            effort="high",
            max_budget_usd=1.0,
            max_turns=3,
            timeout_seconds=15,
            disable_nested_agents=True,
            capability="evidence_read",
        )

    @staticmethod
    def _execution(captured: dict, agent: str):
        def fake(cmd, **kw):
            captured["cmd"] = cmd
            attempt = kw["attempt"]
            attempt.stdout.write_text("", encoding="utf-8")
            attempt.stderr.write_text("", encoding="utf-8")
            if agent == "claude":
                output = json.dumps({"type": "result", "result": "ok"})
            else:
                attempt.last_message.write_text("ok", encoding="utf-8")
                output = json.dumps({"type": "turn.completed", "usage": {}})
            from claudex.events import EventEmitter, EventJournal
            emitter = EventEmitter(
                journal=EventJournal(attempt.events),
                run_id="run", attempt_id=attempt.attempt_id, agent=agent,
                phase="test", started_monotonic=time.monotonic(),
            )
            return ExecResult(0, output, "", 0.1, attempt, emitter)
        return fake

    def test_evidence_phase_denies_shell_network_and_nested_claude_tools(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            captured: dict = {}
            root = Path(temp)
            with patch("claudex.agents.stream_process", side_effect=self._execution(captured, "claude")):
                ClaudeAgent("claude").run(
                    "x", cwd=root, run_dir=root / "run", label="verify",
                    policy=self.policy,
                )
            cmd = captured["cmd"]
            self.assertEqual("Read,Glob,Grep", cmd[cmd.index("--tools") + 1])
            denied = " ".join(cmd)
            for tool in ("Bash", "Agent", "WebSearch"):
                self.assertIn(tool, denied)

    def test_codex_safety_is_appended_after_permissive_extra_args(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            captured: dict = {}
            root = Path(temp)
            with patch("claudex.agents.stream_process", side_effect=self._execution(captured, "codex")):
                CodexAgent("codex", extra_args=["-s", "danger-full-access"]).run(
                    "x", cwd=root, run_dir=root / "run", label="verify",
                    policy=self.policy,
                )
            cmd = captured["cmd"]
            self.assertGreater(len(cmd) - 1 - cmd[::-1].index("read-only"), cmd.index("danger-full-access"))
            self.assertIn("sandbox_workspace_write.network_access=false", cmd)
            self.assertIn("multi_agent", cmd)

    def test_config_rejects_broad_bash_permission(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            cfg = Config(Path(temp), claude_write_allowed_tools="Read,Edit,Bash")
            with self.assertRaisesRegex(ValueError, "broad"):
                cfg.validate()


class RateAndCancelTests(unittest.TestCase):
    def test_rate_limit_state_is_durable_and_resume_clears_metadata(self) -> None:
        state = RunState("run", ".", "claude")
        state.record_lifecycle_start()
        state.rate_limit("limited", agent="claude", label="plan", delay_s=20)
        self.assertEqual(Lifecycle.RATE_LIMITED.value, state.lifecycle)
        self.assertGreater(state.rate_limit_reset_at, time.time())
        state.resume_rate_limit("operator")
        self.assertEqual(Lifecycle.RUNNING.value, state.lifecycle)
        self.assertEqual(0, state.rate_limit_reset_at)

    def test_autonomous_wait_is_injectable_and_cancellable_without_sleeping(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            now = [0.0]
            sleeps: list[float] = []
            def sleeper(delay: float) -> None:
                sleeps.append(delay)
                now[0] += delay
            self.assertTrue(
                _wait_until_rate_reset(
                    run_dir, 2.0,
                    clock=lambda: now[0],
                    sleeper=sleeper,
                )
            )
            self.assertTrue(sleeps)
            (run_dir / "cancel.requested").touch()
            self.assertFalse(
                _wait_until_rate_reset(run_dir, 20.0, clock=lambda: 0.0, sleeper=sleeper)
            )

    def test_cancel_command_is_idempotent_for_idle_run(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp)
            run_id = "run"
            state = RunState(run_id, str(repo), "claude")
            state.record_lifecycle_start()
            state.save(run_dir_for(repo, run_id))
            set_current_run(repo, run_id)
            args = build_parser().parse_args(["cancel", "--repo", str(repo)])
            self.assertEqual(0, cmd_cancel(args))
            self.assertEqual(0, cmd_cancel(args))
            loaded = RunState.load(run_dir_for(repo, run_id))
            self.assertEqual(Lifecycle.CANCELLED.value, loaded.lifecycle)


class RedactionRetentionTests(unittest.TestCase):
    def test_redaction_covers_display_and_persisted_attempt_artifacts(self) -> None:
        secret = "sk-ant-abcdefghijklmnopqrstuvwxyz"
        self.assertNotIn(secret, redact_text(f"token={secret}"))
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            attempt = AttemptPaths.create(root / "runs" / "run", "secret")
            command = [
                sys.executable,
                "-c",
                f"import sys; print('token={secret}'); print('Authorization: Bearer {secret}', file=sys.stderr)",
            ]
            result = stream_process(
                command, cwd=root, stdin_text="", timeout=10, attempt=attempt,
                agent="fake", phase="secret",
            )
            self.assertEqual(0, result.returncode)
            for path in (attempt.stdout, attempt.stderr, attempt.events, attempt.command):
                self.assertNotIn(secret, path.read_text(encoding="utf-8"))
            atomic_json(
                attempt.result,
                {"token": secret, "error": f"Authorization: Bearer {secret}"},
            )
            self.assertNotIn(secret, attempt.result.read_text(encoding="utf-8"))

    def test_retention_prunes_only_raw_and_survives_malformed_files(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / ".claudex"
            attempt = AttemptPaths.create(root / "runs" / "run", "old")
            for path in (attempt.stdout, attempt.stderr, attempt.last_message):
                path.write_bytes(b"not utf8 \xff" * 100)
            for path in (attempt.events, attempt.result, attempt.summary):
                path.write_text("{}\n", encoding="utf-8")
            report = prune_raw_attempts(root, max_age_days=14, max_bytes=0)
            self.assertEqual(3, len(report["removed"]))
            for path in (attempt.events, attempt.result, attempt.summary):
                self.assertTrue(path.exists())

    def test_event_journal_write_failure_does_not_deadlock_or_lose_result(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            attempt = AttemptPaths.create(root / "runs" / "run", "disk-fault")
            with patch("claudex.events.EventJournal.append", side_effect=OSError("full")):
                result = stream_process(
                    [sys.executable, "-c", "print('complete')"],
                    cwd=root, stdin_text="", timeout=10, attempt=attempt,
                    agent="fake", phase="disk-fault",
                )
            self.assertEqual(0, result.returncode)
            self.assertTrue(result.emitter.write_errors)
            atomic_json(attempt.summary, {"ok": True})
            self.assertTrue(attempt.summary.exists())

    def test_retention_unlink_failure_is_reported_and_nonfatal(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / ".claudex"
            attempt = AttemptPaths.create(root / "runs" / "run", "fault")
            attempt.stdout.write_text("raw", encoding="utf-8")
            original = Path.unlink
            def fail_raw(path: Path, *args, **kwargs):
                if path == attempt.stdout:
                    raise OSError("read-only storage")
                return original(path, *args, **kwargs)
            with patch("pathlib.Path.unlink", new=fail_raw):
                report = prune_raw_attempts(root, max_age_days=14, max_bytes=0)
            self.assertTrue(report["errors"])
            self.assertTrue(attempt.stdout.exists())


if __name__ == "__main__":
    unittest.main()
