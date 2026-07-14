"""Run state: the persisted state machine that survives process restarts.

Every phase transition is written to ``.claudex/runs/<run_id>/state.json``
before the coordinator proceeds, so both drivers — headless ``claudex run``
and a live interactive lead using ``claudex pair`` subcommands — can always
resume from the last completed round. The one human gate (AWAIT_GUIDANCE)
is just a phase the driver refuses to advance past on its own.

A run-dir lockfile serializes state-mutating commands across processes:
the in-process ``threading.Lock`` cannot stop a live ``claudex pair`` call
from racing a headless ``claudex run`` on the same cap counters.
"""

from __future__ import annotations

import dataclasses
import json
import os
import secrets
import time
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path


class Phase(str, Enum):
    INIT = "init"
    PLAN_DRAFT = "plan_draft"
    PLAN_CRITIQUE = "plan_critique"
    PLAN_REVISE = "plan_revise"
    IMPLEMENT_STEP = "implement_step"
    CHECKPOINT = "checkpoint"
    FIX = "fix"
    TESTS = "tests"
    VERIFY = "verify"
    AWAIT_GUIDANCE = "await_guidance"
    DONE = "done"
    FAILED = "failed"
    ABORTED = "aborted"


TERMINAL_PHASES = {Phase.DONE, Phase.FAILED, Phase.ABORTED}
GATE_PHASES = {Phase.AWAIT_GUIDANCE}

AGENTS = ("claude", "codex")

# Session lineages. Codex `exec resume` pins the session's original cwd and
# Claude `--resume` is per-project-dir, so a session created in the main repo
# must never be resumed for worktree work (it would silently read stale
# code). Plan lineages live at cwd=repo; impl/review lineages at cwd=worktree;
# no lineage ever crosses. VERIFY deliberately uses no lineage at all.
SESSION_KEYS = ("lead_plan", "lead_impl", "pair_plan", "pair_review")


def other_agent(name: str) -> str:
    return "codex" if name == "claude" else "claude"


@dataclass
class RunState:
    run_id: str
    repo: str
    lead: str  # holds the pen: drafts the plan, implements, fixes
    # "headless": `claudex run` drives both agents as subprocesses.
    # "live": an interactive session IS the lead; `claudex pair` subcommands
    # run only the pair agent's turns. The two drivers must not share a run.
    driver: str = "headless"
    # "change": deliverable is a diff. "report": deliverable IS analysis;
    # the report draft is the single step and PLAN_* phases are skipped.
    mode: str = "change"
    phase: str = Phase.INIT.value
    created_at: str = ""
    base_commit: str = ""
    branch: str = ""
    worktree: str = ""
    # One session id per lineage (see SESSION_KEYS).
    sessions: dict = field(default_factory=dict)
    # Agreed plan's ordered steps: [{title, description, files, tests}, ...]
    steps: list = field(default_factory=list)
    step_index: int = 0
    # Round counters count rounds CONSUMED and only ever increase (artifact
    # names embed them, so a reset would silently replay stale artifacts
    # through the skip-if-present retry logic). checkpoint_round is the one
    # exception: it resets per step, which is safe because checkpoint
    # artifact names also embed the step index.
    plan_round: int = 0
    checkpoint_round: int = 0
    fix_round: int = 0
    test_round: int = 0
    verify_round: int = 0
    # Human guidance re-arms a cap by RAISING the ceiling instead of
    # resetting the counter: cap = config max + extra.
    plan_cap_extra: int = 0
    checkpoint_cap_extra: int = 0
    test_cap_extra: int = 0
    verify_cap_extra: int = 0
    # Last commit the pair has AGREEd to; checkpoint diffs are
    # last_reviewed_commit..HEAD.
    last_reviewed_commit: str = ""
    # Mailbox turn counter (mailbox.md is the append-only transcript).
    mailbox_turn: int = 0
    # AWAIT_GUIDANCE bookkeeping: why we gated, where resolve returns to,
    # and the human's answer (injected into the next prompt).
    gate_reason: str = ""
    return_phase: str = ""
    guidance_notes: str = ""
    findings_file: str = ""  # findings the next FIX must address
    # Where FIX hands control back to: checkpoint | tests | verify —
    # fixes re-enter the loop that produced the findings.
    fix_return: str = ""
    error: str = ""
    failed_phase: str = ""
    events: list = field(default_factory=list)

    # ------------------------------------------------------------------ props
    @property
    def pair(self) -> str:
        """The critique side: reviews plans, checkpoints, and verifies."""
        return other_agent(self.lead)

    def is_terminal(self) -> bool:
        return Phase(self.phase) in TERMINAL_PHASES

    def is_gate(self) -> bool:
        return Phase(self.phase) in GATE_PHASES

    # ------------------------------------------------------------------ moves
    def advance(self, new_phase: Phase, note: str = "") -> None:
        self.log(f"{self.phase} -> {new_phase.value}" + (f" ({note})" if note else ""))
        self.phase = new_phase.value

    def gate(self, reason: str, return_phase: Phase) -> None:
        """Cap hit: stop and surface the open dispute to the human."""
        self.gate_reason = reason
        self.return_phase = return_phase.value
        self.advance(Phase.AWAIT_GUIDANCE, reason[:200])

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
        # Pre-pair state files called the lead "owner"; accept them.
        if "lead" not in data and "owner" in data:
            data["lead"] = data["owner"]
        return RunState(**{k: v for k, v in data.items() if k in known})


# ------------------------------------------------------------------ run lock
class RunLockError(RuntimeError):
    pass


LOCK_NAME = "run.lock"


def acquire_run_lock(run_dir: Path) -> Path:
    """Cross-process mutex for state-mutating commands. O_CREAT|O_EXCL is
    atomic on every platform we care about; a lock whose pid is dead is
    stale and reclaimed."""
    run_dir.mkdir(parents=True, exist_ok=True)
    lock = run_dir / LOCK_NAME
    for _ in range(2):  # second pass after clearing a stale lock
        try:
            fd = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
            os.write(fd, str(os.getpid()).encode())
            os.close(fd)
            return lock
        except FileExistsError:
            try:
                pid = int(lock.read_text(encoding="utf-8").strip() or "0")
            except (OSError, ValueError):
                pid = 0
            if pid and _pid_alive(pid):
                raise RunLockError(
                    f"run is locked by pid {pid} ({lock}) — another claudex "
                    "command is mutating this run; wait for it or remove the "
                    "lock if that pid is not a claudex process"
                )
            lock.unlink(missing_ok=True)  # stale: owner died
    raise RunLockError(f"could not acquire {lock}")


def release_run_lock(run_dir: Path) -> None:
    (run_dir / LOCK_NAME).unlink(missing_ok=True)


def _pid_alive(pid: int) -> bool:
    if os.name == "nt":
        import subprocess

        out = subprocess.run(
            ["tasklist", "/FI", f"PID eq {pid}", "/NH", "/FO", "CSV"],
            capture_output=True,
            text=True,
        )
        return str(pid) in out.stdout
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


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


# -------------------------------------------------------------------- history
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
