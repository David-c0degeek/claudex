"""Normalized provider usage, invocation policy, and admission accounting."""

from __future__ import annotations

import time
from dataclasses import asdict, dataclass


USAGE_KEYS = (
    "input_tokens",
    "cached_input_tokens",
    "cache_creation_input_tokens",
    "output_tokens",
    "reasoning_output_tokens",
)
EXPENSIVE_EFFORTS = {"xhigh", "max"}


@dataclass(frozen=True)
class InvocationPolicy:
    profile: str
    model: str
    requested_effort: str
    effort: str
    max_budget_usd: float | None
    max_turns: int
    timeout_seconds: int
    disable_nested_agents: bool
    capability: str = "repo_read"


def normalize_usage(raw: dict | None) -> dict[str, int]:
    """Map Claude/Codex terminal usage into stable non-overlapping buckets."""
    value = raw or {}

    def number(*names: str) -> int:
        for name in names:
            item = value.get(name)
            if isinstance(item, (int, float)) and item >= 0:
                return int(item)
        return 0

    input_tokens = number("input_tokens")
    cached_input = number("cached_input_tokens", "cache_read_input_tokens")
    output_tokens = number("output_tokens")
    reasoning_output = number("reasoning_output_tokens", "reasoning_tokens")
    # Codex reports cached input as a subset of input_tokens; Anthropic's
    # cache_read_input_tokens is a separate category. Normalize both into
    # mutually exclusive buckets while retaining every reported token.
    if "cached_input_tokens" in value:
        input_tokens = max(0, input_tokens - cached_input)
    # Providers that expose reasoning details generally include them in the
    # reported output total. Store the non-reasoning remainder separately.
    if reasoning_output:
        output_tokens = max(0, output_tokens - reasoning_output)
    normalized = {
        "input_tokens": input_tokens,
        "cached_input_tokens": cached_input,
        "cache_creation_input_tokens": number("cache_creation_input_tokens"),
        "output_tokens": output_tokens,
        "reasoning_output_tokens": reasoning_output,
    }
    normalized["total_tokens"] = total_reported_input(
        normalized
    ) + total_reported_output(normalized)
    return normalized


def empty_usage() -> dict[str, int]:
    return {key: 0 for key in USAGE_KEYS}


def total_reported_input(usage: dict) -> int:
    return sum(
        int(usage.get(key, 0) or 0)
        for key in (
            "input_tokens",
            "cached_input_tokens",
            "cache_creation_input_tokens",
        )
    )


def total_reported_output(usage: dict) -> int:
    return sum(
        int(usage.get(key, 0) or 0)
        for key in ("output_tokens", "reasoning_output_tokens")
    )


def _effective(configured: int | float, overrides: dict, key: str) -> int | float:
    return configured + max(0, overrides.get(key, 0) or 0)


def capture_run_policy(cfg) -> dict[str, object]:
    """Freeze CLI/config economics so resume cannot silently change them."""
    return {
        "max_invocation_cost_usd": cfg.max_invocation_cost_usd,
        "max_invocation_turns": cfg.max_invocation_turns,
        "max_invocation_output_tokens": cfg.max_invocation_output_tokens,
        "max_invocation_tool_calls": cfg.max_invocation_tool_calls,
        "max_run_invocations": cfg.max_run_invocations,
        "max_run_input_tokens": cfg.max_run_input_tokens,
        "max_run_output_tokens": cfg.max_run_output_tokens,
        "max_run_cost_usd": cfg.max_run_cost_usd,
        "max_run_tool_calls": cfg.max_run_tool_calls,
        "max_run_wall_seconds": cfg.max_run_wall_seconds,
        "agent_timeout": cfg.agent_timeout,
        "planning_effort": cfg.planning_effort,
        "implementation_effort": cfg.implementation_effort,
        "verification_effort": cfg.verification_effort,
        "claude_model": cfg.claude_model,
        "codex_model": cfg.codex_model,
        "claude_planning_model": cfg.claude_planning_model,
        "claude_implementation_model": cfg.claude_implementation_model,
        "claude_verification_model": cfg.claude_verification_model,
        "codex_planning_model": cfg.codex_planning_model,
        "codex_implementation_model": cfg.codex_implementation_model,
        "codex_verification_model": cfg.codex_verification_model,
        "disable_nested_agents": cfg.disable_nested_agents,
    }


def _policy_value(cfg, state, name: str):
    return state.run_policy.get(name, getattr(cfg, name))


