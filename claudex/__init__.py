"""claudex — deterministic co-engineering orchestrator for Claude Code and OpenAI Codex.

An external coordinator drives both agents through an explicit state machine:
independent investigation, disagreement analysis, adversarial plan review,
single-owner implementation in an isolated git worktree, commit-based review,
remediation, and fresh-context verification. Neither model is ever "the boss"
of the other; the coordinator owns phase transitions and edit permissions.
"""

__version__ = "0.1.0"
