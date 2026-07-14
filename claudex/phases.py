"""The orchestrator: deterministic phase driver for pair programming.

Pipeline (one plan, one implementation, two agents converging on both):

    INIT               report mode -> synthesize the single report step,
                       skip the plan phases entirely
      -> PLAN_DRAFT    lead drafts the plan, grounded in the repo, read-only
      -> PLAN_CRITIQUE pair critiques it against the repo
      -> PLAN_REVISE   lead accepts or rebuts each finding, re-emits the plan
           (critique/revise loops until the pair AGREEs with zero
            blocking/major findings, or the round cap gates the run)
      -> IMPLEMENT_STEP lead implements exactly one plan step, commits
      -> CHECKPOINT    pair reviews that step's exact diff
      -> FIX           lead fixes blocking/major findings, commits  (loops)
      -> TESTS         coordinator runs the configured test command itself
      -> VERIFY        pair with FRESH context checks acceptance criteria
      -> DONE          automatically on verify pass
    AWAIT_GUIDANCE     any round cap hit -> the open dispute goes to the
                       human; `claudex resolve` feeds the answer back

The coordinator — not either model — owns phase transitions, edit
permissions, artifact routing, round caps, and the mailbox transcript.

Two drivers share every pair-turn method: headless `claudex run` calls them
from run_until_gate(); the live `claudex pair` subcommands (interactive
session as lead) call them directly. One code path, one cap check, one
mailbox append. One round = one handler iteration, so the save-per-iteration
persistence gives round-granular crash durability. Round counters only ever
increase (artifact names embed them); human guidance re-arms a cap by
raising its ceiling, never by resetting a counter.
"""

from __future__ import annotations

import json
import re
import shutil
import subprocess
import threading
import time
from pathlib import Path

from . import artifacts, gitops, prompts, schemas
from .agents import (
    AgentError,
    ClaudeAgent,
    CodexAgent,
    classify_limit,
    resolve_claude_bin,
    resolve_codex_bin,
)
from .config import Config
from .state import (
    Phase,
    RunState,
    append_history,
    run_dir_for,
)


class OrchestratorError(RuntimeError):
    pass


_HTML_COMMENT_RE = re.compile(r"<!--.*?-->", re.DOTALL)


def task_has_content(task_md: str) -> bool:
    """True if the contract contains substantive text beyond the scaffold —
    an unfilled template (headings + HTML comments only) must not reach the
    agents: they would either report an empty contract or invent a task."""
    stripped = _HTML_COMMENT_RE.sub("", task_md)
    substantive = [
        line.strip()
        for line in stripped.splitlines()
        if line.strip() and not line.strip().startswith("#")
    ]
    return len(" ".join(substantive)) >= 20


def detect_mode(task_md: str, configured: str = "auto") -> str:
    """"report": the deliverable IS analysis — the report draft is the single
    step and no plan-about-a-plan layer exists. "change": the deliverable is
    a diff. Detected from the goal's [REPORT]/[CHANGE]/[MIXED] prefix unless
    configured."""
    if configured in ("report", "change"):
        return configured
    m = re.search(r"^#\s*Goal\s*\n+(.{0,120})", task_md, re.MULTILINE | re.DOTALL)
    goal_head = m.group(1) if m else ""
    return "report" if "[REPORT]" in goal_head.upper() else "change"


def build_agents(cfg: Config) -> dict:
    return {
        "claude": ClaudeAgent(
            binary=resolve_claude_bin(cfg.claude_bin or None),
            model=cfg.claude_model,
            write_allowed_tools=cfg.claude_write_allowed_tools,
            extra_args=list(cfg.claude_extra_args),
        ),
        "codex": CodexAgent(
            binary=resolve_codex_bin(cfg.codex_bin or None),
            model=cfg.codex_model,
            extra_args=list(cfg.codex_extra_args),
        ),
    }


def draft_task(cfg: Config, description: str, agent_name: str = "claude") -> Path:
    """Agent-assisted task authoring: expand a one-paragraph human description
    into a full contract, grounded in the repository. The human reviews and
    edits the result before `claudex run` — this drafts, it does not decide."""
    from .state import claudex_dir, task_file

    agent = build_agents(cfg)[agent_name]
    log_root = claudex_dir(cfg.repo)
    print(f"[claudex] {agent_name}: drafting task contract (read-only) ...", flush=True)
    result = agent.run(
        prompts.task_contract(description),
        cwd=cfg.repo,
        run_dir=log_root,
        label="task-draft",
        read_only=True,
        schema=schemas.TASK_CONTRACT_SCHEMA,
        timeout=cfg.agent_timeout,
    )
    if not result.ok:
        raise OrchestratorError(f"{agent_name}/task-draft: {result.error}")
    contract = result.require_structured()
    target = task_file(cfg.repo)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(artifacts.render_task_contract(contract), encoding="utf-8")
    return target


