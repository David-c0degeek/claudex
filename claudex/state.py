"""Run state: the persisted state machine that survives process restarts.

Every phase transition is written to ``.claudex/runs/<run_id>/state.json``
before the coordinator proceeds, so both drivers — headless ``claudex run``
and a live interactive lead using ``claudex pair`` subcommands — can always
resume from the last completed turn. AWAIT_GUIDANCE distinguishes concrete
human decisions from resumable quality-budget stops.

A run-dir lockfile serializes state-mutating commands across processes:
the in-process ``threading.Lock`` cannot stop a live ``claudex pair`` call
from racing a headless ``claudex run`` on the same cap counters.
"""

from __future__ import annotations

import dataclasses
import json
import os
import re
import secrets
import time
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path

from .lifecycle import Lifecycle, transition_allowed

STATE_SCHEMA_VERSION = 4


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
    PAUSED_BUDGET = "paused_budget"
    DONE = "done"
    FAILED = "failed"
    ABORTED = "aborted"


TERMINAL_PHASES = {Phase.DONE, Phase.FAILED, Phase.ABORTED}
GATE_PHASES = {Phase.AWAIT_GUIDANCE, Phase.PAUSED_BUDGET}

AGENTS = ("claude", "codex")

# Only implementation/review lineages may resume. Planning and final
# verification are fresh and consume coordinator-built bounded evidence.
# Codex `exec resume` pins the original cwd and Claude resume is per project,
# so even these worktree lineages never cross cwd boundaries.
SESSION_KEYS = ("lead_impl", "pair_review")

_MAILBOX_GUIDANCE_RE = re.compile(
    r"===== \[HUMAN\][^\r\n]*STATUS: GUIDANCE =====\r?\n"
    r"(.*?)\r?\n----- end \[HUMAN\]",
    re.DOTALL,
)


def other_agent(name: str) -> str:
    return "codex" if name == "claude" else "claude"