def budget_limits(cfg, state) -> dict[str, int | float]:
    overrides = state.budget_overrides
    return {
        "invocations": int(
            _effective(
                _policy_value(cfg, state, "max_run_invocations"),
                overrides,
                "invocations",
            )
        ),
        "input_tokens": int(
            _effective(
                _policy_value(cfg, state, "max_run_input_tokens"),
                overrides,
                "input_tokens",
            )
        ),
        "output_tokens": int(
            _effective(
                _policy_value(cfg, state, "max_run_output_tokens"),
                overrides,
                "output_tokens",
            )
        ),
        "cost_usd": float(
            _effective(
                _policy_value(cfg, state, "max_run_cost_usd"),
                overrides,
                "cost_usd",
            )
        ),
        "tool_calls": int(
            _effective(
                _policy_value(cfg, state, "max_run_tool_calls"),
                overrides,
                "tool_calls",
            )
        ),
        "wall_seconds": int(
            _effective(
                _policy_value(cfg, state, "max_run_wall_seconds"),
                overrides,
                "wall_seconds",
            )
        ),
    }


def budget_usage(state) -> dict[str, int | float]:
    return {
        "invocations": state.provider_invocations,
        "input_tokens": total_reported_input(state.provider_usage),
        "output_tokens": total_reported_output(state.provider_usage),
        "cost_usd": state.provider_cost_usd,
        "tool_calls": state.provider_tool_calls,
        "wall_seconds": run_wall_seconds(state),
        "provider_seconds": state.provider_duration_s,
    }


def run_wall_seconds(state) -> float:
    if state.started_epoch_s:
        return max(0.0, time.time() - state.started_epoch_s)
    # Conservative migration for old states that have no wall-clock anchor.
    return max(0.0, state.provider_duration_s)


def budget_violations(cfg, state) -> list[str]:
    violations: list[str] = []
    currencies = sorted(
        key for key, value in state.provider_costs.items()
        if key != "USD"
        and key not in state.acknowledged_cost_currencies
        and float(value or 0) > 0
    )
    if currencies:
        violations.append(
            "reported cost currency cannot be reconciled with the USD budget: "
            + ", ".join(currencies)
        )
    invocation_output = total_reported_output(state.last_attempt_usage)
    invocation_output_limit = int(
        _policy_value(cfg, state, "max_invocation_output_tokens")
    ) + int(state.budget_overrides.get("invocation_output_tokens", 0) or 0)
    if invocation_output > invocation_output_limit:
        violations.append(
            "last invocation output_tokens exceeded "
            f"({invocation_output} / {invocation_output_limit})"
        )
    invocation_tool_limit = int(
        _policy_value(cfg, state, "max_invocation_tool_calls")
    ) + int(state.budget_overrides.get("invocation_tool_calls", 0) or 0)
    if state.last_attempt_tool_calls > invocation_tool_limit:
        violations.append(
            "last invocation tool_calls exceeded "
            f"({state.last_attempt_tool_calls} / {invocation_tool_limit})"
        )
    limits = budget_limits(cfg, state)
    used = budget_usage(state)
    violations.extend(
        f"{key} exhausted ({used[key]} / {limits[key]})"
        for key in limits
        if used[key] >= limits[key]
    )
    return violations


def profile_for_phase(phase_label: str) -> str:
    lowered = phase_label.lower()
    if any(token in lowered for token in ("implement", "fix", "report-draft")):
        return "implementation"
    if any(token in lowered for token in ("verify", "review", "critique", "checkpoint")):
        return "verification"
    return "planning"


def capability_for_phase(phase_label: str) -> str:
    lowered = phase_label.lower()
    if any(token in lowered for token in ("implement", "fix", "report-draft")):
        return "workspace_write"
    if any(token in lowered for token in ("critique", "review", "checkpoint", "verify", "revise")):
        return "evidence_read"
    return "repo_read"


