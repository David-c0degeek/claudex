"""Run state: the persisted state machine that survives process restarts.

Every phase transition is written to ``.claudex/runs/<run_id>/state.json``
before the coordinator proceeds, so ``claudex run`` can always resume from
the last completed phase. Human gates (plan selection, final approval) are
just phases the driver refuses to advance past on its own.
"""

from __future__ import annotations

import dataclasses
import json
import secrets
import time
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path


class Phase(str, Enum):
    INIT = "init"
    INVESTIGATE = "investigate"
    DISAGREEMENT = "disagreement"
    AWAIT_PLAN_SELECTION = "await_plan_selection"
    PLAN_REVIEW = "plan_review"
    PLAN_FINALIZE = "plan_finalize"
    IMPLEMENT = "implement"
    REVIEW = "review"
    REMEDIATE = "remediate"
    VERIFY = "verify"
    AWAIT_FINAL_APPROVAL = "await_final_approval"
    DONE = "done"
    FAILED = "failed"
    ABORTED = "aborted"


TERMINAL_PHASES = {Phase.DONE, Phase.FAILED, Phase.ABORTED}
GATE_PHASES = {Phase.AWAIT_PLAN_SELECTION, Phase.AWAIT_FINAL_APPROVAL}

AGENTS = ("claude", "codex")


def other_agent(name: str) -> str:
    return "codex" if name == "claude" else "claude"


@dataclass
class RunState:
    run_id: str
    repo: str
    owner: str  # implementation owner: "claude" | "codex"
    # "change": deliverable is a diff (plan -> attack -> implement).
    # "report": deliverable IS analysis (parallel reviews -> consolidate).
    mode: str = "change"
    phase: str = Phase.INIT.value
    created_at: str = ""
    base_commit: str = ""
    branch: str = ""
    worktree: str = ""
    # Investigation session ids, for optional context-preserving resumes.
    sessions: dict = field(default_factory=dict)  # {"claude": id, "codex": id}
    # Owner's implementation session — remediation resumes it.
    impl_session: str = ""
    plan_author: str = ""  # whose analysis was selected as the plan
    selected_plan: str = ""  # "claude" | "codex"
    plan_selection_notes: str = ""
    review_round: int = 0
    verify_round: int = 0
    findings_file: str = ""  # findings the next REMEDIATE must address
    error: str = ""
    failed_phase: str = ""
    events: list = field(default_factory=list)

    # ------------------------------------------------------------------ props
    @property
    def reviewer(self) -> str:
        """The agent that reviews the owner's diffs and verifies the result."""
        return other_agent(self.owner)

    @property
    def plan_reviewer(self) -> str:
        """The agent that did NOT author the selected plan attacks it."""
        return other_agent(self.plan_author or self.owner)

    def is_terminal(self) -> bool:
        return Phase(self.phase) in TERMINAL_PHASES

    def is_gate(self) -> bool:
        return Phase(self.phase) in GATE_PHASES

    # ------------------------------------------------------------------ moves
    def advance(self, new_phase: Phase, note: str = "") -> None:
        self.log(f"{self.phase} -> {new_phase.value}" + (f" ({note})" if note else ""))
        self.phase = new_phase.value

    def fail(self, error: str) -> None:
        self.failed_phase = self.phase
        self.error = error
        self.advance(Phase.FAILED, error[:200])

    def retry(self) -> None:
        """Rewind FAILED back to the phase that failed, for a re-attempt."""
        if Phase(self.phase) is not Phase.FAILED or not self.failed_phase:
            raise ValueError("run is not in a retryable failed state")
        self.log(f"retry: failed -> {self.failed_phase}")
        self.phase = self.failed_phase
        self.error = ""
        self.failed_phase = ""

    def log(self, message: str) -> None:
        self.events.append(
            {"ts": time.strftime("%Y-%m-%dT%H:%M:%S"), "message": message}
        )

    # ------------------------------------------------------------ persistence
    def save(self, run_dir: Path) -> None:
        run_dir.mkdir(parents=True, exist_ok=True)
        tmp = run_dir / "state.json.tmp"
        tmp.write_text(
            json.dumps(dataclasses.asdict(self), indent=2), encoding="utf-8"
        )
        tmp.replace(run_dir / "state.json")

    @staticmethod
    def load(run_dir: Path) -> "RunState":
        data = json.loads((run_dir / "state.json").read_text(encoding="utf-8"))
        known = {f.name for f in dataclasses.fields(RunState)}
        return RunState(**{k: v for k, v in data.items() if k in known})


# --------------------------------------------------------------------- layout
def claudex_dir(repo: Path) -> Path:
    return repo / ".claudex"


def runs_root(repo: Path) -> Path:
    return claudex_dir(repo) / "runs"


def run_dir_for(repo: Path, run_id: str) -> Path:
    return runs_root(repo) / run_id


def task_file(repo: Path) -> Path:
    return claudex_dir(repo) / "task.md"


def current_run_pointer(repo: Path) -> Path:
    return claudex_dir(repo) / "current"


def new_run_id() -> str:
    return time.strftime("%Y%m%d-%H%M%S") + "-" + secrets.token_hex(3)


def set_current_run(repo: Path, run_id: str) -> None:
    p = current_run_pointer(repo)
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(run_id, encoding="utf-8")


def get_current_run(repo: Path) -> str | None:
    p = current_run_pointer(repo)
    if p.exists():
        rid = p.read_text(encoding="utf-8").strip()
        if rid and run_dir_for(repo, rid).joinpath("state.json").exists():
            return rid
    return None


def clear_current_run(repo: Path) -> None:
    p = current_run_pointer(repo)
    if p.exists():
        p.unlink()


# ------------------------------------------------------------ owner rotation
def history_file(repo: Path) -> Path:
    return claudex_dir(repo) / "history.json"


def read_history(repo: Path) -> list:
    p = history_file(repo)
    if p.exists():
        try:
            return json.loads(p.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError):
            return []
    return []


def append_history(repo: Path, entry: dict) -> None:
    hist = read_history(repo)
    hist.append(entry)
    history_file(repo).write_text(json.dumps(hist, indent=2), encoding="utf-8")


def next_owner_auto(repo: Path) -> str:
    """Alternate ownership per task so neither model becomes the permanent
    planner while the other rubber-stamps."""
    hist = read_history(repo)
    last = next(
        (e.get("owner") for e in reversed(hist) if e.get("owner") in AGENTS), None
    )
    return other_agent(last) if last else "claude"
