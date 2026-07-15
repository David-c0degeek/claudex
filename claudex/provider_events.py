"""Claude and Codex JSONL adapters for the provider-neutral event journal."""

from __future__ import annotations

import json

from .events import EventEmitter
from .limits import classify_limit


def _truncate(value: object, limit: int = 1000) -> str:
    text = str(value or "").replace("\r", "").strip()
    return text if len(text) <= limit else text[: limit - 1] + "…"


def _json_event(line: str) -> dict | None:
    try:
        value = json.loads(line)
    except json.JSONDecodeError:
        return None
    return value if isinstance(value, dict) else None


def decode_codex_event(line: str, line_no: int, emitter: EventEmitter) -> None:
    payload = _json_event(line)
    raw_ref = f"stdout.jsonl:{line_no}"
    if payload is None:
        if line.strip():
            emitter.emit("warning", "malformed Codex JSONL event", provider_type="malformed", raw_ref=raw_ref)
        return
    event_type = str(payload.get("type", "unknown"))
    if event_type == "thread.started":
        emitter.emit("provider_started", "Codex thread started", provider_type=event_type, raw_ref=raw_ref, metadata={"session_id": payload.get("thread_id", "")})
    elif event_type == "turn.started":
        emitter.emit("progress", "Codex turn started", provider_type=event_type, raw_ref=raw_ref)
    elif event_type in ("item.started", "item.updated", "item.completed"):
        item = payload.get("item") or {}
        item_type = str(item.get("type", "item"))
        status = str(item.get("status", ""))
        lifecycle = event_type.rsplit(".", 1)[-1]
        if item_type == "agent_message":
            emitter.emit("message", _truncate(item.get("text", "")), provider_type=event_type, raw_ref=raw_ref, metadata={"item_type": item_type})
        elif item_type == "reasoning":
            emitter.emit("progress", "Codex reasoning update", provider_type=event_type, raw_ref=raw_ref, metadata={"item_type": item_type, "status": status})
        else:
            tool_name = str(item.get("name") or item_type)
            command = item.get("command") or item.get("query") or ""
            summary = tool_name + (f": {_truncate(command, 500)}" if command else "")
            emitter.emit(
                "tool_started" if lifecycle == "started" else "tool_finished" if lifecycle == "completed" else "tool_progress",
                summary,
                provider_type=event_type,
                raw_ref=raw_ref,
                tool_name=tool_name,
                tool_status=status or lifecycle,
                metadata={"item_type": item_type},
            )
    elif event_type == "turn.completed":
        usage = payload.get("usage") if isinstance(payload.get("usage"), dict) else {}
        emitter.emit("usage", "Codex usage update", provider_type=event_type, raw_ref=raw_ref, usage=usage)
    elif event_type == "turn.failed":
        error = payload.get("error") or {}
        text = json.dumps(error, ensure_ascii=False) if isinstance(error, dict) else str(error)
        kind = "rate_limited" if classify_limit(text)[0] else "failed"
        emitter.emit(kind, _truncate(text, 1000), provider_type=event_type, raw_ref=raw_ref)
    elif event_type == "error":
        emitter.emit("warning", _truncate(payload.get("message") or payload, 1000), provider_type=event_type, raw_ref=raw_ref)
    else:
        emitter.emit("provider_event", event_type, provider_type=event_type, raw_ref=raw_ref)