def invocation_policy(cfg, state, phase_label: str, agent_name: str = "") -> InvocationPolicy:
    profile = profile_for_phase(phase_label)
    requested_effort = str(_policy_value(cfg, state, f"{profile}_effort"))
    # Claude calls its maximum setting `max`; Codex calls its maximum
    # supported reasoning effort `xhigh`. Preserve the requested profile and
    # emit the provider-native equivalent instead of sending an invalid flag.
    effort = (
        "xhigh"
        if agent_name == "codex" and requested_effort == "max"
        else requested_effort
    )
    phase_model = str(
        _policy_value(cfg, state, f"{agent_name}_{profile}_model")
    ) if agent_name else ""
    model = phase_model or (
        str(_policy_value(cfg, state, f"{agent_name}_model")) if agent_name else ""
    )
    limits = budget_limits(cfg, state)
    remaining_run_cost = max(0.0, limits["cost_usd"] - state.provider_cost_usd)
    native_cost_cap = min(
        float(_policy_value(cfg, state, "max_invocation_cost_usd")),
        remaining_run_cost,
    )
    remaining_seconds = max(
        1, int(limits["wall_seconds"] - run_wall_seconds(state))
    )
    return InvocationPolicy(
        profile=profile,
        model=model,
        requested_effort=requested_effort,
        effort=effort,
        max_budget_usd=round(native_cost_cap, 6),
        max_turns=int(_policy_value(cfg, state, "max_invocation_turns")),
        timeout_seconds=min(
            int(_policy_value(cfg, state, "agent_timeout")), remaining_seconds
        ),
        disable_nested_agents=bool(
            _policy_value(cfg, state, "disable_nested_agents")
        ),
        capability=capability_for_phase(phase_label),
    )


def standalone_policy(cfg, profile: str, agent_name: str) -> InvocationPolicy:
    """Apply per-call safety to provider work performed outside a run."""
    requested_effort = str(getattr(cfg, f"{profile}_effort"))
    effort = (
        "xhigh"
        if agent_name == "codex" and requested_effort == "max"
        else requested_effort
    )
    model = str(getattr(cfg, f"{agent_name}_{profile}_model")) or str(
        getattr(cfg, f"{agent_name}_model")
    )
    return InvocationPolicy(
        profile=profile,
        model=model,
        requested_effort=requested_effort,
        effort=effort,
        max_budget_usd=float(cfg.max_invocation_cost_usd),
        max_turns=cfg.max_invocation_turns,
        timeout_seconds=cfg.agent_timeout,
        disable_nested_agents=cfg.disable_nested_agents,
        capability="repo_read",
    )


def record_attempt_started(state, policy: InvocationPolicy) -> None:
    state.provider_invocations += 1
    state.last_invocation_policy = asdict(policy)


def record_result(state, result) -> bool:
    if result.attempt_id and result.attempt_id in state.accounted_attempt_ids:
        return False
    normalized = normalize_usage(result.usage)
    if not state.provider_usage:
        state.provider_usage = empty_usage()
    for key in USAGE_KEYS:
        state.provider_usage[key] = (
            int(state.provider_usage.get(key, 0)) + normalized[key]
        )
    if result.usage:
        state.known_usage_attempts += 1
    else:
        state.unknown_usage_attempts += 1
    state.provider_duration_s += max(0.0, float(result.duration_s or 0.0))
    state.provider_tool_calls += max(0, int(result.tool_calls or 0))
    if result.tool_calls_observed:
        state.known_tool_attempts += 1
    else:
        state.unknown_tool_attempts += 1
    reported_cost = getattr(result, "reported_cost", None)
    if reported_cost is None and result.cost_usd is not None:
        reported_cost = result.cost_usd
    if reported_cost is None:
        state.unknown_cost_attempts += 1
    else:
        currency = str(result.cost_currency or "USD").upper()
        amount = max(0.0, float(reported_cost))
        state.provider_costs[currency] = (
            float(state.provider_costs.get(currency, 0.0)) + amount
        )
        if currency == "USD":
            state.provider_cost_usd += amount
        else:
            state.unbudgeted_currency_attempts += 1
        state.known_cost_attempts += 1
    state.last_attempt_id = result.attempt_id
    state.last_attempt_usage = dict(normalized)
    state.last_attempt_tool_calls = max(0, int(result.tool_calls or 0))
    state.active_attempt_id = ""
    if result.attempt_id:
        state.accounted_attempt_ids.append(result.attempt_id)
    return True


def record_unreported_attempt(state, attempt_id: str = "") -> None:
    """Record a started call that never produced a provider terminal result."""
    state.unknown_cost_attempts += 1
    state.unknown_usage_attempts += 1
    state.unknown_tool_attempts += 1
    state.last_attempt_id = attempt_id or state.active_attempt_id
    state.active_attempt_id = ""


def add_overrides(state, additions: dict[str, int | float]) -> None:
    for key, value in additions.items():
        if value < 0:
            raise ValueError(f"budget override {key} must be non-negative")
    for key, value in additions.items():
        if value:
            state.budget_overrides[key] = state.budget_overrides.get(key, 0) + value


def remaining_budgets(cfg, state) -> dict[str, int | float]:
    limits = budget_limits(cfg, state)
    used = budget_usage(state)
    return {key: max(0, limits[key] - used[key]) for key in limits}
