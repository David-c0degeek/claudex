"""Stable terminal projections over durable state and normalized events."""

from __future__ import annotations

import json
import sys
import time
from pathlib import Path
from typing import Callable

from .events import AgentEvent, EventJournal
from .lifecycle import Lifecycle
from .security import redact_value
from .state import Phase, RunState


_COLORS = {
    "failed": "31",
    "cancelled": "31",
    "rate_limited": "33",
    "warning": "33",
    "stderr": "33",
    "completed": "32",
    "tool_started": "36",
    "tool_finished": "36",
    "text_delta": "37",
}


def color_enabled(mode: str, stream=None) -> bool:
    if mode == "always":
        return True
    if mode == "never":
        return False
    target = stream or sys.stdout
    return bool(getattr(target, "isatty", lambda: False)())


def render_event(event: AgentEvent, *, raw: bool = False, color: bool = False) -> str:
    if raw:
        return json.dumps(
            redact_value(event.to_dict()),
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        )
    label = f"[{event.agent.upper()}][{event.phase}][{event.kind.upper()}]"
    if color and event.kind in _COLORS:
        label = f"\x1b[{_COLORS[event.kind]}m{label}\x1b[0m"
    detail = event.summary
    if event.tool_name:
        detail += f" · tool={event.tool_name} status={event.tool_status or '?'}"
    if event.cost is not None:
        detail += f" · cost={event.cost:.4f} {event.currency or '?'}"
    return f"{event.timestamp} +{event.elapsed_s:8.3f}s {label} {detail}".rstrip()


def render_state(state: RunState, *, raw: bool = False, color: bool = False) -> str:
    """Render the durable run state that caused a watcher to stop or snapshot."""
    reason = terminal_reason(state)
    if raw:
        return json.dumps(
            redact_value(
                {
                    "record_type": "run_state",
                    "run_id": state.run_id,
                    "state_schema_version": state.state_schema_version,
                    "lifecycle": state.lifecycle,
                    "phase": state.phase,
                    "reason": reason,
                    "next_action": next_action(state),
                }
            ),
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        )
    label = "[CLAUDEX][STATE]"
    lifecycle = Lifecycle(state.lifecycle)
    if color and lifecycle.value in _COLORS:
        label = f"\x1b[{_COLORS[lifecycle.value]}m{label}\x1b[0m"
    detail = f"lifecycle={lifecycle.value} phase={state.phase}"
    if reason:
        detail += f" · reason={reason}"
    return f"{label} {detail} · next={next_action(state)}"


def watch_run(
    run_dir: Path,
    *,
    agent: str = "all",
    follow: bool = True,
    raw: bool = False,
    color: str = "auto",
    poll_interval: float = 0.1,
    output: Callable[[str], None] | None = None,
) -> int:
    """Replay every matching event, then tail new attempts until a stop state."""
    if not (run_dir / "state.json").exists():
        raise ValueError(f"run does not exist: {run_dir.name}")
    emit = output or (lambda value: print(value, flush=True))
    cursors: dict[str, int] = {}
    use_color = color_enabled(color)
    while True:
        emitted = False
        batch: list[AgentEvent] = []
        for path in (run_dir / "attempts").glob("*/events.jsonl"):
            key = str(path)
            events = EventJournal(path).read(after_sequence=cursors.get(key, -1))
            for event in events:
                cursors[key] = max(cursors.get(key, -1), event.sequence)
                if agent == "all" or event.agent == agent:
                    batch.append(event)
        for event in sorted(
            batch, key=lambda item: (item.timestamp, item.attempt_id, item.sequence)
        ):
            emit(render_event(event, raw=raw, color=use_color))
            emitted = True
        if not follow:
            break
        state = RunState.load(run_dir, persist_migration=False)
        if Lifecycle(state.lifecycle) is not Lifecycle.RUNNING and not emitted:
            break
        time.sleep(poll_interval)
    state = RunState.load(run_dir, persist_migration=False)
    emit(render_state(state, raw=raw, color=use_color))
    lifecycle = Lifecycle(state.lifecycle)
    if lifecycle is Lifecycle.CANCELLED:
        return 130
    if lifecycle is Lifecycle.RATE_LIMITED:
        return 75
    if lifecycle in (Lifecycle.FAILED_RETRYABLE, Lifecycle.FAILED_TERMINAL):
        return 1
    return 0


def next_action(state: RunState) -> str:
    lifecycle = Lifecycle(state.lifecycle)
    if lifecycle is Lifecycle.COMPLETED:
        return f"git merge {state.branch}" if state.branch else "inspect completed artifacts"
    if lifecycle is Lifecycle.CANCELLED:
        return "inspect retained artifacts; start a new run when ready"
    if lifecycle is Lifecycle.FAILED_RETRYABLE:
        return "claudex retry"
    if lifecycle is Lifecycle.FAILED_TERMINAL:
        return "export/inspect artifacts; start a new run"
    if lifecycle is Lifecycle.RATE_LIMITED:
        return "claudex resume after the reported reset"
    if lifecycle is Lifecycle.PAUSED_BUDGET:
        return "claudex resume --add-invocations N [other additions]"
    if lifecycle is Lifecycle.PAUSED:
        return (
            "claudex resolve --notes ..."
            if state.gate_kind == "decision"
            else "claudex continue"
        )
    phase = Phase(state.phase)
    if state.driver == "live":
        return {
            Phase.PLAN_DRAFT: "claudex pair plan --file PLAN.json",
            Phase.PLAN_REVISE: "revise the plan, then claudex pair plan --file PLAN.json",
            Phase.IMPLEMENT_STEP: "implement and commit the current step, then claudex pair checkpoint",
            Phase.FIX: "fix and commit the findings, then run the indicated pair gate",
            Phase.TESTS: "claudex pair verify",
            Phase.VERIFY: "claudex pair verify",
        }.get(phase, "claudex status")
    return "claudex run"


def terminal_reason(state: RunState) -> str:
    if state.error:
        return state.error
    if state.lifecycle_history:
        return str(state.lifecycle_history[-1].get("reason", ""))
    return ""
