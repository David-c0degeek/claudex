from __future__ import annotations

import io
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from claudex import gitops, planops, prompts, schemas
from claudex.cli import _read_guidance_notes, _replacement_state, build_parser
from claudex.config import Config
from claudex.phases import (
    Orchestrator,
    OrchestratorError,
    validate_plan_shape,
    validate_revision_responses,
    verification_findings,
)
from claudex.state import Phase, RunState, run_dir_for


def plan() -> dict:
    return {
        "plan_markdown": "A complete plan.",
        "steps": [
            {
                "title": "Implement",
                "description": "Make the change.",
                "files": ["example.txt"],
                "tests": ["exercise the behavior"],
            }
        ],
        "risks": [],
        "open_questions": [],
    }


def revision() -> dict:
    value = plan()
    value["base_plan_sha256"] = planops.plan_digest(plan())
    value["responses"] = [
        {
            "finding_key": "missing-test",
            "finding": "A test is missing.",
            "action": "accepted",
            "rationale": "The test is now in the plan.",
        }
    ]
    return value


def critique(*, decision: bool = False) -> dict:
    return {
        "verdict": "REVISE",
        "findings": [
            {
                "key": "choose-contract" if decision else "missing-test",
                "kind": "decision" if decision else "new",
                "category": "decision" if decision else "validation",
                "severity": "major",
                "file": "plan.json",
                "line": 1,
                "problem": "A human choice is required." if decision else "A test is missing.",
                "evidence": "The contract leaves two incompatible choices open.",
                "suggested_fix": "Ask the human." if decision else "Add the test.",
            }
        ],
        "implementation_checks": [],
        "requires_human_decision": decision,
        "decision_question": "Choose A or B." if decision else None,
        "missing_evidence": [],
        "simpler_alternative": None,
        "notes": "",
    }


def result(payload: dict, session_id: str = "session") -> SimpleNamespace:
    return SimpleNamespace(
        require_structured=lambda: payload,
        session_id=session_id,
    )


class OrchestrationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.repo = Path(self.temp.name)
        gitops.git(self.repo, "init")
        gitops.git(self.repo, "config", "user.email", "tests@example.invalid")
        gitops.git(self.repo, "config", "user.name", "Claudex Tests")
        (self.repo / ".gitignore").write_text(".claudex/\n", encoding="utf-8")
        gitops.git(self.repo, "add", ".gitignore")
        gitops.git(self.repo, "commit", "-m", "fixture")
        self.cfg = Config(repo=self.repo, max_plan_rounds=1)
        self.state = RunState(
            run_id="run",
            repo=str(self.repo),
            lead="claude",
            phase=Phase.PLAN_CRITIQUE.value,
        )
        with patch("claudex.phases.build_agents", return_value={}):
            self.orch = Orchestrator(self.cfg, self.state)
        self.orch.run_dir.mkdir(parents=True)
        self.orch.task_snapshot.write_text("# Acceptance criteria\n\n- Works\n", encoding="utf-8")
        self.orch._plan_path(0).write_text(json.dumps(plan()), encoding="utf-8")

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_cap_allows_final_revision_then_fresh_audit(self) -> None:
        self.orch._run_agent = Mock(return_value=result(critique()))

        self.orch.pair_plan_turn()

        self.assertEqual(Phase.PLAN_REVISE.value, self.state.phase)
        self.assertEqual("", self.state.gate_kind)
        self.assertEqual(1, self.state.plan_round)

        self.orch._run_agent = Mock(return_value=result(revision(), "lead-session"))
        self.orch.phase_plan_revise()
        self.assertEqual(1, self.state.plan_revisions)
        self.assertEqual(Phase.PLAN_CRITIQUE.value, self.state.phase)

        captured: dict = {}

        def final_agent(_name, prompt, **kwargs):
            captured["prompt"] = prompt
            captured["resume"] = kwargs["resume"]
            return result(critique(), "fresh-audit-session")

        self.orch._run_agent = final_agent
        self.orch.pair_plan_turn()

        self.assertEqual(Phase.AWAIT_GUIDANCE.value, self.state.phase)
        self.assertEqual("budget", self.state.gate_kind)
        self.assertEqual("", captured["resume"])
        self.assertIn("FINAL PLAN AUDIT", captured["prompt"])

    def test_default_planning_protocol_is_bounded_to_four_fresh_calls(self) -> None:
        self.state.phase = Phase.PLAN_DRAFT.value
        self.orch._plan_path(0).unlink()
        agreed = critique()
        agreed["verdict"] = "AGREE"
        agreed["findings"] = []
        payloads = [plan(), critique(), revision(), agreed]
        calls: list[dict] = []

        def agent(name, prompt, **kwargs):
            calls.append({"name": name, "prompt": prompt, **kwargs})
            return result(payloads.pop(0), f"session-{len(calls)}")

        self.orch._run_agent = agent
        self.orch._ensure_worktree = Mock()

        self.orch.phase_plan_draft()
        self.orch.phase_plan_critique()
        self.orch.phase_plan_revise()
        self.orch.phase_plan_critique()

        self.assertEqual(4, len(calls))
        self.assertEqual(Phase.IMPLEMENT_STEP.value, self.state.phase)
        review_calls = [call for call in calls if "plan-critique" in call["label"]]
        self.assertEqual(["", ""], [call["resume"] for call in review_calls])
        self.assertTrue(all(call["cwd"] == self.orch.run_dir for call in review_calls))
        self.assertIn("FINAL PLAN AUDIT", review_calls[-1]["prompt"])
        manifests = sorted(self.orch.run_dir.glob("evidence-plan-review-*.json"))
        self.assertEqual(2, len(manifests))

    def test_malformed_plan_delta_retains_last_canonical_plan(self) -> None:
        self.state.phase = Phase.PLAN_REVISE.value
        self.state.plan_round = 1
        baseline = plan()
        self.orch._plan_path(0).write_text(json.dumps(baseline), encoding="utf-8")
        self.orch._critique_path(0).write_text(
            json.dumps(critique()), encoding="utf-8"
        )
        malformed = revision()
        malformed["base_plan_sha256"] = "wrong-baseline"
        self.orch._run_agent = Mock(return_value=result(malformed))

        with self.assertRaisesRegex(OrchestratorError, "canonical round 0 is unchanged"):
            self.orch.phase_plan_revise()

        self.assertFalse(self.orch._plan_path(1).exists())
        self.assertTrue(self.orch._art("plan-revision-1.rejected.json").exists())
        self.assertEqual(
            baseline,
            json.loads(self.orch._plan_path(0).read_text(encoding="utf-8")),
        )

    def test_actual_decision_gates_without_consuming_revision_budget(self) -> None:
        self.cfg.max_plan_rounds = 5
        self.orch._run_agent = Mock(return_value=result(critique(decision=True)))

        self.orch.pair_plan_turn()

        self.assertEqual(Phase.AWAIT_GUIDANCE.value, self.state.phase)
        self.assertEqual("decision", self.state.gate_kind)
        self.assertEqual("Choose A or B.", self.state.gate_reason)
        self.assertEqual(0, self.state.plan_revisions)

    def test_inconclusive_plan_review_retries_reviewer_not_lead(self) -> None:
        inconclusive = critique()
        inconclusive["findings"] = []
        inconclusive["missing_evidence"] = ["local command runner denied access"]
        self.state.sessions["pair_plan"] = "contaminated-session"
        self.orch._run_agent = Mock(return_value=result(inconclusive))

        with self.assertRaisesRegex(OrchestratorError, "inconclusive"):
            self.orch.pair_plan_turn()

        self.assertEqual(Phase.PLAN_CRITIQUE.value, self.state.phase)
        self.assertEqual(0, self.state.plan_round)
        self.assertEqual("", self.state.sessions["pair_plan"])
        self.assertFalse(self.orch._critique_path(0).exists())

    def test_missing_evidence_gets_one_bounded_fresh_packet_expansion(self) -> None:
        source = self.repo / "needed.py"
        source.write_text("VALUE = 1\n", encoding="utf-8")
        missing = critique()
        missing["findings"] = []
        missing["missing_evidence"] = ["needed.py"]
        self.orch._run_agent = Mock(return_value=result(missing))

        with self.assertRaisesRegex(OrchestratorError, "bounded retry packet"):
            self.orch.pair_plan_turn()

        self.assertEqual(["needed.py"], self.state.evidence_requests)
        self.assertEqual(1, self.state.evidence_expansions)

        agreed = critique()
        agreed["verdict"] = "AGREE"
        agreed["findings"] = []
        self.orch._run_agent = Mock(return_value=result(agreed))
        self.orch._ensure_worktree = Mock()
        self.orch.pair_plan_turn()

        manifests = sorted(self.orch.run_dir.glob("evidence-plan-review-*.json"))
        self.assertEqual(2, len(manifests))
        expanded = json.loads(manifests[-1].read_text(encoding="utf-8"))
        self.assertIn(
            "repo:needed.py",
            {entry["purpose"] for entry in expanded["entries"]},
        )

    def test_decision_flag_takes_precedence_over_inconsistent_agree(self) -> None:
        inconsistent = critique(decision=True)
        inconsistent["verdict"] = "AGREE"
        self.orch._run_agent = Mock(return_value=result(inconsistent))

        self.orch.pair_plan_turn()

        self.assertEqual(Phase.AWAIT_GUIDANCE.value, self.state.phase)
        self.assertEqual("decision", self.state.gate_kind)

    def test_incident_decision_findings_do_not_override_explicit_false(self) -> None:
        incident = critique()
        incident["findings"] = [
            {
                **incident["findings"][0],
                "key": f"incident-{index}",
                "kind": "decision",
                "category": "decision",
                "severity": severity,
            }
            for index, severity in enumerate(("blocking", "major", "major"), 1)
        ]
        incident["requires_human_decision"] = False
        incident["decision_question"] = None
        self.orch._run_agent = Mock(return_value=result(incident))

        self.orch.pair_plan_turn()

        self.assertEqual(Phase.PLAN_REVISE.value, self.state.phase)
        self.assertEqual("", self.state.gate_kind)

    def test_inconsistent_decision_contract_is_a_protocol_failure(self) -> None:
        for requested, question in ((True, None), (False, "Choose A or B.")):
            invalid = critique()
            invalid["requires_human_decision"] = requested
            invalid["decision_question"] = question
            self.orch._run_agent = Mock(return_value=result(invalid))

            with self.assertRaisesRegex(OrchestratorError, "protocol violation"):
                self.orch.pair_plan_turn()

    def test_guidance_is_persistent_and_reaches_both_roles(self) -> None:
        self.state.binding_guidance = ["Use option A."]
        self.state.phase = Phase.PLAN_REVISE.value
        self.state.plan_round = 1
        self.orch._critique_path(0).write_text(json.dumps(critique()), encoding="utf-8")
        prompts_seen: list[str] = []

        def agent(_name, prompt, **_kwargs):
            prompts_seen.append(prompt)
            payload = revision() if len(prompts_seen) == 1 else critique()
            return result(payload)

        self.orch._run_agent = agent
        self.orch.phase_plan_revise()
        self.orch.pair_plan_turn()

        self.assertEqual(["Use option A."], self.state.binding_guidance)
        self.assertEqual(2, len(prompts_seen))
        self.assertTrue(all("Use option A." in prompt for prompt in prompts_seen))
        self.assertIn(str(self.orch._plan_path(0)), prompts_seen[0])
        self.assertIn("Do not reconstruct it from session memory", prompts_seen[0])

    def test_continue_extends_only_budget_gates(self) -> None:
        self.state.phase = Phase.AWAIT_GUIDANCE.value
        self.state.gate_kind = "budget"
        self.state.return_phase = Phase.PLAN_REVISE.value
        self.state.plan_revisions = 1

        extended = self.orch.continue_after_budget()

        self.assertTrue(extended)
        self.assertEqual(Phase.PLAN_REVISE.value, self.state.phase)
        self.assertEqual(1, self.state.plan_cap_extra)
        self.assertEqual([], self.state.binding_guidance)

        self.state.phase = Phase.AWAIT_GUIDANCE.value
        self.state.gate_kind = "decision"
        self.state.return_phase = Phase.PLAN_REVISE.value
        with self.assertRaises(OrchestratorError):
            self.orch.continue_after_budget()

    def test_continue_resumes_legacy_gate_before_extending_budget(self) -> None:
        self.state.phase = Phase.AWAIT_GUIDANCE.value
        self.state.gate_kind = "budget"
        self.state.return_phase = Phase.PLAN_REVISE.value
        self.state.plan_revisions = 0

        extended = self.orch.continue_after_budget()

        self.assertFalse(extended)
        self.assertEqual(0, self.state.plan_cap_extra)
        self.assertEqual(Phase.PLAN_REVISE.value, self.state.phase)

    def test_continue_default_adds_only_one_response_even_with_large_config(self) -> None:
        self.cfg.max_plan_rounds = 5
        self.state.phase = Phase.AWAIT_GUIDANCE.value
        self.state.gate_kind = "budget"
        self.state.return_phase = Phase.PLAN_REVISE.value
        self.state.plan_revisions = 10
        self.state.plan_cap_extra = 5

        self.orch.continue_after_budget()

        self.assertEqual(6, self.state.plan_cap_extra)

    def test_interrupted_legacy_continue_is_idempotent_and_narrowed(self) -> None:
        self.cfg.max_plan_rounds = 5
        self.state.phase = Phase.PLAN_REVISE.value
        self.state.plan_round = 11
        self.state.plan_revisions = 10
        self.state.plan_cap_extra = 10
        self.state.events = [
            {
                "ts": "2026-07-15T19:54:17",
                "message": "await_guidance -> plan_revise (quality budget extended)",
            }
        ]

        resumed = self.orch.resume_interrupted_continue()

        self.assertTrue(resumed)
        self.assertEqual(6, self.state.plan_cap_extra)
        self.assertEqual(11, self.cfg.max_plan_rounds + self.state.plan_cap_extra)
        self.assertTrue(self.orch.resume_interrupted_continue())

    def test_resolve_persists_guidance_without_extending_decision_budget(self) -> None:
        self.state.phase = Phase.AWAIT_GUIDANCE.value
        self.state.gate_kind = "decision"
        self.state.return_phase = Phase.PLAN_REVISE.value

        self.orch.resolve_guidance("Choose A.")

        self.assertEqual(["Choose A."], self.state.binding_guidance)
        self.assertEqual(0, self.state.plan_cap_extra)
        self.assertEqual(Phase.PLAN_REVISE.value, self.state.phase)

    def test_decision_from_final_audit_also_rearms_revision_budget(self) -> None:
        self.state.phase = Phase.AWAIT_GUIDANCE.value
        self.state.gate_kind = "decision"
        self.state.return_phase = Phase.PLAN_REVISE.value
        self.state.plan_revisions = 1

        self.orch.resolve_guidance("Choose A.")

        self.assertEqual(1, self.state.plan_cap_extra)
        self.assertEqual(Phase.PLAN_REVISE.value, self.state.phase)

    def test_live_or_headless_fix_budget_counts_completed_responses(self) -> None:
        self.state.fix_return = Phase.CHECKPOINT.value
        self.orch.record_fix_completed()
        self.state.fix_return = Phase.TESTS.value
        self.orch.record_fix_completed()
        self.state.fix_return = Phase.VERIFY.value
        self.orch.record_fix_completed()

        self.assertEqual(1, self.state.checkpoint_fixes_used)
        self.assertEqual(1, self.state.test_fixes_used)
        self.assertEqual(1, self.state.verify_fixes_used)

    def test_mechanical_gate_allows_configured_fix_before_budget_gate(self) -> None:
        self.cfg.max_test_rounds = 1
        self.cfg.test_command = 'python -c "import sys; sys.exit(1)"'
        self.state.worktree = str(self.repo)
        self.state.phase = Phase.TESTS.value

        self.assertFalse(self.orch.run_test_gate())
        self.assertEqual(Phase.FIX.value, self.state.phase)

        self.orch.record_fix_completed()
        self.state.phase = Phase.TESTS.value
        self.assertFalse(self.orch.run_test_gate())
        self.assertEqual(Phase.AWAIT_GUIDANCE.value, self.state.phase)
        self.assertEqual("budget", self.state.gate_kind)

    def test_continue_command_is_exposed(self) -> None:
        args = build_parser().parse_args(["continue", "--repo", str(self.repo)])
        self.assertEqual("continue", args.command)

    def test_restart_state_preserves_guidance_and_plan_checkpoint(self) -> None:
        self.state.binding_guidance = ["Keep the Preview disposition."]
        self.state.plan_round = 11
        self.state.plan_revisions = 11

        replacement = _replacement_state(self.cfg, self.state)

        self.assertEqual(["Keep the Preview disposition."], replacement.binding_guidance)
        self.assertEqual(2, replacement.plan_protocol_version)
        self.assertEqual(self.state.phase, replacement.phase)
        self.assertEqual(11, replacement.plan_round)

    def test_legacy_planning_run_must_restart_not_extend(self) -> None:
        self.state.plan_protocol_version = 1
        self.state.phase = Phase.AWAIT_GUIDANCE.value
        self.state.gate_kind = "budget"
        self.state.return_phase = Phase.PLAN_REVISE.value

        with self.assertRaisesRegex(OrchestratorError, "claudex cancel"):
            self.orch.continue_after_budget()

    def test_resolve_accepts_multiline_guidance_from_file_or_stdin(self) -> None:
        path = self.repo / "decision.md"
        path.write_text("Choose A.\nPreserve compatibility.\n", encoding="utf-8")
        args = build_parser().parse_args(["resolve", "--notes-file", str(path)])
        self.assertEqual(
            "Choose A.\nPreserve compatibility.\n", _read_guidance_notes(args)
        )

        args = build_parser().parse_args(["resolve", "--notes-file", "-"])
        stream = io.StringIO("Piped decision.\n")
        stream.isatty = lambda: False
        with patch("claudex.cli.sys.stdin", stream):
            self.assertEqual("Piped decision.\n", _read_guidance_notes(args))

    def test_plan_schema_requires_stable_keys_and_decision_classification(self) -> None:
        finding_properties = schemas.PLAN_FINDING["properties"]
        self.assertIn("key", finding_properties)
        self.assertIn("kind", finding_properties)
        self.assertNotIn("content", finding_properties["category"]["enum"])
        self.assertIn("requires_human_decision", schemas.PLAN_CRITIQUE_SCHEMA["required"])
        self.assertIn("implementation_checks", schemas.PLAN_CRITIQUE_SCHEMA["required"])

    def test_implementation_checks_do_not_block_plan_agreement(self) -> None:
        agreed = critique()
        agreed["verdict"] = "AGREE"
        agreed["findings"] = []
        agreed["implementation_checks"] = [
            {
                "key": "verify-current-client-prerequisite",
                "description": "Verify the current client prerequisite in primary docs.",
                "evidence": "The fact is volatile and belongs in the committed matrix.",
                "action": "add",
                "target_step": "Implement",
            }
        ]
        self.orch._run_agent = Mock(return_value=result(agreed))
        self.orch._ensure_worktree = Mock()

        self.orch.pair_plan_turn()

        self.assertEqual(Phase.IMPLEMENT_STEP.value, self.state.phase)
        self.assertEqual(1, len(self.state.implementation_checks))
        ledger = json.loads(
            (self.orch.run_dir / "implementation-checks.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertEqual(
            "verify-current-client-prerequisite", ledger["checks"][0]["key"]
        )

    def test_plan_size_is_not_an_arbitrary_validity_rule(self) -> None:
        large_but_structurally_valid = plan()
        large_but_structurally_valid["plan_markdown"] = "x" * 50_000

        validate_plan_shape(large_but_structurally_valid)

    def test_revision_must_answer_every_keyed_major_finding_once(self) -> None:
        missing = revision()
        missing["responses"] = []
        with self.assertRaisesRegex(OrchestratorError, "missing-test"):
            validate_revision_responses(critique(), missing)

        duplicate = revision()
        duplicate["responses"].append(dict(duplicate["responses"][0]))
        with self.assertRaisesRegex(OrchestratorError, "duplicate"):
            validate_revision_responses(critique(), duplicate)

    def test_verification_pass_label_cannot_override_failed_evidence(self) -> None:
        verdict = {
            "criteria": [
                {"criterion": "preserves data", "met": False, "evidence": "test failed"}
            ],
            "scope_expansion": ["modified an unrelated file"],
            "tests_meaningful": False,
            "unsupported_claims": ["claimed compatibility without a test"],
            "verdict": "pass",
            "notes": "",
        }

        findings = verification_findings(verdict)

        self.assertEqual(4, len(findings))
        self.assertTrue(all(f["severity"] in ("blocking", "major") for f in findings))

    def test_verify_prompt_carries_binding_guidance(self) -> None:
        text = prompts.verify(
            Path("task.md"),
            Path("plan.md"),
            Path("implementation-checks.json"),
            Path("diff.patch"),
            "abcdef",
            guidance="Decision 1: preserve user data.",
        )
        self.assertIn("preserve user data", text)


class StateMigrationTests(unittest.TestCase):
    def test_v02_state_migrates_guidance_and_revision_count(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            old = {
                "run_id": "old",
                "repo": temp,
                "lead": "claude",
                "phase": Phase.PLAN_REVISE.value,
                "guidance_notes": "Keep the simple design.",
            }
            (run_dir / "state.json").write_text(json.dumps(old), encoding="utf-8")
            (run_dir / "plan-round-0.json").write_text("{}", encoding="utf-8")
            (run_dir / "plan-round-3.json").write_text("{}", encoding="utf-8")
            (run_dir / "mailbox.md").write_text(
                "===== [HUMAN] turn 2 | guidance | STATUS: CONTINUE =====\n"
                "quality budget extended\n"
                "----- end [HUMAN] turn 2 -----\n"
                "===== [HUMAN] turn 3 | guidance | STATUS: GUIDANCE =====\n"
                "Also preserve compatibility.\n"
                "----- end [HUMAN] turn 3 -----\n",
                encoding="utf-8",
            )

            state = RunState.load(run_dir)

            self.assertEqual(
                ["Keep the simple design.", "Also preserve compatibility."],
                state.binding_guidance,
            )
            self.assertFalse(hasattr(state, "guidance_notes"))
            self.assertEqual(3, state.plan_revisions)
            self.assertEqual(1, state.plan_protocol_version)


if __name__ == "__main__":
    unittest.main()
