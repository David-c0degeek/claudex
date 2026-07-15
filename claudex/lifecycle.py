"""Durable macro lifecycle independent from the orchestration phase."""

from __future__ import annotations

from enum import Enum


class Lifecycle(str, Enum):
    RUNNING = "running"
    PAUSED = "paused"
    PAUSED_BUDGET = "paused_budget"
    RATE_LIMITED = "rate_limited"
    CANCELLED = "cancelled"
    FAILED_RETRYABLE = "failed_retryable"
    FAILED_TERMINAL = "failed_terminal"
    COMPLETED = "completed"


LEGAL_TRANSITIONS = {
    Lifecycle.RUNNING: {
        Lifecycle.PAUSED,
        Lifecycle.PAUSED_BUDGET,
        Lifecycle.RATE_LIMITED,
        Lifecycle.CANCELLED,
        Lifecycle.FAILED_RETRYABLE,
        Lifecycle.FAILED_TERMINAL,
        Lifecycle.COMPLETED,
    },
    Lifecycle.PAUSED: {
        Lifecycle.RUNNING,
        Lifecycle.CANCELLED,
        Lifecycle.FAILED_TERMINAL,
    },
    Lifecycle.PAUSED_BUDGET: {
        Lifecycle.RUNNING,
        Lifecycle.CANCELLED,
        Lifecycle.FAILED_TERMINAL,
    },
    Lifecycle.RATE_LIMITED: {
        Lifecycle.RUNNING,
        Lifecycle.CANCELLED,
        Lifecycle.FAILED_RETRYABLE,
        Lifecycle.FAILED_TERMINAL,
    },
    Lifecycle.FAILED_RETRYABLE: {
        Lifecycle.RUNNING,
        Lifecycle.CANCELLED,
        Lifecycle.FAILED_TERMINAL,
    },
    Lifecycle.FAILED_TERMINAL: set(),
    Lifecycle.CANCELLED: set(),
    Lifecycle.COMPLETED: set(),
}


def transition_allowed(previous: Lifecycle, target: Lifecycle) -> bool:
    return target in LEGAL_TRANSITIONS[previous]
