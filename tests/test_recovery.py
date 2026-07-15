from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from claudex import evidence, planops, prompts, recovery
from claudex.cli import cmd_restart, cmd_resume
from claudex.config import Config
from claudex.lifecycle import LEGAL_TRANSITIONS, Lifecycle
from claudex.state import (
    STATE_SCHEMA_VERSION,
    Phase,
    RunState,
    get_current_run,
    run_dir_for,
)


def run_state(root: Path, **values) -> RunState:
    data = {"run_id": "old", "repo": str(root), "lead": "claude"}
    data.update(values)
    return RunState(**data)


class LifecycleTests(unittest.TestCase):
    def test_transition_table_accepts_only_declared_edges(self) -> None:
        all_states = set(Lifecycle)
        for previous in Lifecycle:
            for target in all_states:
                state = run_state(Path("."), lifecycle=previous.value)
                if target in LEGAL_TRANSITIONS[previous]:
                    state.transition_lifecycle(target, "test", "resume test")
                    self.assertEqual(target.value, state.lifecycle)
                else:
                    with self.assertRaisesRegex(ValueError, "illegal lifecycle"):
                        state.transition_lifecycle(target, "test", "resume test")

    def test_lifecycle_history_has_required_resume_metadata(self) -> None:
        state = run_state(Path("."))
        state.record_lifecycle_start()
        state.gate("Choose A or B", Phase.PLAN_REVISE, kind="decision")

        entry = state.lifecycle_history[-1]
        self.assertEqual(Lifecycle.PAUSED.value, entry["to"])
        self.assertEqual("Choose A or B", entry["reason"])
        self.assertEqual(Phase.INIT.value, entry["phase"])
        self.assertIn("claudex resolve", entry["resume_instruction"])
        self.assertEqual(1, entry["version"])

    def test_retryable_failure_transitions_back_to_exact_phase(self) -> None:
        state = run_state(Path("."), phase=Phase.PLAN_CRITIQUE.value)
        state.fail("provider protocol failure")
        self.assertEqual(Lifecycle.FAILED_RETRYABLE.value, state.lifecycle)

        state.retry()

        self.assertEqual(Phase.PLAN_CRITIQUE.value, state.phase)
        self.assertEqual(Lifecycle.RUNNING.value, state.lifecycle)


class EvidencePacketTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.repo = Path(self.temp.name)
        self.run_dir = self.repo / ".claudex" / "runs" / "run"
        self.run_dir.mkdir(parents=True)
        (self.run_dir / "task.md").write_text("# Goal\n\nChange it.\n", encoding="utf-8")
        (self.repo / "src").mkdir()
        (self.repo / "src" / "app.py").write_text("VALUE = 1\n", encoding="utf-8")
        self.plan = {
            "plan_markdown": "Update the value.",
            "steps": [
                {
                    "title": "Update",
                    "description": "Change the value.",
                    "files": ["src/app.py"],
                    "tests": ["run unit tests"],
                }
            ],
            "risks": [],
            "open_questions": [],
        }
        self.plan_path = self.run_dir / "plan-round-0.json"
        self.plan_path.write_text(json.dumps(self.plan), encoding="utf-8")

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_manifest_is_bounded_hashed_and_contains_selected_repo_evidence(self) -> None:
        cfg = Config(repo=self.repo, max_evidence_bytes=10_000)
        state = run_state(self.repo, run_id="run")

        path = evidence.build_plan_review_packet(
            cfg, state, self.run_dir, self.plan_path
        )
        manifest = json.loads(path.read_text(encoding="utf-8"))

        self.assertLessEqual(manifest["total_bytes"], manifest["max_bytes"])
        purposes = {entry["purpose"] for entry in manifest["entries"]}
        self.assertIn("canonical_plan", purposes)
        self.assertIn("repo:src/app.py", purposes)
        self.assertTrue(all(len(entry["sha256"]) == 64 for entry in manifest["entries"]))
        prompt = prompts.plan_critique(path, 0)
        self.assertIn("Read only the files listed", prompt)
        self.assertNotIn("plan-critique-*", prompt)

    def test_required_evidence_over_cap_fails_closed(self) -> None:
        cfg = Config(
            repo=self.repo,
            max_evidence_bytes=100,
            max_evidence_file_bytes=100,
        )
        state = run_state(self.repo, run_id="run")

        with self.assertRaisesRegex(evidence.EvidenceError, "required evidence"):
            evidence.build_plan_review_packet(cfg, state, self.run_dir, self.plan_path)

    def test_only_existing_repo_relative_evidence_requests_are_accepted(self) -> None:
        cfg = Config(repo=self.repo)

        accepted = evidence.requested_repo_paths(
            cfg, ["src/app.py", "../outside.txt", "missing.py", "src/app.py"]
        )

        self.assertEqual(["src/app.py"], accepted)


