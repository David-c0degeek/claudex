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
    # Hard round caps — the "this is good enough" stop rules. A cap hit
    # never loops silently: the run gates on AWAIT_GUIDANCE for the human.
    max_plan_rounds: int = 5  # plan critique/revise cycles
    max_checkpoint_rounds: int = 3  # review/fix cycles per step
    max_test_rounds: int = 2  # test-gate failures -> fix cycles
    max_verify_rounds: int = 2  # verify failures -> fix cycles
    # Mechanical test gate: run in the worktree after the last step; exit
    # code decides, never agent testimony. Empty = skip the mechanical gate.
    test_command: str = ""
    agent_timeout: int = 3600  # seconds per agent invocation
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
        return cfg
