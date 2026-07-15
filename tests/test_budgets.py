from __future__ import annotations

import io
import json
import tempfile
import time
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from claudex import budgets
from claudex.agents import AgentResult, ClaudeAgent, CodexAgent
from claudex.cli import _print_economic_summary, build_parser, cmd_status
from claudex.config import Config
from claudex.events import EventEmitter, EventJournal
from claudex.phases import BudgetPause, Orchestrator, OrchestratorError
from claudex.processes import ExecResult
from claudex.state import (
    STATE_SCHEMA_VERSION,
    Phase,
    RunState,
    run_dir_for,
    set_current_run,
)


def state_for(root: Path, **values) -> RunState:
    defaults = {
        "run_id": "run",
        "repo": str(root),
        "lead": "claude",
        "phase": Phase.PLAN_DRAFT.value,
    }
    defaults.update(values)
    return RunState(**defaults)


def emitter_for(attempt, agent: str) -> EventEmitter:
    import time

    return EventEmitter(
        journal=EventJournal(attempt.events),
        run_id="run",
        attempt_id=attempt.attempt_id,
        agent=agent,
        phase="fixture",
        started_monotonic=time.monotonic(),
    )


class UsageAccountingTests(unittest.TestCase):
    def test_normalizes_provider_categories_without_double_counting_subsets(self) -> None:
        codex = budgets.normalize_usage(
            {
                "input_tokens": 100,
                "cached_input_tokens": 40,
                "output_tokens": 30,
                "reasoning_tokens": 20,
            }
        )
        claude = budgets.normalize_usage(
            {
                "input_tokens": 60,
                "cache_read_input_tokens": 40,
                "cache_creation_input_tokens": 5,
                "output_tokens": 10,
            }
        )

        self.assertEqual(100, budgets.total_reported_input(codex))
        self.assertEqual(30, budgets.total_reported_output(codex))
        self.assertEqual(105, budgets.total_reported_input(claude))

    def test_terminal_result_is_idempotent_and_missing_fields_stay_unknown(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            state = state_for(Path(temp))
            result = AgentResult(
                ok=True,
                attempt_id="attempt-1",
                usage={"input_tokens": 10, "output_tokens": 2},
                duration_s=1.5,
                cost_usd=None,
                tool_calls=3,
                tool_calls_observed=True,
            )

            self.assertTrue(budgets.record_result(state, result))
            self.assertFalse(budgets.record_result(state, result))

            self.assertEqual(10, state.provider_usage["input_tokens"])
            self.assertEqual(1, state.unknown_cost_attempts)
            self.assertEqual(0.0, state.provider_cost_usd)
            self.assertEqual(3, state.provider_tool_calls)

    def test_foreign_currency_is_preserved_and_requires_acknowledgement(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = Config(repo=root)
            state = state_for(root)
            budgets.record_result(
                state,
                AgentResult(
                    ok=True,
                    attempt_id="eur-1",
                    reported_cost=1.25,
                    cost_currency="EUR",
                ),
            )

            self.assertEqual({"EUR": 1.25}, state.provider_costs)
            self.assertEqual(0.0, state.provider_cost_usd)
            self.assertIn("currency", budgets.budget_violations(cfg, state)[0])
            state.acknowledged_cost_currencies.append("EUR")
            self.assertEqual([], budgets.budget_violations(cfg, state))


class ConfigPolicyTests(unittest.TestCase):
    def test_legacy_minimal_config_migrates_to_safe_defaults(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            config_dir = root / ".claudex"
            config_dir.mkdir()
            (config_dir / "config.json").write_text(
                json.dumps({"lead": "codex", "agent_timeout": 30}),
                encoding="utf-8",
            )

            cfg = Config.load(root)

            self.assertEqual("codex", cfg.lead)
            self.assertTrue(cfg.disable_nested_agents)
            self.assertEqual("high", cfg.planning_effort)

    def test_run_state_schema_migrates_old_and_rejects_future_versions(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            run_dir = Path(temp)
            old = {"run_id": "run", "repo": temp, "lead": "claude"}
            (run_dir / "state.json").write_text(json.dumps(old), encoding="utf-8")

            loaded = RunState.load(run_dir)
            self.assertEqual(STATE_SCHEMA_VERSION, loaded.state_schema_version)

            old["state_schema_version"] = STATE_SCHEMA_VERSION + 1
            (run_dir / "state.json").write_text(json.dumps(old), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "newer than supported"):
                RunState.load(run_dir)

    def test_rejects_negative_or_wrong_typed_limits(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            with self.assertRaisesRegex(ValueError, "max_run_invocations"):
                Config(repo=root, max_run_invocations=-1).validate()
            with self.assertRaisesRegex(ValueError, "max_invocation_turns"):
                Config(repo=root, max_invocation_turns=1.5).validate()
            with self.assertRaisesRegex(ValueError, "disable_nested_agents"):
                Config(repo=root, disable_nested_agents="false").validate()

    def test_maximum_effort_requires_explicit_opt_in(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            with self.assertRaisesRegex(ValueError, "maximum-cost"):
                Config(repo=root, planning_effort="xhigh").validate()
            Config(
                repo=root,
                planning_effort="xhigh",
                allow_expensive_profiles=True,
            ).validate()

    def test_provider_maximum_effort_uses_each_cli_native_value(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = Config(
                repo=root,
                planning_effort="max",
                allow_expensive_profiles=True,
            )
            state = state_for(root, run_policy=budgets.capture_run_policy(cfg))

            claude = budgets.invocation_policy(cfg, state, "plan", "claude")
            codex = budgets.invocation_policy(cfg, state, "plan", "codex")

            self.assertEqual("max", claude.effort)
            self.assertEqual("xhigh", codex.effort)
            self.assertEqual("max", codex.requested_effort)

    def test_phase_model_is_frozen_into_invocation_policy(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = Config(repo=root, codex_verification_model="review-model")
            state = state_for(root, run_policy=budgets.capture_run_policy(cfg))

            policy = budgets.invocation_policy(cfg, state, "checkpoint", "codex")

            self.assertEqual("verification", policy.profile)
            self.assertEqual("review-model", policy.model)

    def test_start_summary_makes_expensive_profile_and_nested_opt_in_prominent(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = Config(
                repo=root,
                planning_effort="xhigh",
                allow_expensive_profiles=True,
                disable_nested_agents=False,
            )
            state = state_for(root, run_policy=budgets.capture_run_policy(cfg))
            output = io.StringIO()

            with redirect_stdout(output):
                _print_economic_summary(cfg, state)

            rendered = output.getvalue()
            self.assertIn("EXPLICIT HIGH-COST OPT-IN", rendered)
            self.assertIn("ENABLED (explicit opt-in)", rendered)

    def test_budget_flags_and_resume_command_are_exposed(self) -> None:
        parser = build_parser()
        args = parser.parse_args(
            [
                "run",
                "--max-run-invocations",
                "3",
                "--planning-effort",
                "high",
                "--allow-nested-agents",
            ]
        )
        resume = parser.parse_args(["resume", "--add-invocations", "2"])

        self.assertEqual(3, args.max_run_invocations)
        self.assertFalse(args.disable_nested_agents)
        self.assertEqual(2, resume.add_invocations)


class ProviderCommandPolicyTests(unittest.TestCase):
    def setUp(self) -> None:
        self.policy = budgets.InvocationPolicy(
            profile="planning",
            model="fixture-model",
            requested_effort="high",
            effort="high",
            max_budget_usd=1.25,
            max_turns=7,
            timeout_seconds=15,
            disable_nested_agents=True,
        )

    def _exec(self, captured: dict, agent: str):
        def fake(cmd, **kwargs):
            captured["cmd"] = cmd
            attempt = kwargs["attempt"]
            if agent == "claude":
                output = json.dumps(
                    {
                        "type": "result",
                        "subtype": "success",
                        "result": "{}",
                        "structured_output": {},
                        "usage": {},
                        "total_cost_usd": 0.0,
                    }
                )
            else:
                attempt.last_message.write_text("{}", encoding="utf-8")
                output = json.dumps({"type": "turn.completed", "usage": {}})
            return ExecResult(
                returncode=0,
                stdout=output + "\n",
                stderr="",
                duration_s=0.1,
                attempt=attempt,
                emitter=emitter_for(attempt, agent),
            )

        return fake

    def test_claude_command_has_native_caps_effort_and_nested_agent_block(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            captured: dict = {}
            with patch("claudex.agents.stream_process", side_effect=self._exec(captured, "claude")):
                ClaudeAgent(binary="claude").run(
                    "prompt",
                    cwd=root,
                    run_dir=root / "run",
                    label="plan",
                    schema={"type": "object"},
                    policy=self.policy,
                )

            cmd = captured["cmd"]
            self.assertEqual("high", cmd[cmd.index("--effort") + 1])
            self.assertEqual("1.25", cmd[cmd.index("--max-budget-usd") + 1])
            self.assertEqual("7", cmd[cmd.index("--max-turns") + 1])
            self.assertEqual(["Agent", "Task"], cmd[-2:])

    def test_codex_command_has_effort_and_multi_agent_disable(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            captured: dict = {}
            with patch("claudex.agents.stream_process", side_effect=self._exec(captured, "codex")):
                CodexAgent(binary="codex").run(
                    "prompt",
                    cwd=root,
                    run_dir=root / "run",
                    label="review",
                    schema={"type": "object"},
                    policy=self.policy,
                )

            cmd = captured["cmd"]
            self.assertIn('model_reasoning_effort="high"', cmd)
            self.assertEqual("multi_agent", cmd[cmd.index("--disable") + 1])

    def test_nested_agent_opt_in_omits_provider_blocks(self) -> None:
        policy = budgets.InvocationPolicy(
            profile="planning",
            model="",
            requested_effort="high",
            effort="high",
            max_budget_usd=1.0,
            max_turns=3,
            timeout_seconds=15,
            disable_nested_agents=False,
        )
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            captured: dict = {}
            with patch("claudex.agents.stream_process", side_effect=self._exec(captured, "codex")):
                CodexAgent(binary="codex").run(
                    "prompt",
                    cwd=root,
                    run_dir=root / "run",
                    label="review",
                    policy=policy,
                )
            self.assertNotIn("--disable", captured["cmd"])


class AdmissionAndResumeTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def orchestrator(self, cfg: Config, state: RunState, agent) -> Orchestrator:
        with patch("claudex.phases.build_agents", return_value={"claude": agent}):
            orch = Orchestrator(cfg, state)
        return orch

    def test_exact_limit_pauses_before_provider_is_called(self) -> None:
        cfg = Config(repo=self.root, max_run_invocations=0)
        state = state_for(self.root)
        agent = SimpleNamespace(model="", run=Mock())
        orch = self.orchestrator(cfg, state, agent)

        with self.assertRaises(BudgetPause):
            orch._run_agent("claude", "prompt", cwd=self.root, label="plan")

        self.assertEqual(Phase.PAUSED_BUDGET.value, state.phase)
        agent.run.assert_not_called()
        loaded = RunState.load(run_dir_for(self.root, "run"))
        self.assertEqual(Phase.PAUSED_BUDGET.value, loaded.phase)

    def test_within_call_overshoot_is_charged_then_next_call_is_denied(self) -> None:
        cfg = Config(repo=self.root, max_run_input_tokens=10)
        state = state_for(self.root)
        result = AgentResult(
            ok=True,
            attempt_id="overshoot",
            usage={"input_tokens": 12},
            duration_s=0.1,
            cost_usd=0.01,
            cost_currency="USD",
        )
        agent = SimpleNamespace(model="test", run=Mock(return_value=result))
        orch = self.orchestrator(cfg, state, agent)

        self.assertIs(result, orch._run_agent("claude", "one", cwd=self.root, label="plan"))
        with self.assertRaises(BudgetPause):
            orch._run_agent("claude", "two", cwd=self.root, label="plan")

        self.assertEqual(1, agent.run.call_count)
        self.assertIn("input_tokens exhausted", state.gate_reason)

    def test_per_invocation_output_and_tool_overshoots_block_the_next_call(self) -> None:
        cfg = Config(
            repo=self.root,
            max_invocation_output_tokens=10,
            max_invocation_tool_calls=2,
        )
        state = state_for(self.root)
        result = AgentResult(
            ok=True,
            attempt_id="wide",
            usage={"output_tokens": 11},
            tool_calls=3,
            tool_calls_observed=True,
        )
        agent = SimpleNamespace(model="", run=Mock(return_value=result))
        orch = self.orchestrator(cfg, state, agent)

        orch._run_agent("claude", "one", cwd=self.root, label="plan")
        with self.assertRaises(BudgetPause):
            orch._run_agent("claude", "two", cwd=self.root, label="plan")

        self.assertIn("last invocation output_tokens exceeded", state.gate_reason)
        self.assertEqual(1, agent.run.call_count)

    def test_run_wall_clock_cap_is_an_admission_limit(self) -> None:
        cfg = Config(repo=self.root, max_run_wall_seconds=5)
        state = state_for(self.root, started_epoch_s=time.time() - 6)
        agent = SimpleNamespace(model="", run=Mock())
        orch = self.orchestrator(cfg, state, agent)

        with self.assertRaises(BudgetPause):
            orch._run_agent("claude", "late", cwd=self.root, label="plan")

        self.assertIn("wall_seconds exhausted", state.gate_reason)
        agent.run.assert_not_called()

    def test_per_invocation_tool_limit_is_checked_when_observable(self) -> None:
        cfg = Config(repo=self.root, max_invocation_tool_calls=2)
        state = state_for(self.root, last_attempt_tool_calls=3)

        violations = budgets.budget_violations(cfg, state)

        self.assertIn("last invocation tool_calls exceeded", violations[0])

    def test_policy_is_frozen_and_invocation_counted_before_launch(self) -> None:
        cfg = Config(repo=self.root, max_run_invocations=2, planning_effort="medium")
        state = state_for(self.root, run_policy=budgets.capture_run_policy(cfg))
        observed: dict = {}

        def run(*_args, **kwargs):
            observed["saved"] = RunState.load(run_dir_for(self.root, "run")).provider_invocations
            observed["policy"] = kwargs["policy"]
            return AgentResult(ok=True, attempt_id="one")

        agent = SimpleNamespace(model="", run=run)
        orch = self.orchestrator(Config(repo=self.root, planning_effort="high"), state, agent)
        orch._run_agent("claude", "one", cwd=self.root, label="plan-draft")

        self.assertEqual(1, observed["saved"])
        self.assertEqual("medium", observed["policy"].effort)

    def test_budget_resume_requires_sufficient_explicit_addition(self) -> None:
        cfg = Config(repo=self.root, max_run_invocations=1)
        state = state_for(
            self.root,
            phase=Phase.PAUSED_BUDGET.value,
            provider_invocations=1,
            gate_kind="run_budget",
            gate_reason="invocations exhausted",
            return_phase=Phase.PLAN_DRAFT.value,
        )
        orch = self.orchestrator(cfg, state, SimpleNamespace(model="", run=Mock()))

        with self.assertRaisesRegex(OrchestratorError, "does not clear"):
            orch.resume_budget({"input_tokens": 1})
        orch.resume_budget({"invocations": 1})

        self.assertEqual(Phase.PLAN_DRAFT.value, state.phase)
        self.assertEqual(1, state.budget_overrides["invocations"])

    def test_status_labels_unknowns_and_remaining_envelope(self) -> None:
        cfg = Config(repo=self.root, max_run_invocations=3)
        state = state_for(
            self.root,
            provider_invocations=1,
            unknown_cost_attempts=1,
            unknown_usage_attempts=1,
            run_policy=budgets.capture_run_policy(cfg),
        )
        state.save(run_dir_for(self.root, state.run_id))
        set_current_run(self.root, state.run_id)
        args = SimpleNamespace(repo=str(self.root))
        output = io.StringIO()

        with redirect_stdout(output):
            cmd_status(args)

        rendered = output.getvalue()
        self.assertIn("remaining 2", rendered)
        self.assertIn("1 unknown-cost attempt", rendered)
        self.assertIn("did not report token usage", rendered)


if __name__ == "__main__":
    unittest.main()
