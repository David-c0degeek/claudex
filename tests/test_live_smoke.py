from __future__ import annotations

import os
import tempfile
import time
import unittest
from pathlib import Path

from claudex import budgets
from claudex.config import Config
from claudex.phases import build_agents
from claudex.providers import resolve_provider
from claudex.state import RunState


ACK = "I_ACCEPT_CAPPED_PROVIDER_COSTS"


@unittest.skipUnless(
    os.environ.get("CLAUDEX_RUN_LIVE_SMOKE") == "1",
    "live provider smoke is credential-explicit and disabled by default",
)
class LiveProviderCompatibilitySmoke(unittest.TestCase):
    def test_both_installed_providers_honor_safe_compatibility_contract(self) -> None:
        if os.environ.get("CLAUDEX_LIVE_SMOKE_ACK") != ACK:
            self.fail(
                "set CLAUDEX_LIVE_SMOKE_ACK=I_ACCEPT_CAPPED_PROVIDER_COSTS "
                "to acknowledge the paid compatibility smoke"
            )
        infos = {}
        for provider in ("claude", "codex"):
            info = resolve_provider(provider)
            infos[provider] = info
            required = ("stream", "schema", "sandbox", "session", "nested_agent_control")
            missing = [name for name in required if not info.capabilities[name]]
            self.assertFalse(missing, f"{provider} preflight missing {missing}")
        missing_native_budget = [
            name for name, info in infos.items() if not info.capabilities["budget"]
        ]
        if missing_native_budget:
            self.skipTest(
                "preflight refusal before invocation: no provider-native spend "
                "cap for " + ", ".join(missing_native_budget)
            )

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            cfg = Config(
                repo=root,
                planning_effort="low",
                implementation_effort="low",
                verification_effort="low",
                max_invocation_cost_usd=0.05,
                max_invocation_turns=1,
                max_invocation_output_tokens=2_000,
                max_invocation_tool_calls=0,
                max_run_invocations=2,
                max_run_input_tokens=20_000,
                max_run_output_tokens=4_000,
                max_run_cost_usd=0.10,
                max_run_tool_calls=0,
                max_run_wall_seconds=120,
                agent_timeout=60,
                disable_nested_agents=True,
            )
            cfg.validate()
            state = RunState("live-smoke", str(root), "claude")
            state.started_epoch_s = time.time()
            state.run_policy = budgets.capture_run_policy(cfg)
            agents = build_agents(cfg)
            schema = {
                "type": "object",
                "properties": {"ok": {"type": "boolean"}},
                "required": ["ok"],
                "additionalProperties": False,
            }
            for name in ("claude", "codex"):
                self.assertFalse(budgets.budget_violations(cfg, state))
                policy = budgets.invocation_policy(cfg, state, "planning-smoke", name)
                budgets.record_attempt_started(state, policy)
                result = agents[name].run(
                    "Return {\"ok\": true}. Do not use tools.",
                    cwd=root,
                    run_dir=root / "run",
                    label=f"{name}-live-smoke",
                    read_only=True,
                    schema=schema,
                    timeout=policy.timeout_seconds,
                    policy=policy,
                )
                self.assertTrue(result.ok, result.error)
                self.assertEqual({"ok": True}, result.structured)
                budgets.record_result(state, result)
            self.assertLessEqual(state.provider_invocations, 2)
            self.assertLessEqual(state.provider_cost_usd, 0.10)


if __name__ == "__main__":
    unittest.main()
