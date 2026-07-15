"""Provider usage/rate-limit classification without provider dependencies."""

from __future__ import annotations

import re
import time


_LIMIT_RE = re.compile(
    r"(?:usage|rate|session|daily|weekly|monthly|\d+-hour)[ _-]limit"
    r"|limit\s+(?:reached|hit|exceeded)"
    r"|too\s+many\s+requests"
    r"|\b429\b",
    re.IGNORECASE,
)
_EPOCH_RE = re.compile(r"\|(\d{10,13})\b")
_TRY_AGAIN_RE = re.compile(
    r"try again (?:in|after)\s+((?:\d+\s*(?:h(?:ours?)?|m(?:in(?:utes?)?)?|s(?:ec(?:onds?)?)?)\s*)+)",
    re.IGNORECASE,
)
_DUR_PART_RE = re.compile(r"(\d+)\s*(h|m|s)", re.IGNORECASE)
_CLOCK_RE = re.compile(
    r"(?:reset\w*|available)\s*(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?",
    re.IGNORECASE,
)
_TZ_RE = re.compile(r"\(([A-Za-z]+(?:_[A-Za-z]+)*/[A-Za-z_+\-]+)\)")


def classify_limit(text: str, now: float | None = None) -> tuple[bool, int | None]:
    """Return whether text describes a limit and its reported reset delay."""
    if not _LIMIT_RE.search(text):
        return False, None
    now = now if now is not None else time.time()

    match = _EPOCH_RE.search(text)
    if match:
        epoch = int(match.group(1))
        if epoch > 1e12:
            epoch //= 1000
        return True, max(0, int(epoch - now))

    match = _TRY_AGAIN_RE.search(text)
    if match:
        seconds = 0
        for value, unit in _DUR_PART_RE.findall(match.group(1)):
            seconds += int(value) * {"h": 3600, "m": 60, "s": 1}[unit.lower()]
        if seconds:
            return True, seconds

    match = _CLOCK_RE.search(text)
    if match:
        hour = int(match.group(1))
        minute = int(match.group(2) or 0)
        meridiem = (match.group(3) or "").lower()
        if meridiem == "pm" and hour != 12:
            hour += 12
        elif meridiem == "am" and hour == 12:
            hour = 0
        if hour < 24 and minute < 60:
            delay = _delay_until_clock(text, now, hour, minute)
            if delay is not None:
                return True, delay
    return True, None


def _delay_until_clock(text: str, now: float, hour: int, minute: int) -> int | None:
    from datetime import datetime, timedelta

    tzinfo = None
    match = _TZ_RE.search(text)
    if match:
        try:
            from zoneinfo import ZoneInfo

            tzinfo = ZoneInfo(match.group(1))
        except Exception:
            tzinfo = None
    try:
        now_dt = datetime.fromtimestamp(now, tz=tzinfo).astimezone(tzinfo)
        target = now_dt.replace(hour=hour, minute=minute, second=0, microsecond=0)
        if target <= now_dt:
            target += timedelta(days=1)
        return int(target.timestamp() - now)
    except (ValueError, OverflowError, OSError):
        return None