def validate_plan_shape(plan: dict) -> None:
    """Live mode accepts a lead-authored plan file; check the minimal shape
    before it enters the run (full schema enforcement only exists for
    agent-emitted output)."""
    if not isinstance(plan, dict):
        raise OrchestratorError("plan file must contain a JSON object")
    if not str(plan.get("plan_markdown", "")).strip():
        raise OrchestratorError("plan.plan_markdown is missing or empty")
    steps = plan.get("steps")
    if not isinstance(steps, list) or not steps:
        raise OrchestratorError("plan.steps must be a non-empty array")
    for i, s in enumerate(steps):
        if not isinstance(s, dict) or not str(s.get("title", "")).strip():
            raise OrchestratorError(f"plan.steps[{i}] needs at least a title")


class Orchestrator:
    def __init__(self, cfg: Config, state: RunState):
        self.cfg = cfg
        self.state = state
        self.run_dir = run_dir_for(cfg.repo, state.run_id)
        self.agents = build_agents(cfg)
        self._save_lock = threading.Lock()

    # ------------------------------------------------------------- utilities
    def _save(self) -> None:
        with self._save_lock:
            self.state.save(self.run_dir)

    def _art(self, name: str) -> Path:
        return self.run_dir / name

    def say(self, msg: str) -> None:
        print(f"[claudex] {msg}", flush=True)

    @property
    def task_snapshot(self) -> Path:
        return self._art("task.md")

    @property
    def report_mode(self) -> bool:
        return self.state.mode == "report"

    def _agreed_plan(self) -> Path | None:
        p = self._art("agreed-plan.md")
        return p if p.exists() else None

    def _post(self, role: str, stage: str, status: str, body: str) -> None:
        """Append one turn to the mailbox transcript. Coordinator-written:
        the pair is sandboxed read-only during critiques, and its output
        reaches us as schema-validated JSON anyway."""
        self.state.mailbox_turn += 1
        artifacts.mailbox_append(
            self.run_dir, role, self.state.mailbox_turn, stage, status, body
        )

    def _take_guidance(self) -> str:
        """Consume pending human guidance: inject once, then clear. In
        headless runs the lead phase after `resolve` consumes it; in live
        runs the next pair turn does (the interactive lead saw it at
        `resolve` time)."""
        notes = self.state.guidance_notes
        self.state.guidance_notes = ""
        return notes

    def _run_agent(self, agent_name: str, prompt: str, **kw):
        agent = self.agents[agent_name]
        label = kw.pop("label")
        self.say(
            f"{agent_name}: {label} "
            f"({'read-only' if kw.get('read_only', True) else 'WRITE'}, "
            f"cwd={kw.get('cwd')}) ..."
        )
        waits = 0
        while True:
            result = agent.run(
                prompt,
                run_dir=self.run_dir,
                label=label,
                timeout=self.cfg.agent_timeout,
                **kw,
            )
            self.say(
                f"{agent_name}: {label} finished in {result.duration_s:.0f}s "
                f"(ok={result.ok})"
            )
            if result.ok:
                return result
            is_limit, delay = classify_limit(f"{result.error}\n{result.text}")
            if not (is_limit and self.cfg.wait_on_limits and waits < self.cfg.max_limit_waits):
                raise AgentError(f"{agent_name}/{label}: {result.error}")
            delay = min(
                self.cfg.max_limit_wait,
                max(60, delay if delay is not None else self.cfg.default_limit_wait),
            )
            resume_at = time.strftime("%H:%M:%S", time.localtime(time.time() + delay))
            self.say(
                f"{agent_name}: usage limit hit; waiting {delay // 60} min "
                f"(retry ~{resume_at}, wait {waits + 1}/{self.cfg.max_limit_waits})"
            )
            self.state.log(f"{agent_name}/{label}: usage limit, waiting {delay}s")
            self._save()  # durable: Ctrl+C here loses nothing, resume continues
            time.sleep(delay)
            waits += 1

    def _remember_session(self, lineage: str, session_id: str) -> None:
        """Resumed Claude runs return a NEW session id every time; a missed
        update orphans the lineage. Always re-capture."""
        if session_id:
            self.state.sessions[lineage] = session_id

    # ----------------------------------------------------------------- driver
    def run_until_gate(self) -> None:
        handlers = {
            Phase.INIT: self.phase_init,
            Phase.PLAN_DRAFT: self.phase_plan_draft,
            Phase.PLAN_CRITIQUE: self.phase_plan_critique,
            Phase.PLAN_REVISE: self.phase_plan_revise,
            Phase.IMPLEMENT_STEP: self.phase_implement_step,
            Phase.CHECKPOINT: self.phase_checkpoint,
            Phase.FIX: self.phase_fix,
            Phase.TESTS: self.phase_tests,
            Phase.VERIFY: self.phase_verify,
        }
        while True:
            phase = Phase(self.state.phase)
            if self.state.is_terminal():
                self._report_terminal()
                return
            if self.state.is_gate():
                self._report_gate()
                return
            handler = handlers[phase]
            try:
                handler()
            except (AgentError, gitops.GitError, OrchestratorError) as exc:
                self.state.fail(str(exc))
                self._save()
                self.say(f"FAILED in {phase.value}: {exc}")
                self.say("Fix the cause, then `claudex retry` to re-attempt the phase.")
                return
            self._save()

    def _report_gate(self) -> None:
        self.say("GATE: the pair hit a round cap — human guidance required.")
        self.say(f"  Why: {self.state.gate_reason}")
        self.say(f"  Transcript: {artifacts.mailbox_path(self.run_dir)}")
        self.say("  Then: claudex resolve --notes 'your decision'")

    def _report_terminal(self) -> None:
        phase = Phase(self.state.phase)
        if phase is Phase.DONE:
            self.say(f"DONE. Merge with: git merge {self.state.branch}")
        elif phase is Phase.FAILED:
            self.say(f"FAILED: {self.state.error}")
        else:
            self.say("Run aborted.")

    # ----------------------------------------------------------------- phases
    def phase_init(self) -> None:
        from .state import task_file

        task = task_file(self.cfg.repo)
        if not task.exists():
            raise OrchestratorError(
                f"task contract missing: {task} — run `claudex init` first"
            )
        task_text = task.read_text(encoding="utf-8")
        if not task_has_content(task_text):
            raise OrchestratorError(
                f"task contract is an unfilled template: {task} — fill it in, "
                'or draft it from a description with `claudex task "..."`'
            )
        self.state.mode = detect_mode(task_text, self.cfg.mode)
        if not gitops.is_git_repo(self.cfg.repo):
            raise OrchestratorError(f"{self.cfg.repo} is not a git repository")
        self.run_dir.mkdir(parents=True, exist_ok=True)
        # Immutable snapshot: both agents get the exact same contract, and a
        # later edit of .claudex/task.md cannot skew a run in flight.
        shutil.copyfile(task, self.task_snapshot)
        self.state.base_commit = gitops.head_commit(self.cfg.repo)
        self.state.branch = f"claudex/{self.state.run_id}"
        if self.report_mode:
            # The report IS the single step; no plan-about-the-work layer.
            self.state.steps = [
                {
                    "title": "Draft and commit the report",
                    "description": "Write the complete report the task contract "
                    "requires and commit it.",
                    "files": [],
                    "tests": [],
                }
            ]
            self._ensure_worktree()
            self.state.advance(Phase.IMPLEMENT_STEP, "report mode: single step")
        else:
            self.state.advance(Phase.PLAN_DRAFT)

    # -------------------------------------------------------- plan converge
    # plan_round counts critique rounds consumed and doubles as the index of
    # the current plan version: critique r reads plan-round-r and writes
    # plan-critique-r; a revision writes plan-round-(r+1).
    def _plan_path(self, round_no: int) -> Path:
        return self._art(f"plan-round-{round_no}.json")

    def _critique_path(self, round_no: int) -> Path:
        return self._art(f"plan-critique-{round_no}.json")

    def phase_plan_draft(self) -> None:
        """Headless only: the lead agent drafts the initial plan. In live
        mode the interactive lead drafts it and submits via `pair plan`."""
        if self._plan_path(0).exists():
            self.say("plan draft already present, skipping")
        else:
            res = self._run_agent(
                self.state.lead,
                prompts.plan_draft(self.task_snapshot),
                cwd=self.cfg.repo,
                read_only=True,
                schema=schemas.PAIR_PLAN_SCHEMA,
                label="plan-draft",
            )
            plan = res.require_structured()
            self._remember_session("lead_plan", res.session_id)
            self._store_plan(plan, 0)
        self.state.advance(Phase.PLAN_CRITIQUE)

    def _store_plan(self, plan: dict, round_no: int) -> None:
        artifacts.save_json(self._plan_path(round_no), plan)
        self._art(f"plan-round-{round_no}.md").write_text(
            artifacts.render_plan(self.state.lead, round_no, plan), encoding="utf-8"
        )
        titles = "\n".join(
            f"  {i + 1}. {s.get('title', '?')}" for i, s in enumerate(plan.get("steps", []))
        )
        self._post(
            "LEAD",
            "plan",
            "PROPOSE" if round_no == 0 else "REVISE",
            f"plan round {round_no}: {self._plan_path(round_no)}\n"
            f"steps ({len(plan.get('steps', []))}):\n{titles}",
        )

    def submit_plan(self, plan: dict) -> None:
        """Live mode: the interactive lead submits its (initial or revised)
        plan for critique."""
        validate_plan_shape(plan)
        self._store_plan(plan, self.state.plan_round)
        self.state.advance(Phase.PLAN_CRITIQUE, "live plan submitted")
        self._save()

    def phase_plan_critique(self) -> None:
        self.pair_plan_turn()

    def pair_plan_turn(self) -> dict:
        """One critique round. Shared by both drivers."""
        rnd = self.state.plan_round
        cpath = self._critique_path(rnd)
        if cpath.exists():
            self.say(f"plan critique {rnd} already present, skipping")
            critique = json.loads(cpath.read_text(encoding="utf-8"))
        else:
            guidance = self._take_guidance() if self.state.driver == "live" else ""
            res = self._run_agent(
                self.state.pair,
                prompts.plan_critique(
                    self.task_snapshot,
                    self._plan_path(rnd),
                    rnd,
                    guidance=guidance,
                ),
                cwd=self.cfg.repo,
                read_only=True,
                schema=schemas.PLAN_CRITIQUE_SCHEMA,
                # The pair keeps its critique context across rounds — it must
                # remember what it already conceded or escalated.
                resume=self.state.sessions.get("pair_plan", ""),
                label=f"plan-critique-{rnd}",
            )
            critique = res.require_structured()
            self._remember_session("pair_plan", res.session_id)
            artifacts.save_json(cpath, critique)
            self._art(f"plan-critique-{rnd}.md").write_text(
                artifacts.render_critique(self.state.pair, "Plan critique", rnd, critique),
                encoding="utf-8",
            )
        self._post(
            "PAIR",
            "plan",
            critique.get("verdict", "?"),
            artifacts.findings_digest(critique.get("findings", [])),
        )
        if schemas.is_converged(critique):
            self._lock_agreed_plan(rnd)
            self.state.advance(Phase.IMPLEMENT_STEP, f"plan agreed at round {rnd}")
        else:
            self.state.plan_round = rnd + 1  # round consumed
            cap = self.cfg.max_plan_rounds + self.state.plan_cap_extra
            if self.state.plan_round >= cap:
                self.state.gate(
                    f"plan not converged after {self.state.plan_round} round(s); "
                    f"open findings in {cpath.name}",
                    Phase.PLAN_REVISE,
                )
            else:
                self.state.advance(Phase.PLAN_REVISE)
        self._save()
        return critique

    def _lock_agreed_plan(self, round_no: int) -> None:
        plan = json.loads(self._plan_path(round_no).read_text(encoding="utf-8"))
        steps = plan.get("steps", [])
        if not steps:
            raise OrchestratorError("agreed plan has no steps — nothing to implement")
        self.state.steps = steps
        shutil.copyfile(self._plan_path(round_no), self._art("agreed-plan.json"))
        self._art("agreed-plan.md").write_text(
            artifacts.render_plan(self.state.lead, round_no, plan), encoding="utf-8"
        )
        self._ensure_worktree()

    def phase_plan_revise(self) -> None:
        """Headless only: the lead agent revises. In live mode the
        interactive lead revises and re-submits via `pair plan`."""
        rnd = self.state.plan_round  # the version this revision will become
        if self._plan_path(rnd).exists():
            self.say(f"plan revision {rnd} already present, skipping")
        else:
            guidance = self._take_guidance()
            res = self._run_agent(
                self.state.lead,
                prompts.plan_revise(
                    self.task_snapshot,
                    self._critique_path(rnd - 1),
                    rnd - 1,
                    guidance=guidance,
                ),
                cwd=self.cfg.repo,
                read_only=True,
                schema=schemas.PLAN_REVISION_SCHEMA,
                resume=self.state.sessions.get("lead_plan", ""),
                label=f"plan-revise-{rnd}",
            )
            revision = res.require_structured()
            self._remember_session("lead_plan", res.session_id)
            self._store_plan(revision, rnd)
        self.state.advance(Phase.PLAN_CRITIQUE)

    # ------------------------------------------------------------- implement
    def _ensure_worktree(self) -> None:
        wt = gitops.worktree_path_for(self.cfg.repo, self.state.run_id)
        if not wt.exists():
            gitops.add_worktree(
                self.cfg.repo, self.state.branch, wt, self.state.base_commit
            )
        self.state.worktree = str(wt)
        if not self.state.last_reviewed_commit:
            self.state.last_reviewed_commit = self.state.base_commit

    def _current_step(self) -> dict:
        try:
            return self.state.steps[self.state.step_index]
        except IndexError:
            raise OrchestratorError(
                f"step index {self.state.step_index} out of range "
                f"({len(self.state.steps)} steps)"
            )

    def phase_implement_step(self) -> None:
        """Headless only: the lead agent implements the current step. In
        live mode the interactive lead codes and commits, then runs
        `claudex pair checkpoint`."""
        self._ensure_worktree()
        wt = Path(self.state.worktree)
        i = self.state.step_index
        step = self._current_step()
        prev_head = gitops.head_commit(wt)
        if self.report_mode:
            prompt = prompts.draft_report(self.task_snapshot)
        else:
            prompt = prompts.implement_step(
                self.task_snapshot,
                self._art("agreed-plan.md"),
                i,
                len(self.state.steps),
                step,
            )
        res = self._run_agent(
            self.state.lead,
            prompt,
            cwd=wt,
            read_only=False,
            schema=schemas.IMPLEMENTATION_REPORT_SCHEMA,
            # Fresh session at step 0 (plan context arrives by file path; a
            # plan-lineage session would be pinned to the wrong cwd), then
            # one continuous implementation lineage.
            resume=self.state.sessions.get("lead_impl", "") if i > 0 else "",
            label=f"implement-step-{i}",
        )
        report = res.require_structured()
        self._remember_session("lead_impl", res.session_id)
        self._store_implementation(report, f"step-{i}-report")
        if gitops.head_commit(wt) == prev_head:
            raise OrchestratorError(
                f"step {i + 1} produced no commits — nothing to review"
            )
        commits = gitops.commits_between(wt, prev_head)
        self._post(
            "LEAD",
            f"step {i + 1}/{len(self.state.steps)}",
            "COMMIT",
            f"{step.get('title', '')}\n"
            + "\n".join(f"  {c['sha'][:12]} {c['message']}" for c in commits),
        )
        self.state.advance(Phase.CHECKPOINT)

    def _store_implementation(self, report: dict, stem: str) -> None:
        artifacts.save_json(self._art(f"{stem}.json"), report)
        self._art(f"{stem}.md").write_text(
            artifacts.render_implementation_report(self.state.lead, report),
            encoding="utf-8",
        )

    # ------------------------------------------------------------ checkpoint
    def phase_checkpoint(self) -> None:
        self.pair_checkpoint_turn()

    def pair_checkpoint_turn(self, lead_notes: str = "") -> dict:
        """One checkpoint-review round for the current step. Shared by both
        drivers; live mode passes the interactive lead's notes."""
        self._ensure_worktree()
        wt = Path(self.state.worktree)
        if gitops.has_uncommitted_changes(wt):
            # A dirty tree means the reviewed diff is not what would ship.
            raise OrchestratorError(
                f"worktree has uncommitted changes ({wt}) — commit them first; "
                "the pair reviews exact commits only"
            )
        base = self.state.last_reviewed_commit
        if gitops.head_commit(wt) == base:
            raise OrchestratorError(
                "no new commits since the last reviewed commit — nothing to review"
            )
        i = self.state.step_index
        rnd = self.state.checkpoint_round
        step = self._current_step()
        if lead_notes:
            self._post(
                "LEAD", f"step {i + 1}/{len(self.state.steps)}", "COMMIT", lead_notes
            )
        diff_path = self._art(f"diff-step-{i}-r{rnd}.patch")
        diff_path.write_text(gitops.diff_text(wt, base), encoding="utf-8")
        review_path = self._art(f"checkpoint-{i}-r{rnd}.json")
        if review_path.exists():
            self.say(f"checkpoint {i} round {rnd} already present, skipping")
            review = json.loads(review_path.read_text(encoding="utf-8"))
        else:
            guidance = self._take_guidance() if self.state.driver == "live" else ""
            res = self._run_agent(
                self.state.pair,
                prompts.checkpoint_review(
                    self.task_snapshot,
                    None if self.report_mode else self._agreed_plan(),
                    f"step {i + 1}/{len(self.state.steps)}: {step.get('title', '')}",
                    diff_path,
                    base,
                    rnd,
                    guidance=guidance,
                ),
                cwd=wt,
                read_only=True,
                schema=schemas.CHECKPOINT_REVIEW_SCHEMA,
                # Review lineage lives in the worktree cwd and never mixes
                # with the plan lineage (resume pins the original cwd).
                resume=self.state.sessions.get("pair_review", ""),
                label=f"checkpoint-{i}-r{rnd}",
            )
            review = res.require_structured()
            self._remember_session("pair_review", res.session_id)
            artifacts.save_json(review_path, review)
            self._art(f"checkpoint-{i}-r{rnd}.md").write_text(
                artifacts.render_critique(
                    self.state.pair, f"Checkpoint step {i + 1}", rnd, review
                ),
                encoding="utf-8",
            )
        self._post(
            "PAIR",
            f"step {i + 1}/{len(self.state.steps)}",
            review.get("verdict", "?"),
            artifacts.findings_digest(review.get("findings", [])),
        )
        if schemas.is_converged(review):
            self.state.last_reviewed_commit = gitops.head_commit(wt)
            self.state.checkpoint_round = 0
            self.state.checkpoint_cap_extra = 0
            if self.state.step_index + 1 < len(self.state.steps):
                self.state.step_index += 1
                self.state.advance(Phase.IMPLEMENT_STEP, f"step {i + 1} agreed")
            else:
                self.state.advance(Phase.TESTS, "all steps agreed")
        else:
            self.state.checkpoint_round = rnd + 1  # round consumed
            self.state.findings_file = str(review_path)
            self.state.fix_return = Phase.CHECKPOINT.value
            cap = self.cfg.max_checkpoint_rounds + self.state.checkpoint_cap_extra
            if self.state.checkpoint_round >= cap:
                self.state.gate(
                    f"step {i + 1} not agreed after {self.state.checkpoint_round} "
                    f"review round(s); open findings in {review_path.name}",
                    Phase.FIX,
                )
            else:
                self.state.advance(Phase.FIX)
        self._save()
        return review

    # ------------------------------------------------------------------- fix
    def phase_fix(self) -> None:
        """Headless only: the lead agent fixes the open findings. In live
        mode the interactive lead fixes, commits, and re-runs the loop that
        produced the findings."""
        wt = Path(self.state.worktree)
        prev_head = gitops.head_commit(wt)
        findings_path = Path(self.state.findings_file)
        self.state.fix_round += 1
        source = {
            Phase.CHECKPOINT.value: "Your pair's checkpoint review",
            Phase.TESTS.value: "The mechanical test gate",
            Phase.VERIFY.value: "The fresh-context final verification",
        }.get(self.state.fix_return, "A review")
        guidance = self._take_guidance()
        prompt = prompts.fix(findings_path, self.state.fix_round, source)
        if guidance:
            prompt += prompts.guidance_block(guidance)
        res = self._run_agent(
            self.state.lead,
            prompt,
            cwd=wt,
            read_only=False,
            schema=schemas.IMPLEMENTATION_REPORT_SCHEMA,
            resume=self.state.sessions.get("lead_impl", ""),
            label=f"fix-{self.state.fix_round}",
        )
        report = res.require_structured()
        self._remember_session("lead_impl", res.session_id)
        self._store_implementation(report, f"fix-{self.state.fix_round}-report")
        if gitops.head_commit(wt) == prev_head:
            self.say("WARNING: fix made no new commit.")
        self._post(
            "LEAD",
            "fix",
            "COMMIT",
            f"fix round {self.state.fix_round} for {findings_path.name}",
        )
        self.state.advance(Phase(self.state.fix_return or Phase.CHECKPOINT.value))

    # ----------------------------------------------------------------- tests
    def phase_tests(self) -> None:
        self.run_test_gate()

    def run_test_gate(self) -> bool:
        """Mechanical gate: the coordinator runs the configured test command
        itself — exit code decides, never agent testimony. Shared by both
        drivers. Returns True if the gate passed (or is unconfigured)."""
        if not self.cfg.test_command:
            self.say("no test_command configured — skipping mechanical gate")
            self.state.advance(Phase.VERIFY, "test gate skipped")
            self._save()
            return True
        wt = Path(self.state.worktree)
        rnd = self.state.test_round
        self.say(f"test gate: `{self.cfg.test_command}` in {wt} ...")
        proc = subprocess.run(
            self.cfg.test_command,
            shell=True,
            cwd=str(wt),
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=self.cfg.agent_timeout,
        )
        output = (proc.stdout or "") + ("\n" + proc.stderr if proc.stderr else "")
        out_path = self._art(f"tests-round-{rnd}.txt")
        out_path.write_text(
            f"$ {self.cfg.test_command}\nexit: {proc.returncode}\n\n{output}",
            encoding="utf-8",
        )
        passed = proc.returncode == 0
        tail = "\n".join(output.strip().splitlines()[-15:])
        self._post(
            "COORD",
            "tests",
            "PASS" if passed else "FAIL",
            f"`{self.cfg.test_command}` exit {proc.returncode}\n{tail}",
        )
        if passed:
            self.state.advance(Phase.VERIFY, "test gate passed")
            self._save()
            return True
        findings = {
            "findings": [
                {
                    "severity": "blocking",
                    "file": None,
                    "line": None,
                    "problem": f"test command failed (exit {proc.returncode}): "
                    f"{self.cfg.test_command}",
                    "evidence": f"see {out_path} (last lines):\n{tail}",
                    "suggested_fix": "",
                }
            ],
            "verdict": "REVISE",
        }
        findings_path = self._art(f"tests-findings-{rnd}.json")
        artifacts.save_json(findings_path, findings)
        self.state.test_round = rnd + 1  # attempt consumed
        self.state.findings_file = str(findings_path)
        self.state.fix_return = Phase.TESTS.value
        cap = self.cfg.max_test_rounds + self.state.test_cap_extra
        if self.state.test_round >= cap:
            self.state.gate(
                f"test command still failing after {self.state.test_round} "
                f"attempt(s) — see {out_path.name}",
                Phase.FIX,
            )
        else:
            self.state.advance(Phase.FIX, "test gate failed")
        self._save()
        return False

    # ---------------------------------------------------------------- verify
    def phase_verify(self) -> None:
        self.pair_verify_turn()

    def pair_verify_turn(self) -> dict:
        """Fresh-context verification. Shared by both drivers. On pass the
        run is DONE — the stop condition is agreement + evidence, not a
        human signature."""
        wt = Path(self.state.worktree)
        if gitops.has_uncommitted_changes(wt):
            raise OrchestratorError(
                f"worktree has uncommitted changes ({wt}) — commit them first"
            )
        rnd = self.state.verify_round
        diff_path = self._art("diff-final.patch")
        diff_path.write_text(
            gitops.diff_text(wt, self.state.base_commit), encoding="utf-8"
        )
        gate_summary = ""
        if self.cfg.test_command:
            for r in range(self.state.test_round, -1, -1):
                tp = self._art(f"tests-round-{r}.txt")
                if tp.exists():
                    first = tp.read_text(encoding="utf-8").splitlines()[:2]
                    gate_summary = " · ".join(first)
                    break
        res = self._run_agent(
            self.state.pair,
            prompts.verify(
                self.task_snapshot,
                None if self.report_mode else self._agreed_plan(),
                diff_path,
                self.state.base_commit,
                test_gate_summary=gate_summary,
            ),
            cwd=wt,
            read_only=True,
            schema=schemas.VERIFICATION_SCHEMA,
            # Deliberately NO resume: the verifier must not inherit the
            # review conversation, let alone the implementation one.
            label=f"verify-{rnd}",
        )
        verdict = res.require_structured()
        artifacts.save_json(self._art(f"verification-{rnd}.json"), verdict)
        self._art("verification.md").write_text(
            artifacts.render_verification(self.state.pair, verdict),
            encoding="utf-8",
        )
        failed = [c for c in verdict.get("criteria", []) if not c.get("met")]
        self._post(
            "PAIR",
            "verify",
            verdict.get("verdict", "?").upper(),
            "all acceptance criteria met"
            if not failed
            else "\n".join(f"unmet: {c.get('criterion', '?')}" for c in failed),
        )
        if verdict.get("verdict") == "pass":
            self._finish()
            self._save()
            return verdict
        findings = {
            "findings": [
                {
                    "severity": "blocking",
                    "file": None,
                    "line": None,
                    "problem": f"acceptance criterion not met: {c.get('criterion', '')}",
                    "evidence": c.get("evidence", ""),
                    "suggested_fix": "",
                }
                for c in failed
            ],
            "verdict": "REVISE",
        }
        findings_path = self._art(f"verification-findings-{rnd}.json")
        artifacts.save_json(findings_path, findings)
        self.state.verify_round = rnd + 1  # attempt consumed
        self.state.findings_file = str(findings_path)
        self.state.fix_return = Phase.VERIFY.value
        cap = self.cfg.max_verify_rounds + self.state.verify_cap_extra
        if self.state.verify_round >= cap:
            self.state.gate(
                f"verification still failing after {self.state.verify_round} "
                "attempt(s) — see verification.md",
                Phase.FIX,
            )
        else:
            self.state.advance(Phase.FIX, "verification failed")
        self._save()
        return verdict

    def _finish(self) -> None:
        self.state.advance(Phase.DONE, "verified")
        self._post("COORD", "done", "DONE", f"merge with: git merge {self.state.branch}")
        # Persist DONE before the history write: a failing append_history
        # must not leave state.json behind the in-memory phase.
        self._save()
        append_history(
            self.cfg.repo,
            {
                "run_id": self.state.run_id,
                "lead": self.state.lead,
                "pair": self.state.pair,
                "branch": self.state.branch,
                "base_commit": self.state.base_commit,
                "completed_at": self.state.events[-1]["ts"],
            },
        )
        self.say(f"DONE (verified). Merge with:\n  git merge {self.state.branch}")
        self.say(
            f"Then clean up with: claudex clean   (removes worktree {self.state.worktree})"
        )

    # ------------------------------------------------------------------ gates
    def resolve_guidance(self, notes: str) -> None:
        """AWAIT_GUIDANCE resolution: record the human's decision, re-arm the
        cap that gated (by raising its ceiling — counters never reset), and
        return control to the phase the gate interrupted."""
        if Phase(self.state.phase) is not Phase.AWAIT_GUIDANCE:
            raise OrchestratorError(
                f"run is in phase {self.state.phase}, not awaiting guidance"
            )
        if not notes.strip():
            raise OrchestratorError("guidance notes must not be empty")
        self.state.guidance_notes = notes
        self._post("HUMAN", "guidance", "GUIDANCE", notes)
        ret = Phase(self.state.return_phase or Phase.PLAN_REVISE.value)
        if ret is Phase.PLAN_REVISE:
            self.state.plan_cap_extra += self.cfg.max_plan_rounds
        elif self.state.fix_return == Phase.CHECKPOINT.value:
            self.state.checkpoint_cap_extra += self.cfg.max_checkpoint_rounds
        elif self.state.fix_return == Phase.TESTS.value:
            self.state.test_cap_extra += self.cfg.max_test_rounds
        elif self.state.fix_return == Phase.VERIFY.value:
            self.state.verify_cap_extra += self.cfg.max_verify_rounds
        self.state.gate_reason = ""
        self.state.return_phase = ""
        self.state.advance(ret, "guidance received")
        self._save()

    def abort(self) -> None:
        self.state.advance(Phase.ABORTED)
        self._save()

    def clean(self) -> None:
        wt = Path(self.state.worktree) if self.state.worktree else None
        if wt and wt.exists():
            gitops.remove_worktree(self.cfg.repo, wt, force=True)
            self.say(f"Removed worktree {wt}")
        else:
            self.say("No worktree to remove.")
