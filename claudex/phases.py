"""The orchestrator: deterministic phase driver.

Pipeline (mirrors the co-engineering protocol):

    INIT
      -> INVESTIGATE          both agents in parallel, read-only, blind to each other
      -> DISAGREEMENT         reviewer compares analyses against the repo
      -> AWAIT_PLAN_SELECTION human gate (or --auto-plan)
      -> PLAN_REVIEW          non-author adversarially attacks the chosen plan
      -> PLAN_FINALIZE        author addresses blocking corrections (skipped on clean approve)
      -> IMPLEMENT            owner edits in an isolated worktree, commits
      -> REVIEW               reviewer reads exact diff + commit, read-only
      -> REMEDIATE            owner resumes its session, fixes, commits   (loops)
      -> VERIFY               reviewer with FRESH context checks acceptance criteria
      -> AWAIT_FINAL_APPROVAL human gate
      -> DONE

The coordinator — not either model — owns phase transitions, edit
permissions, artifact routing, and disagreement surfacing.
"""

from __future__ import annotations

import concurrent.futures
import json
import re
import shutil
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


class Orchestrator:
    def __init__(self, cfg: Config, state: RunState):
        self.cfg = cfg
        self.state = state
        self.run_dir = run_dir_for(cfg.repo, state.run_id)
        self.agents = build_agents(cfg)

    # ------------------------------------------------------------- utilities
    def _save(self) -> None:
        self.state.save(self.run_dir)

    def _art(self, name: str) -> Path:
        return self.run_dir / name

    def say(self, msg: str) -> None:
        print(f"[claudex] {msg}", flush=True)

    @property
    def task_snapshot(self) -> Path:
        return self._art("task.md")

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
            self._save()  # durable: Ctrl+C here loses nothing, `claudex run` resumes
            time.sleep(delay)
            waits += 1

    # ----------------------------------------------------------------- driver
    def run_until_gate(self) -> None:
        handlers = {
            Phase.INIT: self.phase_init,
            Phase.INVESTIGATE: self.phase_investigate,
            Phase.DISAGREEMENT: self.phase_disagreement,
            Phase.PLAN_REVIEW: self.phase_plan_review,
            Phase.PLAN_FINALIZE: self.phase_plan_finalize,
            Phase.IMPLEMENT: self.phase_implement,
            Phase.REVIEW: self.phase_review,
            Phase.REMEDIATE: self.phase_remediate,
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
        phase = Phase(self.state.phase)
        if phase is Phase.AWAIT_PLAN_SELECTION:
            self.say("GATE: plan selection required.")
            self.say(f"  Read: {self._art('claude-analysis.md')}")
            self.say(f"        {self._art('codex-analysis.md')}")
            self.say(f"        {self._art('disagreement.md')}")
            self.say("  Then: claudex approve plan claude|codex [--notes '...']")
        elif phase is Phase.AWAIT_FINAL_APPROVAL:
            self.say("GATE: final human approval required.")
            self.say(f"  Read: {self._art('verification.md')}")
            self.say(f"        {self._art(f'diff-round-{self.state.review_round}.patch')}")
            self.say(f"  Branch: {self.state.branch}  Worktree: {self.state.worktree}")
            self.say("  Then: claudex approve final   (or claudex abort)")

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
        if not task_has_content(task.read_text(encoding="utf-8")):
            raise OrchestratorError(
                f"task contract is an unfilled template: {task} — fill it in, "
                'or draft it from a description with `claudex task "..."`'
            )
        if not gitops.is_git_repo(self.cfg.repo):
            raise OrchestratorError(f"{self.cfg.repo} is not a git repository")
        self.run_dir.mkdir(parents=True, exist_ok=True)
        # Immutable snapshot: both agents get the exact same contract, and a
        # later edit of .claudex/task.md cannot skew a run in flight.
        shutil.copyfile(task, self.task_snapshot)
        self.state.base_commit = gitops.head_commit(self.cfg.repo)
        self.state.branch = f"claudex/{self.state.run_id}"
        self.state.advance(Phase.INVESTIGATE)

    def phase_investigate(self) -> None:
        prompt = prompts.investigation(self.task_snapshot)

        def investigate(name: str):
            return name, self._run_agent(
                name,
                prompt,
                cwd=self.cfg.repo,
                read_only=True,
                schema=schemas.ANALYSIS_SCHEMA,
                label=f"{name}-investigate",
            )

        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results = dict(pool.map(investigate, ("claude", "codex")))

        for name, res in results.items():
            analysis = res.require_structured()
            self.state.sessions[name] = res.session_id
            artifacts.save_json(self._art(f"{name}-analysis.json"), analysis)
            self._art(f"{name}-analysis.md").write_text(
                artifacts.render_analysis(name, analysis), encoding="utf-8"
            )
        self.state.advance(Phase.DISAGREEMENT)

    def phase_disagreement(self) -> None:
        res = self._run_agent(
            self.state.reviewer,
            prompts.disagreement(
                self.task_snapshot,
                self._art("claude-analysis.json"),
                self._art("codex-analysis.json"),
            ),
            cwd=self.cfg.repo,
            read_only=True,
            schema=schemas.DISAGREEMENT_SCHEMA,
            label="disagreement",
        )
        d = res.require_structured()
        artifacts.save_json(self._art("disagreement.json"), d)
        self._art("disagreement.md").write_text(
            artifacts.render_disagreement(d), encoding="utf-8"
        )
        if self.cfg.auto_plan:
            choice = d.get("recommended_plan", "")
            if choice not in ("claude", "codex"):
                raise OrchestratorError(
                    f"--auto-plan set but recommendation invalid: {choice!r}"
                )
            self.select_plan(choice, notes="auto-selected from disagreement analysis")
        else:
            self.state.advance(Phase.AWAIT_PLAN_SELECTION)

    def select_plan(self, choice: str, notes: str = "") -> None:
        """Gate 1 resolution — invoked by `claudex approve plan` or --auto-plan."""
        analysis = json.loads(
            self._art(f"{choice}-analysis.json").read_text(encoding="utf-8")
        )
        self.state.selected_plan = choice
        self.state.plan_author = choice
        self.state.plan_selection_notes = notes
        self._art("selected-plan.md").write_text(
            artifacts.render_selected_plan(choice, analysis, notes), encoding="utf-8"
        )
        self.state.advance(Phase.PLAN_REVIEW, f"plan author: {choice}")

    def phase_plan_review(self) -> None:
        reviewer = self.state.plan_reviewer
        res = self._run_agent(
            reviewer,
            prompts.plan_review(self.task_snapshot, self._art("selected-plan.md")),
            cwd=self.cfg.repo,
            read_only=True,
            schema=schemas.PLAN_REVIEW_SCHEMA,
            label="plan-review",
        )
        review = res.require_structured()
        artifacts.save_json(self._art("plan-review.json"), review)
        self._art("plan-review.md").write_text(
            artifacts.render_plan_review(reviewer, review), encoding="utf-8"
        )
        if review.get("verdict") == "approve" and not review.get("blocking_corrections"):
            # Clean approve: the selected plan IS the agreed plan.
            shutil.copyfile(self._art("selected-plan.md"), self._art("agreed-plan.md"))
            self.state.advance(Phase.IMPLEMENT, "plan approved as-is")
        else:
            self.state.advance(Phase.PLAN_FINALIZE)

    def phase_plan_finalize(self) -> None:
        author = self.state.plan_author
        res = self._run_agent(
            author,
            prompts.plan_finalize(
                self.task_snapshot,
                self._art("selected-plan.md"),
                self._art("plan-review.json"),
            ),
            cwd=self.cfg.repo,
            read_only=True,
            schema=schemas.FINAL_PLAN_SCHEMA,
            # Resume the author's investigation session so the final plan is
            # written with full context of its own evidence.
            resume=self.state.sessions.get(author, ""),
            label="plan-finalize",
        )
        final = res.require_structured()
        addressed = final.get("blocking_corrections_addressed", [])
        plan_md = final.get("final_plan", "")
        if addressed:
            plan_md += "\n\n## Blocking corrections addressed\n\n" + "\n".join(
                f"- **{a.get('correction', '?')}** — {a.get('resolution', '')}"
                for a in addressed
            )
        self._art("agreed-plan.md").write_text(plan_md, encoding="utf-8")
        self.state.advance(Phase.IMPLEMENT)

    def phase_implement(self) -> None:
        wt = gitops.worktree_path_for(self.cfg.repo, self.state.run_id)
        if not wt.exists():
            gitops.add_worktree(
                self.cfg.repo, self.state.branch, wt, self.state.base_commit
            )
        self.state.worktree = str(wt)
        self._save()

        res = self._run_agent(
            self.state.owner,
            prompts.implement(self.task_snapshot, self._art("agreed-plan.md")),
            cwd=wt,
            read_only=False,
            schema=schemas.IMPLEMENTATION_REPORT_SCHEMA,
            label="implement",
        )
        report = res.require_structured()
        self.state.impl_session = res.session_id
        self._store_implementation(report, "implementation-report")

        commits = gitops.commits_between(wt, self.state.base_commit)
        if not commits:
            raise OrchestratorError(
                "implementation produced no commits — nothing to review"
            )
        if gitops.has_uncommitted_changes(wt):
            self.say("WARNING: worktree has uncommitted changes; review covers commits only.")
        self._write_diff()
        self.state.advance(Phase.REVIEW, f"{len(commits)} commit(s)")

    def _store_implementation(self, report: dict, stem: str) -> None:
        artifacts.save_json(self._art(f"{stem}.json"), report)
        self._art(f"{stem}.md").write_text(
            artifacts.render_implementation_report(self.state.owner, report),
            encoding="utf-8",
        )

    def _write_diff(self) -> Path:
        wt = Path(self.state.worktree)
        diff_path = self._art(f"diff-round-{self.state.review_round}.patch")
        diff_path.write_text(
            gitops.diff_text(wt, self.state.base_commit), encoding="utf-8"
        )
        return diff_path

    def phase_review(self) -> None:
        round_no = self.state.review_round
        report_stem = (
            "implementation-report" if round_no == 0 else f"remediation-report-{round_no}"
        )
        res = self._run_agent(
            self.state.reviewer,
            prompts.code_review(
                self.task_snapshot,
                self._art("agreed-plan.md"),
                self._art(f"diff-round-{round_no}.patch"),
                self._art(f"{report_stem}.json"),
                self.state.base_commit,
            ),
            cwd=Path(self.state.worktree),
            read_only=True,
            schema=schemas.CODE_REVIEW_SCHEMA,
            label=f"review-round-{round_no}",
        )
        review = res.require_structured()
        artifacts.save_json(self._art(f"review-round-{round_no}.json"), review)
        self._art(f"review-round-{round_no}.md").write_text(
            artifacts.render_code_review(self.state.reviewer, round_no, review),
            encoding="utf-8",
        )
        if review.get("verdict") == "approve":
            self.state.advance(Phase.VERIFY, f"approved at round {round_no}")
        elif round_no + 1 >= self.cfg.max_review_rounds:
            raise OrchestratorError(
                f"review still requests changes after {round_no + 1} round(s) — "
                f"human intervention required (see review-round-{round_no}.md)"
            )
        else:
            self.state.findings_file = str(self._art(f"review-round-{round_no}.json"))
            self.state.advance(Phase.REMEDIATE)

    def phase_remediate(self) -> None:
        wt = Path(self.state.worktree)
        prev_head = gitops.head_commit(wt)
        findings_path = Path(
            self.state.findings_file
            or self._art(f"review-round-{self.state.review_round}.json")
        )
        self.state.review_round += 1
        res = self._run_agent(
            self.state.owner,
            prompts.remediate(findings_path, self.state.review_round),
            cwd=wt,
            read_only=False,
            schema=schemas.IMPLEMENTATION_REPORT_SCHEMA,
            # Same engineer, same context: resume the implementation session.
            resume=self.state.impl_session,
            label=f"remediate-round-{self.state.review_round}",
        )
        report = res.require_structured()
        if res.session_id:
            self.state.impl_session = res.session_id
        self._store_implementation(report, f"remediation-report-{self.state.review_round}")
        if gitops.head_commit(wt) == prev_head:
            self.say("WARNING: remediation made no new commit.")
        self._write_diff()
        self.state.advance(Phase.REVIEW)

    def phase_verify(self) -> None:
        res = self._run_agent(
            self.state.reviewer,
            prompts.verify(
                self.task_snapshot,
                self._art("agreed-plan.md"),
                self._art(f"diff-round-{self.state.review_round}.patch"),
                self.state.base_commit,
            ),
            cwd=Path(self.state.worktree),
            read_only=True,
            schema=schemas.VERIFICATION_SCHEMA,
            # Deliberately NO resume: the verifier must not inherit the
            # review conversation, let alone the implementation one.
            label=f"verify-{self.state.verify_round}",
        )
        verdict = res.require_structured()
        artifacts.save_json(self._art("verification.json"), verdict)
        self._art("verification.md").write_text(
            artifacts.render_verification(self.state.reviewer, verdict),
            encoding="utf-8",
        )
        if verdict.get("verdict") == "pass":
            self.state.advance(Phase.AWAIT_FINAL_APPROVAL)
            return
        self.state.verify_round += 1
        if self.state.verify_round > self.cfg.max_verify_rounds:
            raise OrchestratorError(
                "verification still failing after "
                f"{self.cfg.max_verify_rounds} remediation attempt(s) — "
                "human intervention required (see verification.md)"
            )
        # Convert failed criteria into review findings and remediate.
        findings = {
            "findings": [
                {
                    "severity": "blocking",
                    "file": "",
                    "line": None,
                    "problem": f"acceptance criterion not met: {c.get('criterion', '')}",
                    "evidence": c.get("evidence", ""),
                    "suggested_fix": "",
                }
                for c in verdict.get("criteria", [])
                if not c.get("met")
            ],
            "tests_adequate": verdict.get("tests_meaningful", False),
            "tests_critique": verdict.get("notes", ""),
            "verdict": "request_changes",
        }
        findings_path = self._art(
            f"verification-findings-{self.state.verify_round}.json"
        )
        artifacts.save_json(findings_path, findings)
        self.state.findings_file = str(findings_path)
        self.state.advance(Phase.REMEDIATE, "verification failed")

    # ------------------------------------------------------------------ gates
    def approve_final(self) -> None:
        if Phase(self.state.phase) is not Phase.AWAIT_FINAL_APPROVAL:
            raise OrchestratorError(
                f"run is in phase {self.state.phase}, not awaiting final approval"
            )
        self.state.advance(Phase.DONE)
        self._save()
        append_history(
            self.cfg.repo,
            {
                "run_id": self.state.run_id,
                "owner": self.state.owner,
                "reviewer": self.state.reviewer,
                "branch": self.state.branch,
                "base_commit": self.state.base_commit,
                "completed_at": self.state.events[-1]["ts"],
            },
        )
        self.say(f"Approved. Merge with:\n  git merge {self.state.branch}")
        self.say(
            f"Then clean up with: claudex clean   (removes worktree {self.state.worktree})"
        )

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
