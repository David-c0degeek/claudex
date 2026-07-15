from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from claudex import budgets, gitops, planops
from claudex.agents import ClaudeAgent, CodexAgent
from claudex.config import Config
from claudex.lifecycle import Lifecycle
from claudex.events import EventJournal
from claudex.state import RunState, get_current_run, run_dir_for


PROJECT_ROOT = Path(__file__).resolve().parents[1]
FIXTURES = PROJECT_ROOT / "tests" / "fixtures"


def _git_repo(root: Path) -> None:
    gitops.git(root, "init")
    gitops.git(root, "config", "user.email", "tests@example.invalid")
    gitops.git(root, "config", "user.name", "Claudex Integration")
    (root / ".gitignore").write_text(".claudex/\n", encoding="utf-8")
    (root / "verify_fixture.py").write_text(
        "from pathlib import Path\nassert Path('feature.txt').read_text() == 'done\\n'\n",
        encoding="utf-8",
    )
    gitops.git(root, "add", ".gitignore", "verify_fixture.py")
    gitops.git(root, "commit", "-m", "base fixture")


def _plan() -> dict:
    return {
        "plan_markdown": "Implement one bounded fixture file.",
        "steps": [
            {
                "title": "Implement fixture",
                "description": "Create feature.txt and validate it.",
                "files": ["feature.txt"],
                "tests": ["python verify_fixture.py"],
            }
        ],
        "risks": [],
        "open_questions": [],
    }


def _finding() -> dict:
    return {
        "key": "pin-test-command",
        "kind": "decision",
        "category": "decision",
        "severity": "major",
        "file": None,
        "line": None,
        "problem": "The test command must be explicit.",
        "evidence": "The base repository contains verify_fixture.py.",
        "suggested_fix": "Name python verify_fixture.py.",
    }


def _critique(*, gate: bool, agree: bool = False) -> dict:
    return {
        "verdict": "AGREE" if agree else "REVISE",
        "findings": [] if agree else [_finding()],
        "implementation_checks": [],
        "requires_human_decision": gate,
        "decision_question": "Approve verify_fixture.py?" if gate else None,
        "missing_evidence": [],
        "simpler_alternative": None,
        "notes": "",
    }


def _scenario(*, explicit_gate: bool = False) -> dict:
    plan = _plan()
    revision = {
        "base_plan_sha256": planops.plan_digest(plan),
        "plan_markdown": None,
        "steps": None,
        "risks": None,
        "open_questions": None,
        "responses": [
            {
                "finding_key": "pin-test-command",
                "finding": "The test command must be explicit.",
                "action": "accepted",
                "rationale": "The plan already names the committed fixture.",
            }
        ],
    }
    return {
        "schema_version": 1,
        "calls": [
            {"provider": "claude", "response": plan},
            {"provider": "codex", "response": _critique(gate=explicit_gate)},
            {"provider": "claude", "response": revision},
            {"provider": "codex", "response": _critique(gate=False, agree=True)},
            {
                "provider": "claude",
                "write_commits": [
                    {
                        "path": "feature.txt",
                        "content": "done\n",
                        "message": "Implement fixture",
                    }
                ],
                "response": {
                    "commits": [],
                    "files_changed": ["feature.txt"],
                    "tests_command": "python verify_fixture.py",
                    "tests_passed": True,
                    "test_output_summary": "fixture passed",
                    "deviations_from_plan": [],
                    "notes": "",
                },
            },
            {
                "provider": "codex",
                "response": {
                    "verdict": "AGREE",
                    "findings": [],
                    "tests_adequate": True,
                    "tests_critique": "The coordinator gate is exact.",
                },
            },
            {
                "provider": "codex",
                "response": {
                    "criteria": [
                        {
                            "criterion": "feature.txt contains done",
                            "met": True,
                            "evidence": "verify_fixture.py exited zero on the recorded tree",
                        }
                    ],
                    "scope_expansion": [],
                    "tests_meaningful": True,
                    "unsupported_claims": [],
                    "verdict": "pass",
                    "notes": "",
                },
            },
        ],
    }


class BlackBoxCoordinatorTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.repo = Path(self.temp.name)
        _git_repo(self.repo)
        task = self.repo / ".claudex" / "task.md"
        task.parent.mkdir(parents=True, exist_ok=True)
        task.write_text(
            "# Goal\n\n[CHANGE] Complete the deterministic fake-provider fixture.\n\n"
            "# Acceptance criteria\n\n- feature.txt contains done.\n",
            encoding="utf-8",
        )
        self.call_log = self.repo / ".claudex" / "fake-calls.jsonl"
        self.scenario_path = self.repo / ".claudex" / "scenario.json"
        cfg = Config(
            repo=self.repo,
            claude_bin=str(FIXTURES / "fake_claude.py"),
            codex_bin=str(FIXTURES / "fake_codex.py"),
            planning_effort="medium",
            implementation_effort="medium",
            verification_effort="medium",
            max_run_invocations=10,
            test_command=f'"{sys.executable}" verify_fixture.py',
            agent_timeout=20,
        )
        cfg.save()
        self.env = os.environ.copy()
        self.env.update(
            {
                "CLAUDEX_FAKE_SCENARIO": str(self.scenario_path),
                "CLAUDEX_FAKE_CALL_LOG": str(self.call_log),
                "PYTHONPATH": str(PROJECT_ROOT),
                "PYTHONUTF8": "1",
            }
        )

    def tearDown(self) -> None:
        run_id = get_current_run(self.repo)
        if run_id:
            try:
                state = RunState.load(run_dir_for(self.repo, run_id))
                if state.worktree and Path(state.worktree).exists():
                    gitops.remove_worktree(self.repo, Path(state.worktree), force=True)
            except Exception:
                pass
        self.temp.cleanup()

    def _cli(self, *args: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, "-m", "claudex", *args, "--repo", str(self.repo)],
            cwd=PROJECT_ROOT,
            env=self.env,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=60,
        )

    def _calls(self) -> list[dict]:
        return [
            json.loads(line)
            for line in self.call_log.read_text(encoding="utf-8").splitlines()
        ]

    def test_false_decision_label_revises_then_completes_full_protocol(self) -> None:
        self.scenario_path.write_text(json.dumps(_scenario()), encoding="utf-8")
        result = self._cli("run")
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertNotIn("concrete human decision", result.stdout)
        run_id = get_current_run(self.repo)
        state = RunState.load(run_dir_for(self.repo, run_id))
        self.assertEqual(Lifecycle.COMPLETED.value, state.lifecycle)
        calls = self._calls()
        self.assertEqual(7, len(calls))
        self.assertEqual(
            ["claude", "codex", "claude", "codex", "claude", "codex", "codex"],
            [call["provider"] for call in calls],
        )
        identity = json.loads(
            (run_dir_for(self.repo, run_id) / "diff-final-identity.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertEqual(identity["verified_commit"], identity["worktree"]["head"])
        manifests = list(run_dir_for(self.repo, run_id).glob("evidence-plan-review-*.json"))
        self.assertEqual(2, len(manifests))
        for manifest_path in manifests:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            self.assertLessEqual(manifest["total_bytes"], manifest["max_bytes"])
            self.assertTrue(all(entry["sha256"] for entry in manifest["entries"]))
        status = self._cli("status")
        self.assertIn("lifecycle:completed", status.stdout)
        self.assertIn("provider: calls 7/10", status.stdout)

    def test_explicit_human_gate_accepts_guidance_and_resumes_same_run(self) -> None:
        self.scenario_path.write_text(
            json.dumps(_scenario(explicit_gate=True)), encoding="utf-8"
        )
        first = self._cli("run")
        self.assertEqual(0, first.returncode, first.stdout + first.stderr)
        self.assertIn("concrete human decision is required", first.stdout)
        run_id = get_current_run(self.repo)
        paused = RunState.load(run_dir_for(self.repo, run_id))
        self.assertEqual(Lifecycle.PAUSED.value, paused.lifecycle)
        resumed = self._cli("resolve", "--notes", "Approved for this fixture.")
        self.assertEqual(0, resumed.returncode, resumed.stdout + resumed.stderr)
        completed = RunState.load(run_dir_for(self.repo, run_id))
        self.assertEqual(Lifecycle.COMPLETED.value, completed.lifecycle)
        self.assertEqual(["Approved for this fixture."], completed.binding_guidance)
        self.assertEqual(7, len(self._calls()))
        self.assertTrue(
            any("Approved for this fixture." in call["prompt"] for call in self._calls()[2:])
        )

    def test_rate_limit_returns_75_then_resume_uses_a_new_attempt(self) -> None:
        scenario = _scenario()
        scenario["calls"].insert(
            0,
            {
                "provider": "claude",
                "error": "usage limit reached; retry after 1 second",
                "exit_code": 1,
            },
        )
        self.scenario_path.write_text(json.dumps(scenario), encoding="utf-8")
        first = self._cli("run")
        self.assertEqual(75, first.returncode, first.stdout + first.stderr)
        run_id = get_current_run(self.repo)
        limited = RunState.load(run_dir_for(self.repo, run_id))
        self.assertEqual(Lifecycle.RATE_LIMITED.value, limited.lifecycle)
        self.assertEqual("claude", limited.rate_limit_agent)
        resumed = self._cli("resume")
        self.assertEqual(0, resumed.returncode, resumed.stdout + resumed.stderr)
        completed = RunState.load(run_dir_for(self.repo, run_id))
        self.assertEqual(Lifecycle.COMPLETED.value, completed.lifecycle)
        attempts = list((run_dir_for(self.repo, run_id) / "attempts").iterdir())
        provider_attempts = [
            path
            for path in attempts
            if json.loads((path / "command.json").read_text(encoding="utf-8"))["agent"]
            != "coordinator-test"
        ]
        self.assertEqual(8, len(provider_attempts))
        self.assertEqual(8, len({path.name for path in provider_attempts}))

    def test_budget_admission_pauses_without_launching_an_extra_provider(self) -> None:
        cfg = Config.load(self.repo)
        cfg.max_run_invocations = 2
        cfg.save()
        self.scenario_path.write_text(json.dumps(_scenario()), encoding="utf-8")
        result = self._cli("run")
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        run_id = get_current_run(self.repo)
        state = RunState.load(run_dir_for(self.repo, run_id))
        self.assertEqual(Lifecycle.PAUSED_BUDGET.value, state.lifecycle)
        self.assertEqual(2, len(self._calls()))

    def test_restart_fresh_plan_creates_new_identity_and_completes(self) -> None:
        first_protocol = _scenario(explicit_gate=True)
        restarted_protocol = _scenario()
        scenario = {
            "schema_version": 1,
            "calls": first_protocol["calls"][:2] + restarted_protocol["calls"],
        }
        self.scenario_path.write_text(json.dumps(scenario), encoding="utf-8")
        first = self._cli("run")
        self.assertEqual(0, first.returncode, first.stdout + first.stderr)
        old_id = get_current_run(self.repo)
        restarted = self._cli("restart", "--fresh-plan")
        self.assertEqual(0, restarted.returncode, restarted.stdout + restarted.stderr)
        new_id = get_current_run(self.repo)
        self.assertNotEqual(old_id, new_id)
        old = RunState.load(run_dir_for(self.repo, old_id))
        new = RunState.load(run_dir_for(self.repo, new_id))
        self.assertEqual(Lifecycle.CANCELLED.value, old.lifecycle)
        self.assertEqual(Lifecycle.COMPLETED.value, new.lifecycle)
        self.assertEqual(old_id, new.predecessor_run_id)
        self.assertEqual(9, len(self._calls()))

    def test_dirty_starting_repository_fails_before_any_provider_call(self) -> None:
        (self.repo / "untracked.txt").write_text("not in base\n", encoding="utf-8")
        self.scenario_path.write_text(
            json.dumps({"schema_version": 1, "calls": []}), encoding="utf-8"
        )
        result = self._cli("run")
        self.assertEqual(1, result.returncode)
        self.assertIn("tracked or untracked changes before run start", result.stdout)
        self.assertFalse(self.call_log.exists())


class RealGitWorktreeTests(unittest.TestCase):
    def test_tracked_untracked_and_deleted_files_all_change_identity(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            _git_repo(root)
            clean = gitops.worktree_identity(root)
            (root / "verify_fixture.py").write_text("changed\n", encoding="utf-8")
            tracked = gitops.worktree_identity(root)
            self.assertNotEqual(clean["content_sha256"], tracked["content_sha256"])
            gitops.git(root, "restore", "verify_fixture.py")
            (root / "new.txt").write_text("new\n", encoding="utf-8")
            untracked = gitops.worktree_identity(root)
            self.assertTrue(untracked["untracked"])
            (root / "new.txt").unlink()
            (root / "verify_fixture.py").unlink()
            deleted = gitops.worktree_identity(root)
            self.assertIn("verify_fixture.py", deleted["status"])

    def test_tested_worktree_tree_is_the_fast_forward_integration_tree(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "repo"
            root.mkdir()
            _git_repo(root)
            base = gitops.head_commit(root)
            worktree = root.parent / "feature-worktree"
            gitops.add_worktree(root, "feature", worktree, base)
            try:
                (worktree / "feature.txt").write_text("done\n", encoding="utf-8")
                gitops.git(worktree, "add", "feature.txt")
                gitops.git(worktree, "commit", "-m", "feature")
                tested = gitops.worktree_identity(worktree)
                self.assertFalse(tested["status"])
                self.assertEqual(gitops.tree_id(worktree), tested["head_tree"])
                self.assertEqual(
                    0,
                    subprocess.run(
                        ["git", "-C", str(root), "merge-base", "--is-ancestor", "HEAD", "feature"]
                    ).returncode,
                )
                self.assertEqual(gitops.tree_id(root, "feature"), tested["head_tree"])
            finally:
                gitops.remove_worktree(root, worktree, force=True)

    def test_conflicting_integration_is_detected_without_mutating_main(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "repo"
            root.mkdir()
            _git_repo(root)
            (root / "shared.txt").write_text("base\n", encoding="utf-8")
            gitops.git(root, "add", "shared.txt")
            gitops.git(root, "commit", "-m", "shared base")
            base = gitops.head_commit(root)
            worktree = root.parent / "conflict-worktree"
            gitops.add_worktree(root, "conflict-feature", worktree, base)
            try:
                (worktree / "shared.txt").write_text("feature\n", encoding="utf-8")
                gitops.git(worktree, "add", "shared.txt")
                gitops.git(worktree, "commit", "-m", "feature side")
                (root / "shared.txt").write_text("main\n", encoding="utf-8")
                gitops.git(root, "add", "shared.txt")
                gitops.git(root, "commit", "-m", "main side")
                before = gitops.head_commit(root)
                probe = subprocess.run(
                    [
                        "git", "-C", str(root), "merge-tree", "--write-tree",
                        "HEAD", "conflict-feature",
                    ],
                    capture_output=True,
                    text=True,
                )
                self.assertNotEqual(0, probe.returncode)
                self.assertIn("CONFLICT", probe.stdout + probe.stderr)
                self.assertEqual(before, gitops.head_commit(root))
            finally:
                gitops.remove_worktree(root, worktree, force=True)


class PlatformCommandTests(unittest.TestCase):
    @unittest.skipUnless(os.name == "nt", "Windows quoting contract")
    def test_windows_configured_executable_and_test_path_with_spaces(self) -> None:
        with tempfile.TemporaryDirectory(prefix="claudex quoted ") as temp:
            path = Path(temp) / "script with spaces.py"
            path.write_text("print('quoted-ok')\n", encoding="utf-8")
            from claudex.processes import shell_argv
            proc = subprocess.run(
                shell_argv(f'"{sys.executable}" "{path}"'),
                capture_output=True,
                text=True,
            )
            self.assertEqual(0, proc.returncode, proc.stderr)
            self.assertIn("quoted-ok", proc.stdout)


class FakeProviderScenarioTests(unittest.TestCase):
    def test_versioned_matrix_drives_real_adapters_and_malformed_events(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            call_log = root / "calls.jsonl"
            env = {
                "CLAUDEX_FAKE_SCENARIO": str(
                    FIXTURES / "scenarios" / "event-matrix.json"
                ),
                "CLAUDEX_FAKE_CALL_LOG": str(call_log),
            }
            policy = budgets.InvocationPolicy(
                profile="planning",
                model="",
                requested_effort="low",
                effort="low",
                max_budget_usd=0.1,
                max_turns=1,
                timeout_seconds=10,
                disable_nested_agents=True,
            )
            with patch.dict(os.environ, env):
                claude = ClaudeAgent(str(FIXTURES / "fake_claude.py")).run(
                    "matrix call one",
                    cwd=root,
                    run_dir=root / "run",
                    label="matrix-claude",
                    schema={"type": "object"},
                    policy=policy,
                )
                codex = CodexAgent(str(FIXTURES / "fake_codex.py")).run(
                    "matrix call two",
                    cwd=root,
                    run_dir=root / "run",
                    label="matrix-codex",
                    schema={"type": "object"},
                    policy=policy,
                )
            self.assertTrue(claude.ok)
            self.assertEqual({"ok": True}, claude.structured)
            self.assertFalse(codex.ok)
            self.assertIn("usage limit", codex.error)
            claude_events = EventJournal(Path(claude.events_path)).read()
            self.assertTrue(any(event.kind == "text_delta" for event in claude_events))
            self.assertTrue(any(event.provider_type == "malformed" for event in claude_events))
            calls = call_log.read_text(encoding="utf-8").splitlines()
            self.assertEqual(2, len(calls))


if __name__ == "__main__":
    unittest.main()
