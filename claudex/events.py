"""Versioned provider-neutral events and append-only attempt journals.

Provider adapters retain their raw protocol at the edge.  The coordinator,
status command, and terminal watchers consume :class:`AgentEvent` instead.
Journals are JSON Lines so readers can replay a completed attempt and then tail
the same file while the process is still running.
"""

from __future__ import annotations

import json
import threading
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Callable, Iterable

from .security import redact_text, redact_value


EVENT_SCHEMA_VERSION = 1
TERMINAL_EVENT_KINDS = {"completed", "failed", "cancelled", "rate_limited"}


@dataclass
class AgentEvent:
    """One normalized, forward-compatible provider/process observation."""

    run_id: str
    attempt_id: str
    agent: str
    phase: str
    sequence: int
    timestamp: str
    elapsed_s: float
    kind: str
    summary: str = ""
    provider_type: str = ""
    raw_ref: str = ""
    usage: dict = field(default_factory=dict)
    cost: float | None = None
    currency: str = ""
    tool_name: str = ""
    tool_status: str = ""
    metadata: dict = field(default_factory=dict)
    schema_version: int = EVENT_SCHEMA_VERSION

    def to_dict(self) -> dict:
        return asdict(self)

    @classmethod
    def from_dict(cls, value: dict) -> "AgentEvent":
        """Load known fields and tolerate fields added by later versions."""
        known = cls.__dataclass_fields__
        data = {key: item for key, item in value.items() if key in known}
        return cls(**data)


class EventJournal:
    """Thread-safe append/replay helper for a single JSONL journal."""

    def __init__(self, path: Path):
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    def append(self, event: AgentEvent) -> None:
        encoded = json.dumps(event.to_dict(), ensure_ascii=False, separators=(",", ":"))
        with self._lock:
            with self.path.open("a", encoding="utf-8", newline="\n") as stream:
                stream.write(encoded + "\n")
                stream.flush()

    def read(self, *, after_sequence: int = -1) -> list[AgentEvent]:
        """Read valid complete lines; an in-flight partial final line is ignored."""
        if not self.path.exists():
            return []
        events: list[AgentEvent] = []
        with self.path.open("r", encoding="utf-8", errors="replace") as stream:
            for line in stream:
                if not line.endswith("\n"):
                    break
                try:
                    payload = json.loads(line)
                    event = AgentEvent.from_dict(payload)
                except (json.JSONDecodeError, TypeError, ValueError):
                    continue
                if event.sequence > after_sequence:
                    events.append(event)
        return events

    def follow(
        self,
        *,
        after_sequence: int = -1,
        poll_interval: float = 0.1,
        stop: Callable[[], bool] | None = None,
    ) -> Iterable[AgentEvent]:
        """Replay then poll.  The caller owns the stop policy."""
        cursor = after_sequence
        while True:
            batch = self.read(after_sequence=cursor)
            for event in batch:
                cursor = max(cursor, event.sequence)
                yield event
            if stop and stop():
                return
            time.sleep(poll_interval)


class EventEmitter:
    """Assign identity/order/timing, journal, then notify a live consumer."""

    def __init__(
        self,
        *,
        journal: EventJournal,
        run_id: str,
        attempt_id: str,
        agent: str,
        phase: str,
        started_monotonic: float,
        handler: Callable[[AgentEvent], None] | None = None,
    ):
        self.journal = journal
        self.run_id = run_id
        self.attempt_id = attempt_id
        self.agent = agent
        self.phase = phase
        self.started_monotonic = started_monotonic
        self.handler = handler
        self._sequence = 0
        self._lock = threading.Lock()
        self.write_errors: list[str] = []

    def emit(
        self,
        kind: str,
        summary: str = "",
        *,
        provider_type: str = "",
        raw_ref: str = "",
        usage: dict | None = None,
        cost: float | None = None,
        currency: str = "",
        tool_name: str = "",
        tool_status: str = "",
        metadata: dict | None = None,
    ) -> AgentEvent:
        with self._lock:
            sequence = self._sequence
            self._sequence += 1
            event = AgentEvent(
                run_id=self.run_id,
                attempt_id=self.attempt_id,
                agent=self.agent,
                phase=self.phase,
                sequence=sequence,
                timestamp=time.strftime("%Y-%m-%dT%H:%M:%S%z"),
                elapsed_s=round(time.monotonic() - self.started_monotonic, 6),
                kind=kind,
                summary=redact_text(summary),
                provider_type=provider_type,
                raw_ref=raw_ref,
                usage=redact_value(dict(usage or {})),
                cost=cost,
                currency=currency,
                tool_name=tool_name,
                tool_status=tool_status,
                metadata=redact_value(dict(metadata or {})),
            )
            # Keep file order equal to sequence order even when stdout and
            # stderr reader threads emit concurrently.
            try:
                self.journal.append(event)
            except OSError as exc:
                # A broken raw event sink must not strand a provider process
                # with unread pipes. Compact result/summary persistence still
                # determines whether the attempt can be recovered.
                self.write_errors.append(str(exc))
        if self.handler:
            try:
                self.handler(event)
            except Exception:
                # A display/watch consumer must never break provider execution.
                pass
        return event


def latest_run_events(run_dir: Path, *, agent: str = "all") -> list[AgentEvent]:
    """Replay all attempt journals in stable timestamp/attempt/sequence order."""
    events: list[AgentEvent] = []
    attempts = run_dir / "attempts"
    if not attempts.exists():
        return events
    for path in attempts.glob("*/events.jsonl"):
        for event in EventJournal(path).read():
            if agent == "all" or event.agent == agent:
                events.append(event)
    return sorted(events, key=lambda item: (item.timestamp, item.attempt_id, item.sequence))


def project_attempts(events: Iterable[AgentEvent]) -> list[dict]:
    """Build a deterministic status read-model without creating another store."""
    attempts: dict[str, dict] = {}
    for event in events:
        item = attempts.setdefault(
            event.attempt_id,
            {
                "attempt_id": event.attempt_id,
                "agent": event.agent,
                "phase": event.phase,
                "status": "running",
                "started_at": event.timestamp,
                "last_event_at": event.timestamp,
                "elapsed_s": 0.0,
                "tool_calls": 0,
                "usage": {},
                "cost": None,
                "currency": "",
                "last_summary": "",
            },
        )
        item["last_event_at"] = event.timestamp
        item["elapsed_s"] = max(float(item["elapsed_s"]), event.elapsed_s)
        if event.summary:
            item["last_summary"] = event.summary
        if event.kind == "tool_started":
            item["tool_calls"] += 1
        if event.usage:
            item["usage"].update(event.usage)
        if event.cost is not None:
            item["cost"] = event.cost
            item["currency"] = event.currency
        if event.kind in ("completed", "provider_completed"):
            item["status"] = "completed"
        elif event.kind in ("failed", "cancelled", "rate_limited"):
            item["status"] = event.kind
    return sorted(attempts.values(), key=lambda item: (item["started_at"], item["attempt_id"]))
