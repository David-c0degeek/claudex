"""claudex — pair-programming orchestrator for Claude Code and OpenAI Codex.

An external coordinator makes the two agents work as one team: the LEAD
(chosen by the human at start) drafts the plan, implements, and fixes; the
PAIR critiques the plan, reviews every step's commits, and verifies with
fresh context. Convergence is agreement (AGREE + zero blocking/major
findings) under hard round caps — deadlocks gate to the human instead of
looping. The coordinator owns phase transitions, edit permissions, and the
mailbox transcript; neither model is ever "the boss" of the other.
"""

__version__ = "0.2.0"