@dataclass
class RunState:
    run_id: str
    repo: str
    lead: str  # holds the pen: drafts the plan, implements, fixes
    predecessor_run_id: str = ""
    state_schema_version: int = STATE_SCHEMA_VERSION
    # "headless": `claudex run` drives both agents as subprocesses.
    # "live": an interactive session IS the lead; `claudex pair` subcommands
    # run only the pair agent's turns. The two drivers must not share a run.
    driver: str = "headless"
    # v2 (Claudex 0.4+) separates compact planning from implementation
    # checks. Legacy exhaustive-plan runs must restart while still planning.
    plan_protocol_version: int = 2
    # "change": deliverable is a diff. "report": deliverable IS analysis;
    # the report draft is the single step and PLAN_* phases are skipped.
    mode: str = "change"
    phase: str = Phase.INIT.value
    created_at: str = ""
    started_epoch_s: float = 0.0
    lifecycle: str = Lifecycle.RUNNING.value
    lifecycle_history: list = field(default_factory=list)
    base_commit: str = ""
    branch: str = ""
    worktree: str = ""
    # One session id per lineage (see SESSION_KEYS).
    sessions: dict = field(default_factory=dict)
    # Agreed plan's ordered steps: [{title, description, files, tests}, ...]
    steps: list = field(default_factory=list)
    canonical_plan_round: int = -1
    canonical_plan_sha256: str = ""
    step_index: int = 0
    # Round counters identify critique/review artifacts and only ever
    # increase (artifact names embed them, so a reset would silently replay
    # stale artifacts through the skip-if-present retry logic).
    plan_round: int = 0
    checkpoint_round: int = 0
    fix_round: int = 0
    test_round: int = 0
    verify_round: int = 0
    # Budgets count lead responses, not reviewer findings. A configured cap
    # of N therefore permits N complete critique -> revise/fix cycles and one
    # final review of the Nth response. This prevents a cap from firing after
    # a critique but before the lead is allowed to answer it.
    plan_revisions: int = 0
    checkpoint_fixes_used: int = 0
    test_fixes_used: int = 0
    verify_fixes_used: int = 0
    # `continue` or guidance at a budget gate raises the response ceiling
    # instead of resetting counters: budget = config max + extra.
    plan_cap_extra: int = 0
    checkpoint_cap_extra: int = 0
    test_cap_extra: int = 0
    verify_cap_extra: int = 0
    # Provider-terminal accounting. Intermediate streamed usage is deliberately
    # excluded because provider event values may be cumulative. Missing cost is
    # counted as unknown and is never replaced with a local price estimate.
    provider_usage: dict = field(default_factory=dict)
    provider_cost_usd: float = 0.0
    provider_costs: dict = field(default_factory=dict)
    provider_invocations: int = 0
    provider_duration_s: float = 0.0
    provider_tool_calls: int = 0
    known_cost_attempts: int = 0
    unknown_cost_attempts: int = 0
    known_usage_attempts: int = 0
    unknown_usage_attempts: int = 0
    known_tool_attempts: int = 0
    unknown_tool_attempts: int = 0
    unbudgeted_currency_attempts: int = 0
    usage_source: str = "provider_terminal"
    cost_currency: str = "USD"
    budget_overrides: dict = field(default_factory=dict)
    run_policy: dict = field(default_factory=dict)
    acknowledged_cost_currencies: list = field(default_factory=list)
    last_invocation_policy: dict = field(default_factory=dict)
    active_attempt_id: str = ""
    last_attempt_id: str = ""
    last_attempt_usage: dict = field(default_factory=dict)
    last_attempt_tool_calls: int = 0
    accounted_attempt_ids: list = field(default_factory=list)
    # Last commit the pair has AGREEd to; checkpoint diffs are
    # last_reviewed_commit..HEAD.
    last_reviewed_commit: str = ""
    # Mailbox turn counter (mailbox.md is the append-only transcript).
    mailbox_turn: int = 0
    # AWAIT_GUIDANCE bookkeeping. gate_kind distinguishes a real decision
    # from a quality-budget stop, so the latter can be resumed without
    # inventing binding guidance.
    gate_reason: str = ""
    gate_kind: str = ""  # decision | budget
    return_phase: str = ""
    # Deprecated one-shot field retained for loading v0.2 state files.
    guidance_notes: str = ""
    # Human decisions remain binding for the rest of the run and are sent to
    # both roles on every subsequent agent turn.
    binding_guidance: list = field(default_factory=list)
    # Content facts and edge cases discovered during plan review that fit an
    # existing implementation step. They do not force plan rewrites; the
    # coordinator carries them into implementation, checkpoints, and verify.
    implementation_checks: list = field(default_factory=list)
    accepted_findings: list = field(default_factory=list)
    unresolved_findings: list = field(default_factory=list)
    evidence_requests: list = field(default_factory=list)
    evidence_expansions: int = 0
    last_evidence_manifest: str = ""
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

    def record_lifecycle_start(self, reason: str = "run created") -> None:
        if self.lifecycle_history:
            raise ValueError("lifecycle start is already recorded")
        self._append_lifecycle(
            "",
            Lifecycle(self.lifecycle),
            reason,
            "claudex resume",
        )

    def transition_lifecycle(
        self,
        target: Lifecycle,
        reason: str,
        resume_instruction: str,
    ) -> None:
        previous = Lifecycle(self.lifecycle)
        if not transition_allowed(previous, target):
            raise ValueError(
                f"illegal lifecycle transition {previous.value} -> {target.value}"
            )
        self.lifecycle = target.value
        self._append_lifecycle(
            previous.value,
            target,
            reason,
            resume_instruction,
        )

    def _append_lifecycle(
        self,
        previous: str,
        target: Lifecycle,
        reason: str,
        resume_instruction: str,
    ) -> None:
        self.lifecycle_history.append(
            {
                "version": 1,
                "ts": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
                "from": previous,
                "to": target.value,
                "reason": reason,
                "phase": self.phase,
                "attempt_id": self.active_attempt_id,
                "resume_instruction": resume_instruction,
            }
        )

    def gate(
        self, reason: str, return_phase: Phase, *, kind: str = "decision"
    ) -> None:
        """Stop for either a real decision or an exhausted quality budget."""
        if kind not in ("decision", "budget"):
            raise ValueError(f"unknown gate kind: {kind}")
        self.gate_reason = reason
        self.gate_kind = kind
        self.return_phase = return_phase.value
        self.transition_lifecycle(
            Lifecycle.PAUSED,
            reason,
            "claudex resolve --notes ..." if kind == "decision" else "claudex continue",
        )
        self.advance(Phase.AWAIT_GUIDANCE, reason[:200])

    def pause_budget(self, reason: str, return_phase: Phase) -> None:
        """Persist a run-budget stop without mislabelling it as guidance."""
        self.gate_reason = reason
        self.gate_kind = "run_budget"
        self.return_phase = return_phase.value
        self.transition_lifecycle(
            Lifecycle.PAUSED_BUDGET,
            reason,
            "claudex resume --add-...",
        )
        self.advance(Phase.PAUSED_BUDGET, reason[:200])

    def fail(self, error: str, *, retryable: bool = True) -> None:
        self.failed_phase = self.phase
        self.error = error
        self.transition_lifecycle(
            Lifecycle.FAILED_RETRYABLE if retryable else Lifecycle.FAILED_TERMINAL,
            error,
            "claudex retry" if retryable else "inspect artifacts; start a new run",
        )
        self.advance(Phase.FAILED, error[:200])

    def retry(self) -> None:
        """Rewind FAILED back to the phase that failed, for a re-attempt."""
        if Phase(self.phase) is not Phase.FAILED or not self.failed_phase:
            raise ValueError("run is not in a retryable failed state")
        if Lifecycle(self.lifecycle) is Lifecycle.RUNNING:
            self.transition_lifecycle(
                Lifecycle.FAILED_RETRYABLE,
                "reconciled legacy retryable failure",
                "claudex retry",
            )
        self.log(f"retry: failed -> {self.failed_phase}")
        self.phase = self.failed_phase
        self.error = ""
        self.failed_phase = ""
        self.transition_lifecycle(
            Lifecycle.RUNNING,
            "retry requested",
            "claudex resume",
        )

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
        state_path = run_dir / "state.json"
        original = state_path.read_text(encoding="utf-8")
        data = json.loads(original)
        version = int(data.get("state_schema_version", 1))
        if version > STATE_SCHEMA_VERSION:
            raise ValueError(
                f"run state schema {version} is newer than supported "
                f"schema {STATE_SCHEMA_VERSION}; upgrade Claudex before resuming"
            )
        data["state_schema_version"] = STATE_SCHEMA_VERSION
        if "lifecycle" not in data:
            phase = data.get("phase", Phase.INIT.value)
            data["lifecycle"] = {
                Phase.DONE.value: Lifecycle.COMPLETED.value,
                Phase.ABORTED.value: Lifecycle.CANCELLED.value,
                Phase.FAILED.value: Lifecycle.FAILED_RETRYABLE.value,
                Phase.AWAIT_GUIDANCE.value: Lifecycle.PAUSED.value,
                Phase.PAUSED_BUDGET.value: Lifecycle.PAUSED_BUDGET.value,
            }.get(phase, Lifecycle.RUNNING.value)
        known = {f.name for f in dataclasses.fields(RunState)}
        # Pre-pair state files called the lead "owner"; accept them.
        if "lead" not in data and "owner" in data:
            data["lead"] = data["owner"]
        if "plan_protocol_version" not in data:
            data["plan_protocol_version"] = 1
        # Migrate one-shot v0.2 guidance into the persistent ledger.
        guidance = str(data.get("guidance_notes", "")).strip()
        binding = list(data.get("binding_guidance", []))
        if guidance and guidance not in binding:
            binding.append(guidance)
        # v0.2 cleared guidance after one prompt. Recover the durable human
        # record from the append-only mailbox when upgrading an in-flight run.
        mailbox = run_dir / "mailbox.md"
        if mailbox.exists():
            for recorded in _MAILBOX_GUIDANCE_RE.findall(
                mailbox.read_text(encoding="utf-8")
            ):
                recorded = recorded.strip()
                if recorded and recorded not in binding:
                    binding.append(recorded)
        data["binding_guidance"] = binding
        data["guidance_notes"] = ""
        # Old state did not count completed plan revisions explicitly. Plan
        # artifact indices are authoritative and survive crashes.
        if "plan_revisions" not in data:
            revisions = []
            for path in run_dir.glob("plan-round-*.json"):
                try:
                    revisions.append(int(path.stem.rsplit("-", 1)[1]))
                except (IndexError, ValueError):
                    continue
            data["plan_revisions"] = max(revisions, default=0)
        if not data.get("canonical_plan_sha256"):
            from .planops import plan_digest  # local import keeps state lightweight

            candidates = []
            for path in run_dir.glob("plan-round-*.json"):
                try:
                    candidates.append((int(path.stem.rsplit("-", 1)[1]), path))
                except (IndexError, ValueError):
                    continue
            for round_no, path in sorted(candidates, reverse=True):
                try:
                    plan = json.loads(path.read_text(encoding="utf-8"))
                    if not str(plan.get("plan_markdown", "")).strip() or not plan.get("steps"):
                        continue
                except (OSError, json.JSONDecodeError, AttributeError):
                    continue
                data["canonical_plan_round"] = round_no
                data["canonical_plan_sha256"] = plan_digest(plan)
                break
        if data.get("gate_reason") and not data.get("gate_kind"):
            data["gate_kind"] = "budget"
        if data.get("provider_cost_usd") and not data.get("provider_costs"):
            data["provider_costs"] = {"USD": data["provider_cost_usd"]}
        state = RunState(**{k: v for k, v in data.items() if k in known})
        synthesized_history = not state.lifecycle_history
        if synthesized_history:
            state._append_lifecycle(
                "",
                Lifecycle(state.lifecycle),
                f"migrated from run state schema {version}",
                "claudex resume",
            )
        if version < STATE_SCHEMA_VERSION:
            backup = run_dir / f"state.v{version}.bak.json"
            if not backup.exists():
                backup_tmp = backup.with_suffix(backup.suffix + ".tmp")
                backup_tmp.write_text(original, encoding="utf-8")
                backup_tmp.replace(backup)
        if version < STATE_SCHEMA_VERSION or synthesized_history:
            state.save(run_dir)
        return state


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