class RestartAndMigrationTests(unittest.TestCase):
    def test_restart_command_retires_old_identity_after_verified_copy(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = Config(repo=root)
            old = run_state(root, phase=Phase.PLAN_CRITIQUE.value)
            old_dir = run_dir_for(root, old.run_id)
            old_dir.mkdir(parents=True)
            plan = {
                "plan_markdown": "Canonical",
                "steps": [{"title": "One", "description": "Do it", "files": [], "tests": []}],
                "risks": [],
                "open_questions": [],
            }
            (old_dir / "task.md").write_text("# Goal\n\nDo it", encoding="utf-8")
            (old_dir / "plan-round-0.json").write_text(json.dumps(plan), encoding="utf-8")
            old.canonical_plan_round = 0
            old.canonical_plan_sha256 = planops.plan_digest(plan)
            old.save(old_dir)

            class OldOrchestrator:
                state = old
                run_dir = old_dir

                def retire(self):
                    self.state.advance(Phase.ABORTED)
                    self.state.transition_lifecycle(
                        Lifecycle.CANCELLED, "restart", "inspect artifacts"
                    )
                    self.state.save(self.run_dir)

            created: list = []

            def make_orchestrator(_cfg, state):
                value = SimpleNamespace(state=state, run_until_gate=Mock())
                created.append(value)
                return value

            args = SimpleNamespace(repo=str(root), fresh_plan=False)
            with (
                patch("claudex.cli._cfg", return_value=cfg),
                patch("claudex.cli._active_orchestrator", return_value=OldOrchestrator()),
                patch("claudex.cli.Orchestrator", side_effect=make_orchestrator),
                patch("claudex.cli._locked", side_effect=lambda _c, _o, fn: fn()),
            ):
                result = cmd_restart(args)

            self.assertEqual(0, result)
            new_id = get_current_run(root)
            self.assertIsNotNone(new_id)
            self.assertNotEqual("old", new_id)
            new_dir = run_dir_for(root, new_id)
            replacement = RunState.load(new_dir)
            self.assertEqual("old", replacement.predecessor_run_id)
            self.assertEqual(old.canonical_plan_sha256, replacement.canonical_plan_sha256)
            self.assertTrue((new_dir / "plan-round-0.json").exists())
            self.assertTrue((old_dir / "state.pre-restart.json").exists())
            self.assertEqual(Lifecycle.CANCELLED.value, RunState.load(old_dir).lifecycle)
            created[0].run_until_gate.assert_called_once_with()

    def test_restart_checkpoint_is_hash_equivalent_and_keeps_safe_sessions(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            old_dir = root / "old"
            new_dir = root / "new"
            old_dir.mkdir()
            plan = {
                "plan_markdown": "Canonical",
                "steps": [{"title": "One", "description": "Do it", "files": [], "tests": []}],
                "risks": [],
                "open_questions": [],
            }
            (old_dir / "task.md").write_text("# Goal\n\nDo it", encoding="utf-8")
            (old_dir / "plan-round-1.json").write_text(json.dumps(plan), encoding="utf-8")
            findings = old_dir / "findings.json"
            findings.write_text('{"findings":[]}', encoding="utf-8")
            old = run_state(
                root,
                phase=Phase.FIX.value,
                worktree=str(root / "worktree"),
                base_commit="abc",
                branch="claudex/old",
                canonical_plan_round=1,
                canonical_plan_sha256=planops.plan_digest(plan),
                binding_guidance=["Keep A"],
                implementation_checks=[{"key": "check"}],
                accepted_findings=[{"key": "accepted"}],
                budget_overrides={"invocations": 2},
                provider_invocations=3,
                findings_file=str(findings),
                sessions={
                    "lead_plan": "unsafe-plan-session",
                    "lead_impl": "safe-impl-session",
                    "pair_review": "safe-review-session",
                },
            )
            before = recovery.checkpoint_digest(old, old_dir)

            replacement = recovery.clone_state(old, "new")
            recovery.copy_checkpoint_artifacts(old_dir, new_dir)
            recovery.remap_run_paths(replacement, old_dir, new_dir)
            after = recovery.checkpoint_digest(replacement, new_dir)

            self.assertEqual(before, after)
            self.assertNotIn("lead_plan", replacement.sessions)
            self.assertEqual("safe-impl-session", replacement.sessions["lead_impl"])
            self.assertEqual(str(new_dir / "findings.json"), replacement.findings_file)

    def test_fresh_plan_is_explicit_and_removes_only_planning_checkpoint(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            old_dir = root / "old"
            new_dir = root / "new"
            old_dir.mkdir()
            (old_dir / "task.md").write_text("task", encoding="utf-8")
            (old_dir / "plan-round-0.json").write_text("{}", encoding="utf-8")
            state = run_state(
                root,
                phase=Phase.PLAN_CRITIQUE.value,
                provider_invocations=4,
                binding_guidance=["Keep A"],
            )

            replacement = recovery.clone_state(state, "new", fresh_plan=True)
            recovery.copy_checkpoint_artifacts(old_dir, new_dir)
            recovery.discard_plan_artifacts(new_dir)

            self.assertEqual(Phase.PLAN_DRAFT.value, replacement.phase)
            self.assertEqual(4, replacement.provider_invocations)
            self.assertEqual(["Keep A"], replacement.binding_guidance)
            self.assertTrue((new_dir / "task.md").exists())
            self.assertFalse((new_dir / "plan-round-0.json").exists())

    def test_state_migration_keeps_atomic_original_backup(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            original = {"run_id": "old", "repo": temp, "lead": "claude"}
            encoded = json.dumps(original)
            (run_dir / "state.json").write_text(encoded, encoding="utf-8")

            state = RunState.load(run_dir)

            backup = run_dir / "state.v1.bak.json"
            self.assertEqual(encoded, backup.read_text(encoding="utf-8"))
            persisted = json.loads((run_dir / "state.json").read_text(encoding="utf-8"))
            self.assertEqual(STATE_SCHEMA_VERSION, persisted["state_schema_version"])
            self.assertEqual(STATE_SCHEMA_VERSION, state.state_schema_version)


class ResumeCommandTests(unittest.TestCase):
    def test_resume_continues_same_running_identity_without_override(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = Config(repo=root)
            state = run_state(root, run_id="same-run", phase=Phase.PLAN_DRAFT.value)
            orchestrator = SimpleNamespace(state=state, run_until_gate=Mock())
            args = SimpleNamespace(
                repo=str(root),
                add_invocations=0,
                add_input_tokens=0,
                add_output_tokens=0,
                add_cost_usd=0.0,
                add_tool_calls=0,
                add_wall_seconds=0,
                add_invocation_output_tokens=0,
                add_invocation_tool_calls=0,
                acknowledge_currency=[],
            )

            with (
                patch("claudex.cli._cfg", return_value=cfg),
                patch("claudex.cli._active_orchestrator", return_value=orchestrator),
                patch("claudex.cli._locked", side_effect=lambda _c, _o, fn: fn()),
            ):
                result = cmd_resume(args)

            self.assertEqual(0, result)
            self.assertEqual("same-run", state.run_id)
            orchestrator.run_until_gate.assert_called_once_with()


if __name__ == "__main__":
    unittest.main()
