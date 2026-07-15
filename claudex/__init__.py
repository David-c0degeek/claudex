"""claudex — pair-programming orchestrator for Claude Code and OpenAI Codex.

An external coordinator makes the two agents work as one team: the LEAD
(chosen by the human at start) drafts the plan, implements, and fixes; the
PAIR critiques the plan, reviews every step's commits, and verifies with
fresh context. Convergence is agreement (AGREE + zero blocking/major
findings) under explicit lead-response budgets. Concrete decisions gate to
the human; quality-budget exhaustion is reported separately and can continue
without fabricated guidance. The coordinator owns phase transitions, edit
permissions, and the mailbox transcript; neither model is ever "the boss" of
the other.
"""

__version__ = "0.5.0"
