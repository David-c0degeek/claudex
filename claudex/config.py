"""Configuration: defaults < .claudex/config.json < CLI flags < env vars.

Env vars CLAUDEX_CLAUDE_BIN / CLAUDEX_CODEX_BIN always win for binary paths
(handled in agents.resolve_*), because binary location is a machine property,
not a project property.
"""

from __future__ import annotations

import dataclasses
import json
from dataclasses import dataclass, field
from pathlib import Path

CONFIG_NAME = "config.json"


@dataclass
class Config:
    repo: Path
    claude_bin: str = ""
    codex_bin: str = ""
    claude_model: str = ""
    codex_model: str = ""
    lead: str = "claude"  # who holds the pen: claude | codex
    mode: str = "auto"  # auto (detect from goal prefix) | report | change
    # Lead-response budgets. A value N permits N complete revise/fix cycles
    # followed by a final fresh review of the Nth response. Exhaustion gates
    # as a budget stop; it is not mislabeled as a model disagreement.
    max_plan_rounds: int = 5  # lead plan revisions
    max_checkpoint_rounds: int = 3  # lead fixes per step
    max_test_rounds: int = 2  # fixes after mechanical test failures
    max_verify_rounds: int = 2  # fixes after verification failures
    # Mechanical test gate: run in the worktree after the last step; exit
    # code decides, never agent testimony. Empty = skip the mechanical gate.
    test_command: str = ""
    agent_timeout: int = 3600  # seconds per agent invocation
    # Economic envelope. Terminal provider usage is charged once per attempt;
    # intermediate stream events are display-only to avoid double counting.
    max_invocation_cost_usd: float = 3.0  # native Claude cap; admission only for Codex
    max_invocation_turns: int = 12
    max_invocation_output_tokens: int = 50_000
    max_invocation_tool_calls: int = 50
    max_run_invocations: int = 20
    max_run_input_tokens: int = 5_000_000
    max_run_output_tokens: int = 250_000
    max_run_cost_usd: float = 12.0
    max_run_tool_calls: int = 250
    max_run_wall_seconds: int = 3 * 3600
    planning_effort: str = "high"
    implementation_effort: str = "high"
    verification_effort: str = "high"
    claude_planning_model: str = ""
    claude_implementation_model: str = ""
    claude_verification_model: str = ""
    codex_planning_model: str = ""
    codex_implementation_model: str = ""
    codex_verification_model: str = ""
    disable_nested_agents: bool = True
    allow_expensive_profiles: bool = False
    # Usage-limit handling: when a provider reports a usage/rate limit, wait
    # until the reset time it names (or default_limit_wait when it names
    # none) and retry, instead of failing the run.
    wait_on_limits: bool = True
    default_limit_wait: int = 1800  # seconds, when the message names no time
    max_limit_wait: int = 6 * 3600  # cap a single wait
    max_limit_waits: int = 12  # per agent invocation
    claude_write_allowed_tools: str = "Edit,Write,NotebookEdit,TodoWrite,Bash"
    claude_extra_args: list = field(default_factory=list)
    codex_extra_args: list = field(default_factory=list)

    _PERSISTED = (
        "claude_bin",
        "codex_bin",
        "claude_model",
        "codex_model",
        "lead",
        "mode",
        "max_plan_rounds",
        "max_checkpoint_rounds",
        "max_test_rounds",
        "max_verify_rounds",
        "test_command",
        "agent_timeout",
        "max_invocation_cost_usd",
        "max_invocation_turns",
        "max_invocation_output_tokens",
        "max_invocation_tool_calls",
        "max_run_invocations",
        "max_run_input_tokens",
        "max_run_output_tokens",
        "max_run_cost_usd",
        "max_run_tool_calls",
        "max_run_wall_seconds",
        "planning_effort",
        "implementation_effort",
        "verification_effort",
        "claude_planning_model",
        "claude_implementation_model",
        "claude_verification_model",
        "codex_planning_model",
        "codex_implementation_model",
        "codex_verification_model",
        "disable_nested_agents",
        "allow_expensive_profiles",
        "wait_on_limits",
        "default_limit_wait",
        "max_limit_wait",
        "max_limit_waits",
        "claude_write_allowed_tools",
        "claude_extra_args",
        "codex_extra_args",
    )

    @property
    def config_path(self) -> Path:
        return self.repo / ".claudex" / CONFIG_NAME

    def save(self) -> None:
        self.validate()
        self.config_path.parent.mkdir(parents=True, exist_ok=True)
        data = {k: getattr(self, k) for k in self._PERSISTED}
        self.config_path.write_text(json.dumps(data, indent=2), encoding="utf-8")

    @classmethod
    def load(cls, repo: Path, overrides: dict | None = None) -> "Config":
        cfg = cls(repo=repo.resolve())
        path = cfg.config_path
        if path.exists():
            stored = json.loads(path.read_text(encoding="utf-8"))
            for k in cls._PERSISTED:
                if k in stored:
                    setattr(cfg, k, stored[k])
        known = {f.name for f in dataclasses.fields(cls)}
        for k, v in (overrides or {}).items():
            if v is not None and k in known:
                setattr(cfg, k, v)
        cfg.validate()
        return cfg

    def validate(self) -> None:
        positive_integers = {
            "agent_timeout": self.agent_timeout,
            "max_invocation_turns": self.max_invocation_turns,
            "default_limit_wait": self.default_limit_wait,
            "max_limit_wait": self.max_limit_wait,
        }
        for name, value in positive_integers.items():
            if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
                raise ValueError(f"{name} must be a positive integer")
        if (
            not isinstance(self.max_invocation_cost_usd, (int, float))
            or isinstance(self.max_invocation_cost_usd, bool)
            or self.max_invocation_cost_usd <= 0
        ):
            raise ValueError("max_invocation_cost_usd must be a positive number")
        non_negative_integers = {
            "max_plan_rounds": self.max_plan_rounds,
            "max_checkpoint_rounds": self.max_checkpoint_rounds,
            "max_test_rounds": self.max_test_rounds,
            "max_verify_rounds": self.max_verify_rounds,
            "max_limit_waits": self.max_limit_waits,
            "max_run_invocations": self.max_run_invocations,
            "max_invocation_output_tokens": self.max_invocation_output_tokens,
            "max_invocation_tool_calls": self.max_invocation_tool_calls,
            "max_run_input_tokens": self.max_run_input_tokens,
            "max_run_output_tokens": self.max_run_output_tokens,
            "max_run_tool_calls": self.max_run_tool_calls,
            "max_run_wall_seconds": self.max_run_wall_seconds,
        }
        for name, value in non_negative_integers.items():
            if not isinstance(value, int) or isinstance(value, bool) or value < 0:
                raise ValueError(f"{name} must be a non-negative integer")
        if (
            not isinstance(self.max_run_cost_usd, (int, float))
            or isinstance(self.max_run_cost_usd, bool)
            or self.max_run_cost_usd < 0
        ):
            raise ValueError("max_run_cost_usd must be a non-negative number")
        for name in ("disable_nested_agents", "allow_expensive_profiles"):
            if not isinstance(getattr(self, name), bool):
                raise ValueError(f"{name} must be true or false")
        for name in ("claude_extra_args", "codex_extra_args"):
            if not isinstance(getattr(self, name), list):
                raise ValueError(f"{name} must be an array of CLI arguments")
        model_fields = (
            "claude_model",
            "codex_model",
            "claude_planning_model",
            "claude_implementation_model",
            "claude_verification_model",
            "codex_planning_model",
            "codex_implementation_model",
            "codex_verification_model",
        )
        for name in model_fields:
            if not isinstance(getattr(self, name), str):
                raise ValueError(f"{name} must be a model name string")
        allowed_efforts = {"low", "medium", "high", "xhigh", "max"}
        efforts = {
            "planning_effort": self.planning_effort,
            "implementation_effort": self.implementation_effort,
            "verification_effort": self.verification_effort,
        }
        for name, value in efforts.items():
            if value not in allowed_efforts:
                raise ValueError(
                    f"{name} must be one of {sorted(allowed_efforts)}, got {value!r}"
                )
        expensive_args = " ".join(
            str(item).lower()
            for item in (*self.claude_extra_args, *self.codex_extra_args)
        )
        expensive = any(value in {"xhigh", "max"} for value in efforts.values())
        expensive = expensive or any(
            marker in expensive_args.replace('"', "").replace("'", "")
            for marker in (
                "effort xhigh",
                "effort=xhigh",
                "effort max",
                "effort=max",
            )
        )
        if expensive and not self.allow_expensive_profiles:
            raise ValueError(
                "maximum-cost reasoning effort requires allow_expensive_profiles=true "
                "or --allow-expensive-profiles"
            )