def decode_claude_event(line: str, line_no: int, emitter: EventEmitter) -> None:
    payload = _json_event(line)
    raw_ref = f"stdout.jsonl:{line_no}"
    if payload is None:
        if line.strip():
            emitter.emit("warning", "malformed Claude JSONL event", provider_type="malformed", raw_ref=raw_ref)
        return
    event_type = str(payload.get("type", "unknown"))
    subtype = str(payload.get("subtype", ""))
    provider_type = f"{event_type}/{subtype}" if subtype else event_type
    if event_type == "system" and subtype == "init":
        emitter.emit(
            "provider_started",
            "Claude session started",
            provider_type=provider_type,
            raw_ref=raw_ref,
            metadata={
                "session_id": payload.get("session_id", ""),
                "model": payload.get("model", ""),
                "capabilities": payload.get("capabilities", []),
            },
        )
    elif event_type == "system" and subtype == "api_retry":
        error = str(payload.get("error", "unknown"))
        emitter.emit(
            "rate_limited" if error == "rate_limit" else "warning",
            f"Claude API retry {payload.get('attempt', '?')}/{payload.get('max_retries', '?')} in {payload.get('retry_delay_ms', '?')}ms ({error})",
            provider_type=provider_type,
            raw_ref=raw_ref,
            metadata={key: payload.get(key) for key in ("attempt", "max_retries", "retry_delay_ms", "error_status", "error")},
        )
    elif event_type == "stream_event":
        raw = payload.get("event") or {}
        raw_type = str(raw.get("type", "stream_event"))
        delta = raw.get("delta") or {}
        delta_type = str(delta.get("type", ""))
        if delta_type == "text_delta":
            emitter.emit("text_delta", str(delta.get("text", "")), provider_type=f"{provider_type}/{raw_type}/{delta_type}", raw_ref=raw_ref)
        elif raw_type == "message_start":
            message = raw.get("message") or {}
            usage = message.get("usage") if isinstance(message.get("usage"), dict) else {}
            emitter.emit("usage", "Claude message started", provider_type=f"{provider_type}/{raw_type}", raw_ref=raw_ref, usage=usage)
        elif raw_type == "message_delta":
            usage = raw.get("usage") if isinstance(raw.get("usage"), dict) else {}
            emitter.emit("usage", "Claude usage update", provider_type=f"{provider_type}/{raw_type}", raw_ref=raw_ref, usage=usage)
        else:
            emitter.emit("progress", raw_type, provider_type=f"{provider_type}/{raw_type}", raw_ref=raw_ref)
    elif event_type == "assistant":
        message = payload.get("message") or payload
        usage = message.get("usage") if isinstance(message.get("usage"), dict) else {}
        if usage:
            emitter.emit("usage", "Claude assistant usage", provider_type=provider_type, raw_ref=raw_ref, usage=usage)
        for block in message.get("content", []) if isinstance(message.get("content"), list) else []:
            block_type = str(block.get("type", ""))
            if block_type == "tool_use":
                emitter.emit("tool_started", str(block.get("name", "tool")), provider_type=provider_type, raw_ref=raw_ref, tool_name=str(block.get("name", "")), tool_status="started")
            elif block_type == "text" and block.get("text"):
                emitter.emit("message", _truncate(block.get("text"), 1000), provider_type=provider_type, raw_ref=raw_ref)
    elif event_type == "user":
        emitter.emit("tool_finished", "Claude tool result", provider_type=provider_type, raw_ref=raw_ref, tool_status="completed")
    elif event_type == "result":
        usage = payload.get("usage") if isinstance(payload.get("usage"), dict) else {}
        cost = payload.get("total_cost_usd")
        emitter.emit(
            "provider_completed" if not payload.get("is_error") else "failed",
            _truncate(payload.get("result") or subtype or "Claude result", 1000),
            provider_type=provider_type,
            raw_ref=raw_ref,
            usage=usage,
            cost=float(cost) if isinstance(cost, (int, float)) else None,
            currency="USD" if isinstance(cost, (int, float)) else "",
            metadata={"session_id": payload.get("session_id", ""), "num_turns": payload.get("num_turns")},
        )
    elif event_type in ("rate_limit_event", "rate_limit"):
        emitter.emit("rate_limited", _truncate(payload, 1000), provider_type=provider_type, raw_ref=raw_ref)
    else:
        emitter.emit("provider_event", provider_type, provider_type=provider_type, raw_ref=raw_ref)
