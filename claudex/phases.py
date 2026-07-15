"""The orchestrator: deterministic phase driver for pair programming.

Pipeline (one plan, one implementation, two agents converging on both):

    INIT               report mode -> synthesize the single report step,
                       skip the plan phases entirely
      -> PLAN_DRAFT    lead drafts the plan, grounded in the repo, read-only
      -> PLAN_CRITIQUE pair critiques it against the repo
      -> PLAN_REVISE   lead accepts or rebuts each finding, re-emits the plan
           (critique/revise loops until the pair AGREEs with zero
            blocking/major findings; each configured revision budget ends
            with a fresh-context audit of the lead's final response)
      -> IMPLEMENT_STEP lead implements exactly one plan step, commits
      -> CHECKPOINT    pair reviews that step's exact diff
      -> FIX           lead fixes blocking/major findings, commits  (loops)
      -> TESTS         coordinator runs the configured test command itself
      -> VERIFY        pair with FRESH context checks acceptance criteria
      -> DONE          automatically on verify pass
    AWAIT_GUIDANCE     a real decision needs `claudex resolve`; an exhausted
                       quality budget can use `claudex continue`

The coordinator — not either model — owns phase transitions, edit
permissions, artifact routing, response budgets, and the mailbox transcript.

Two drivers share every pair-turn method: headless `claudex run` calls them
from run_until_gate(); the live `claudex pair` subcommands (interactive
session as lead) call them directly. One code path, one cap check, one
mailbox append. One review attempt = one handler iteration, so the
save-per-iteration persistence gives attempt-granular crash durability.
Artifact counters only increase; separate response counters enforce complete
cycles. Budget extensions raise the ceiling rather than replaying artifacts.
"""

from __future__ import annotations

import json
import re
import shutil
import subprocess
import threading
import time
from pathlib import Path

from . import artifacts, budgets, evidence, gitops, planops, prompts, schemas
from .agents import (
    AgentError,
    ClaudeAgent,
    CodexAgent,
    classify_limit,
    resolve_claude_bin,
    resolve_codex_bin,
)
from .config import Config
from .events import AgentEvent
from .lifecycle import Lifecycle
from .state import (
    Phase,
    RunState,
    append_history,
    run_dir_for,
)


class OrchestratorError(RuntimeError):
    pass


class BudgetPause(OrchestratorError):
    """Control-flow signal: state is already durably paused, not failed."""


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
        policy=budgets.standalone_policy(cfg, "planning", agent_name),
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


def validate_revision_responses(critique: dict, revision: dict) -> None:
    """Mechanically require one response for every keyed blocking/major
    finding. Older v0.2 critiques had no keys and remain loadable."""
    findings = critique.get("findings", [])
    required = {
        str(f.get("key", "")).strip()
        for f in findings
        if f.get("severity") in ("blocking", "major")
        and str(f.get("key", "")).strip()
    }
    if not required:
        return
    known = {
        str(f.get("key", "")).strip()
        for f in findings
        if str(f.get("key", "")).strip()
    }
    response_keys = [
        str(response.get("finding_key", "")).strip()
        for response in revision.get("responses", [])
    ]
    duplicates = sorted({key for key in response_keys if response_keys.count(key) > 1})
    missing = sorted(required - set(response_keys))
    unknown = sorted({key for key in response_keys if key and key not in known})
    problems = []
    if missing:
        problems.append(f"missing responses for: {', '.join(missing)}")
    if duplicates:
        problems.append(f"duplicate responses for: {', '.join(duplicates)}")
    if unknown:
        problems.append(f"responses use unknown keys: {', '.join(unknown)}")
    if problems:
        raise OrchestratorError("invalid plan revision: " + "; ".join(problems))


def verification_findings(verdict: dict) -> list[dict]:
    """Derive the coordinator's verification outcome from evidence fields,
    rather than trusting a potentially contradictory pass/fail label."""
    findings = [
        {
            "severity": "blocking",
            "file": None,
            "line": None,
            "problem": f"acceptance criterion not met: {c.get('criterion', '')}",
            "evidence": c.get("evidence", ""),
            "suggested_fix": "",
        }
        for c in verdict.get("criteria", [])
        if not c.get("met")
    ]
    if verdict.get("tests_meaningful") is False:
        findings.append(
            {
                "severity": "blocking",
                "file": None,
                "line": None,
                "problem": "verification found that the tests are not meaningful",
                "evidence": verdict.get("notes", ""),
                "suggested_fix": "add assertions that fail when behavior is wrong",
            }
        )
    findings.extend(
        {
            "severity": "blocking",
            "file": None,
            "line": None,
            "problem": f"scope expanded beyond the contract: {item}",
            "evidence": item,
            "suggested_fix": "remove or justify the scope expansion",
        }
        for item in verdict.get("scope_expansion", [])
    )
    findings.extend(
        {
            "severity": "major",
            "file": None,
            "line": None,
            "problem": f"verification claim lacks repository evidence: {item}",
            "evidence": item,
            "suggested_fix": "supply repository/test evidence or correct the claim",
        }
        for item in verdict.get("unsupported_claims", [])
    )
    return findings


class Orchestrator:
    def __init__(self, cfg: Config, state: RunState):
        self.cfg = cfg
        self.state = state
        self.run_dir = run_dir_for(cfg.repo, state.run_id)
        self.agents = build_agents(cfg)
        self._save_lock = threading.Lock()
        self._live_text_attempts: set[str] = set()

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

    def _implementation_checks_path(self) -> Path | None:
        p = self._art("implementation-checks.json")
        return p if p.exists() else None

    def _merge_implementation_checks(self, critique: dict, plan_path: Path) -> None:
        incoming = critique.get("implementation_checks", [])
        if not incoming:
            return
        plan = json.loads(plan_path.read_text(encoding="utf-8"))
        step_titles = {
            str(step.get("title", "")).strip() for step in plan.get("steps", [])
        }
        merged = {
            str(item.get("key", "")).strip(): item
            for item in self.state.implementation_checks
            if str(item.get("key", "")).strip()
        }
        for item in incoming:
            key = str(item.get("key", "")).strip()
            if not key:
                raise OrchestratorError("implementation check key must not be empty")
            target = item.get("target_step")
            if target is not None and target not in step_titles:
                raise OrchestratorError(
                    f"implementation check {key!r} targets step {target}, but the "
                    "plan has no step with that exact title"
                )
            if item.get("action") == "remove":
                merged.pop(key, None)
            else:
                merged[key] = item
        self.state.implementation_checks = list(merged.values())
        artifacts.save_json(
            self._art("implementation-checks.json"),
            {"checks": self.state.implementation_checks},
        )

    def _post(self, role: str, stage: str, status: str, body: str) -> None:
        """Append one turn to the mailbox transcript. Coordinator-written:
        the pair is sandboxed read-only during critiques, and its output
        reaches us as schema-validated JSON anyway."""
        self.state.mailbox_turn += 1
        artifacts.mailbox_append(
            self.run_dir, role, self.state.mailbox_turn, stage, status, body
        )

    def _binding_guidance(self) -> str:
        """All human decisions remain visible to both roles for the run."""
        return "\n\n".join(
            f"Decision {i + 1}: {note}"
            for i, note in enumerate(self.state.binding_guidance)
            if str(note).strip()
        )

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
            violations = budgets.budget_violations(self.cfg, self.state)
            if violations:
                reason = "; ".join(violations)
                self.state.pause_budget(reason, Phase(self.state.phase))
                self._save()
                raise BudgetPause(reason)
            policy = budgets.invocation_policy(self.cfg, self.state, label, agent_name)
            budgets.record_attempt_started(self.state, policy)
            self._save()
            model = policy.model or getattr(agent, "model", "") or "provider default"
            remaining = budgets.remaining_budgets(self.cfg, self.state)
            effort_label = policy.effort
            if policy.requested_effort != policy.effort:
                effort_label += f" (requested {policy.requested_effort})"
            self.say(
                f"{agent_name}: policy {policy.profile}, model={model}, "
                f"effort={effort_label}, remaining calls={remaining['invocations']}, "
                f"reported cost=${remaining['cost_usd']:.2f}"
            )
            try:
                result = agent.run(
                    prompt,
                    run_dir=self.run_dir,
                    label=label,
                    timeout=policy.timeout_seconds,
                    policy=policy,
                    event_handler=self._on_agent_event,
                    **kw,
                )
            except AgentError:
                budgets.record_unreported_attempt(self.state)
                self._save()
                raise
            budgets.record_result(self.state, result)
            self._save()
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

    def _on_agent_event(self, event: AgentEvent) -> None:
        """High-signal live console view; the JSONL journal remains canonical."""
        if event.kind == "started":
            self.state.active_attempt_id = event.attempt_id
            self._save()
            return
        if event.kind == "text_delta":
            self._live_text_attempts.add(event.attempt_id)
            print(event.summary, end="", flush=True)
            return
        if event.kind == "message":
            if event.attempt_id not in self._live_text_attempts and event.summary:
                self.say(f"{event.agent}: {event.summary}")
            return
        if event.kind in ("tool_started", "tool_finished", "tool_progress"):
            status = event.tool_status or event.kind.removeprefix("tool_")
            self.say(f"{event.agent}: tool {event.tool_name or '?'} [{status}]")
            return
        if event.kind in ("warning", "stderr", "rate_limited"):
            self.say(f"{event.agent}: {event.kind}: {event.summary}")
            return
        if event.kind in ("provider_completed", "failed", "completed"):
            if event.attempt_id in self._live_text_attempts:
                print(flush=True)
                self._live_text_attempts.discard(event.attempt_id)

    def _remember_session(self, lineage: str, session_id: str) -> None:
        """Resumed Claude runs return a NEW session id every time; a missed
        update orphans the lineage. Always re-capture."""
        if session_id:
            self.state.sessions[lineage] = session_id

    def _require_conclusive_review(
        self,
        review: dict,
        *,
        lineage: str,
        label: str,
        artifact_path: Path | None = None,
    ) -> None:
        try:
            inconclusive = schemas.is_inconclusive_review(review)
        except schemas.ProtocolViolation as exc:
            self.state.sessions[lineage] = ""
            self._quarantine_review_artifact(artifact_path, "protocol-violation")
            raise OrchestratorError(f"{label} protocol violation: {exc}") from exc
        if not inconclusive:
            return
        requested = evidence.requested_repo_paths(
            self.cfg, list(review.get("missing_evidence") or [])
        )
        new_requests = [
            value for value in requested if value not in self.state.evidence_requests
        ]
        expanded = False
        if new_requests and self.state.evidence_expansions < 1:
            remaining = self.cfg.max_evidence_requests - len(self.state.evidence_requests)
            additions = new_requests[: max(0, remaining)]
            if additions:
                self.state.evidence_requests.extend(additions)
                self.state.evidence_expansions += 1
                expanded = True
        # A fresh retry should not inherit a session whose tool/evidence
        # state made it unable to review. Quarantine legacy artifacts so
        # artifact-presence retry logic does not replay them forever.
        self.state.sessions[lineage] = ""
        self._quarantine_review_artifact(artifact_path, "inconclusive")
        details = review.get("missing_evidence") or [
            review.get("tests_critique") or review.get("notes") or "no actionable finding"
        ]
        raise OrchestratorError(
            f"{label} was inconclusive and will not consume a lead response: "
            + "; ".join(str(item) for item in details if item)
            + (
                "; requested paths were added to the bounded retry packet"
                if expanded
                else ""
            )
        )

    def _quarantine_review_artifact(
        self, artifact_path: Path | None, suffix: str
    ) -> None:
        if artifact_path and artifact_path.exists():
            target = artifact_path.with_name(
                f"{artifact_path.stem}.{suffix}{artifact_path.suffix}"
            )
            n = 2
            while target.exists():
                target = artifact_path.with_name(
                    f"{artifact_path.stem}.{suffix}-{n}{artifact_path.suffix}"
                )
                n += 1
            artifact_path.replace(target)
            rendered = artifact_path.with_suffix(".md")
            if rendered.exists():
                rendered.replace(target.with_suffix(".md"))

    # ----------------------------------------------------------------- driver
    def _require_current_plan_protocol(self) -> None:
        phase = Phase(self.state.phase)
        planning = phase in {
            Phase.PLAN_DRAFT,
            Phase.PLAN_CRITIQUE,
            Phase.PLAN_REVISE,
        } or (
            phase is Phase.AWAIT_GUIDANCE
            and self.state.return_phase == Phase.PLAN_REVISE.value
        )
        if self.state.plan_protocol_version < 2 and planning:
            raise OrchestratorError(
                "this run uses the legacy exhaustive-plan protocol and cannot "
                "converge under Claudex 0.4; run `claudex abort`, then start a "
                "new run so content findings become implementation checks"
            )

    def run_until_gate(self) -> None:
        self._require_current_plan_protocol()
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
            except BudgetPause:
                self._report_gate()
                return
            except (AgentError, gitops.GitError, OrchestratorError) as exc:
                self.state.fail(str(exc))
                self._save()
                self.say(f"FAILED in {phase.value}: {exc}")
                self.say("Fix the cause, then `claudex retry` to re-attempt the phase.")
                return
            self._save()

    def _report_gate(self) -> None:
        if Phase(self.state.phase) is Phase.PAUSED_BUDGET:
            self.say("PAUSED_BUDGET: the run economic envelope is exhausted.")
            self.say(f"  Why: {self.state.gate_reason}")
            self.say("  Then: claudex resume --add-invocations N [other additions]")
            return
        if self.state.gate_kind == "decision":
            self.say("GATE: a concrete human decision is required.")
        else:
            self.say("GATE: the quality budget is exhausted; no model deadlock was inferred.")
        self.say(f"  Why: {self.state.gate_reason}")
        self.say(f"  Transcript: {artifacts.mailbox_path(self.run_dir)}")
        if self.state.gate_kind == "decision":
            self.say(
                "  Then: claudex resolve --notes 'your decision' "
                "(or --notes-file PATH for multiline text)"
            )
        else:
            self.say("  Then: claudex continue   (or resolve --notes if you want to steer)")

    def _report_terminal(self) -> None:
        phase = Phase(self.state.phase)
        if phase is Phase.DONE:
            self.say(f"DONE. Merge with: git merge {self.state.branch}")
        elif phase is Phase.FAILED:
            self.say(f"FAILED: {self.state.error}")
        else:
            self.say("Run aborted.")
        used = budgets.budget_usage(self.state)
        self.say(
            "Usage: "
            f"{used['invocations']} invocation(s), "
            f"{used['input_tokens']} reported input token(s), "
            f"{used['output_tokens']} reported output token(s), "
            f"${used['cost_usd']:.4f} provider-reported USD "
            f"({self.state.unknown_cost_attempts} unknown-cost attempt(s)), "
            f"{used['provider_seconds']:.1f}s provider time, "
            f"{used['wall_seconds']:.1f}s run wall time"
        )

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
            self._store_plan(plan, 0)
        self.state.advance(Phase.PLAN_CRITIQUE)

    def _store_plan(self, plan: dict, round_no: int) -> None:
        plan = planops.canonical_plan(plan)
        validate_plan_shape(plan)
        artifacts.save_json(self._plan_path(round_no), plan)
        self.state.canonical_plan_round = round_no
        self.state.canonical_plan_sha256 = planops.plan_digest(plan)
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
        if self.state.plan_round:
            critique = json.loads(
                self._critique_path(self.state.plan_round - 1).read_text(
                    encoding="utf-8"
                )
            )
            validate_revision_responses(critique, plan)
        self._store_plan(plan, self.state.plan_round)
        if self.state.plan_round:
            self.state.plan_revisions = max(
                self.state.plan_revisions, self.state.plan_round
            )
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
            self._require_conclusive_review(
                critique,
                lineage="pair_plan",
                label=f"plan critique {rnd}",
                artifact_path=cpath,
            )
        else:
            budget = self.cfg.max_plan_rounds + self.state.plan_cap_extra
            final_audit = self.state.plan_revisions >= budget
            try:
                manifest = evidence.build_plan_review_packet(
                    self.cfg, self.state, self.run_dir, self._plan_path(rnd)
                )
            except evidence.EvidenceError as exc:
                raise OrchestratorError(f"plan review evidence: {exc}") from exc
            self.state.last_evidence_manifest = str(manifest)
            self._save()
            res = self._run_agent(
                self.state.pair,
                prompts.plan_critique(
                    manifest,
                    rnd,
                    guidance=self._binding_guidance(),
                    final_audit=final_audit,
                ),
                cwd=self.run_dir,
                read_only=True,
                schema=schemas.PLAN_CRITIQUE_SCHEMA,
                # Every review is fresh; bounded ledgers carry only canonical
                # concessions and unresolved evidence across rounds.
                resume="",
                label=f"plan-critique-{rnd}",
            )
            critique = res.require_structured()
            self._require_conclusive_review(
                critique, lineage="pair_plan", label=f"plan critique {rnd}"
            )
            artifacts.save_json(cpath, critique)
            self._art(f"plan-critique-{rnd}.md").write_text(
                artifacts.render_critique(self.state.pair, "Plan critique", rnd, critique),
                encoding="utf-8",
            )
        self._merge_implementation_checks(critique, self._plan_path(rnd))
        self.state.unresolved_findings = [
            dict(item)
            for item in critique.get("findings", [])
            if item.get("severity") in ("blocking", "major")
        ]
        checks = critique.get("implementation_checks", [])
        digest = artifacts.findings_digest(critique.get("findings", []))
        if checks:
            digest += "\nimplementation checks: " + ", ".join(
                str(item.get("key", "?")) for item in checks
            )
        self._post(
            "PAIR",
            "plan",
            critique.get("verdict", "?"),
            digest,
        )
        try:
            decision_requested = schemas.requires_human_decision(critique)
        except schemas.ProtocolViolation as exc:
            raise OrchestratorError(
                f"plan critique {rnd} protocol violation: {exc}"
            ) from exc
        if decision_requested:
            self.state.plan_round = rnd + 1  # round consumed
            self.state.gate(
                critique.get("decision_question")
                or f"plan critique requests a decision; see {cpath.name}",
                Phase.PLAN_REVISE,
                kind="decision",
            )
        elif schemas.is_converged(critique):
            self.state.unresolved_findings = []
            self._lock_agreed_plan(rnd)
            self.state.advance(Phase.IMPLEMENT_STEP, f"plan agreed at round {rnd}")
        else:
            self.state.plan_round = rnd + 1  # round consumed
            budget = self.cfg.max_plan_rounds + self.state.plan_cap_extra
            final_audit = self.state.plan_revisions >= budget
            if final_audit:
                self.state.gate(
                    f"plan still has actionable findings after "
                    f"{self.state.plan_revisions} lead revision(s) and a fresh "
                    f"final audit; see {cpath.name}",
                    Phase.PLAN_REVISE,
                    kind="budget",
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
        titles = {str(step.get("title", "")).strip() for step in steps}
        checks_changed = False
        for item in self.state.implementation_checks:
            if item.get("target_step") and item["target_step"] not in titles:
                # A revision renamed/merged the target step after the check
                # was captured. Keeping it cross-cutting is safer than
                # silently orphaning the obligation.
                item["target_step"] = None
                checks_changed = True
        if checks_changed:
            artifacts.save_json(
                self._art("implementation-checks.json"),
                {"checks": self.state.implementation_checks},
            )
        shutil.copyfile(self._plan_path(round_no), self._art("agreed-plan.json"))
        self._art("agreed-plan.md").write_text(
            artifacts.render_plan(self.state.lead, round_no, plan), encoding="utf-8"
        )
        self._ensure_worktree()

    def phase_plan_revise(self) -> None:
        """Headless only: the lead agent revises. In live mode the
        interactive lead revises and re-submits via `pair plan`."""
        rnd = self.state.plan_round  # the version this revision will become
        revision_path = self._art(f"plan-revision-{rnd}.json")
        baseline = json.loads(
            self._plan_path(rnd - 1).read_text(encoding="utf-8")
        )
        manifest = Path(self.state.last_evidence_manifest)
        if not manifest.exists():
            try:
                manifest = evidence.build_plan_review_packet(
                    self.cfg,
                    self.state,
                    self.run_dir,
                    self._plan_path(rnd - 1),
                    round_no=rnd - 1,
                )
            except evidence.EvidenceError as exc:
                raise OrchestratorError(f"plan revision evidence: {exc}") from exc
            self.state.last_evidence_manifest = str(manifest)
        if self._plan_path(rnd).exists() and revision_path.exists():
            self.say(f"plan revision {rnd} already present, skipping")
            revision = json.loads(revision_path.read_text(encoding="utf-8"))
        else:
            res = self._run_agent(
                self.state.lead,
                prompts.plan_revise(
                    self.task_snapshot,
                    self._plan_path(rnd - 1),
                    self._critique_path(rnd - 1),
                    rnd - 1,
                    planops.plan_digest(baseline),
                    manifest,
                    guidance=self._binding_guidance(),
                ),
                cwd=self.run_dir,
                read_only=True,
                schema=schemas.PLAN_REVISION_SCHEMA,
                resume="",
                label=f"plan-revise-{rnd}",
            )
            revision = res.require_structured()
            artifacts.save_json(revision_path, revision)
        critique = json.loads(
            self._critique_path(rnd - 1).read_text(encoding="utf-8")
        )
        try:
            validate_revision_responses(critique, revision)
            canonical = planops.apply_revision_delta(baseline, revision)
            validate_plan_shape(canonical)
        except (planops.PlanDeltaError, OrchestratorError) as exc:
            self._quarantine_review_artifact(revision_path, "rejected")
            raise OrchestratorError(
                f"plan revision {rnd} rejected; canonical round {rnd - 1} "
                f"is unchanged: {exc}"
            ) from exc
        if not self._plan_path(rnd).exists():
            self._store_plan(canonical, rnd)
        finding_by_key = {
            str(item.get("key", "")): item
            for item in critique.get("findings", [])
        }
        known_accepted = {
            str(item.get("response", {}).get("finding_key", ""))
            for item in self.state.accepted_findings
        }
        for response in revision.get("responses", []):
            key = str(response.get("finding_key", ""))
            if response.get("action") == "accepted" and key not in known_accepted:
                self.state.accepted_findings.append(
                    {
                        "finding": finding_by_key.get(key, {}),
                        "response": dict(response),
                    }
                )
        self.state.plan_revisions = max(self.state.plan_revisions, rnd)
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
                self._implementation_checks_path(),
                i,
                len(self.state.steps),
                step,
            )
        prompt += prompts.guidance_block(self._binding_guidance())
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
            self._require_conclusive_review(
                review,
                lineage="pair_review",
                label=f"checkpoint {i} round {rnd}",
                artifact_path=review_path,
            )
        else:
            budget = self.cfg.max_checkpoint_rounds + self.state.checkpoint_cap_extra
            final_audit = self.state.checkpoint_fixes_used >= budget
            res = self._run_agent(
                self.state.pair,
                prompts.checkpoint_review(
                    self.task_snapshot,
                    None if self.report_mode else self._agreed_plan(),
                    self._implementation_checks_path(),
                    f"step {i + 1}/{len(self.state.steps)}: {step.get('title', '')}",
                    diff_path,
                    base,
                    rnd,
                    guidance=self._binding_guidance(),
                    final_audit=final_audit,
                ),
                cwd=wt,
                read_only=True,
                schema=schemas.CHECKPOINT_REVIEW_SCHEMA,
                # Review lineage lives in the worktree cwd and never mixes
                # with the plan lineage (resume pins the original cwd).
                resume=(
                    ""
                    if final_audit
                    else self.state.sessions.get("pair_review", "")
                ),
                label=f"checkpoint-{i}-r{rnd}",
            )
            review = res.require_structured()
            self._require_conclusive_review(
                review,
                lineage="pair_review",
                label=f"checkpoint {i} round {rnd}",
            )
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
            self.state.checkpoint_fixes_used = 0
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
            budget = self.cfg.max_checkpoint_rounds + self.state.checkpoint_cap_extra
            if self.state.checkpoint_fixes_used >= budget:
                self.state.gate(
                    f"step {i + 1} still has findings after "
                    f"{self.state.checkpoint_fixes_used} fix(es) and a final "
                    f"fresh review; see {review_path.name}",
                    Phase.FIX,
                    kind="budget",
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
        prompt = prompts.fix(findings_path, self.state.fix_round, source)
        prompt += prompts.guidance_block(self._binding_guidance())
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
        self.record_fix_completed()
        self.state.advance(Phase(self.state.fix_return or Phase.CHECKPOINT.value))

    def record_fix_completed(self) -> None:
        """Charge one completed lead response to the budget that requested
        it. Live mode calls this after the human lead has committed a fix;
        headless mode calls it after the fix agent succeeds."""
        if self.state.fix_return == Phase.CHECKPOINT.value:
            self.state.checkpoint_fixes_used += 1
        elif self.state.fix_return == Phase.TESTS.value:
            self.state.test_fixes_used += 1
        elif self.state.fix_return == Phase.VERIFY.value:
            self.state.verify_fixes_used += 1

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
        budget = self.cfg.max_test_rounds + self.state.test_cap_extra
        if self.state.test_fixes_used >= budget:
            self.state.gate(
                f"test command still fails after {self.state.test_fixes_used} "
                f"fix(es) and a final test attempt — see {out_path.name}",
                Phase.FIX,
                kind="budget",
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
                self._implementation_checks_path(),
                diff_path,
                self.state.base_commit,
                test_gate_summary=gate_summary,
                guidance=self._binding_guidance(),
            ),
            cwd=wt,
            read_only=True,
            schema=schemas.VERIFICATION_SCHEMA,
            # Deliberately NO resume: the verifier must not inherit the
            # review conversation, let alone the implementation one.
            label=f"verify-{rnd}",
        )
        verdict = res.require_structured()
        findings_list = verification_findings(verdict)
        if not verdict.get("criteria"):
            raise OrchestratorError(
                "final verification was inconclusive and will not consume a fix "
                "budget: verifier returned no acceptance-criterion results"
            )
        if verdict.get("verdict") != "pass" and not findings_list:
            raise OrchestratorError(
                "final verification was inconclusive and will not consume a fix "
                "budget: FAIL contained no actionable evidence"
            )
        artifacts.save_json(self._art(f"verification-{rnd}.json"), verdict)
        self._art("verification.md").write_text(
            artifacts.render_verification(self.state.pair, verdict),
            encoding="utf-8",
        )
        failed = [c for c in verdict.get("criteria", []) if not c.get("met")]
        effective_pass = verdict.get("verdict") == "pass" and not findings_list
        self._post(
            "PAIR",
            "verify",
            "PASS" if effective_pass else "FAIL",
            "all acceptance criteria met"
            if not failed
            else "\n".join(f"unmet: {c.get('criterion', '?')}" for c in failed),
        )
        if effective_pass:
            self._finish()
            self._save()
            return verdict
        findings = {"findings": findings_list, "verdict": "REVISE"}
        findings_path = self._art(f"verification-findings-{rnd}.json")
        artifacts.save_json(findings_path, findings)
        self.state.verify_round = rnd + 1  # attempt consumed
        self.state.findings_file = str(findings_path)
        self.state.fix_return = Phase.VERIFY.value
        budget = self.cfg.max_verify_rounds + self.state.verify_cap_extra
        if self.state.verify_fixes_used >= budget:
            self.state.gate(
                f"verification still fails after {self.state.verify_fixes_used} "
                "fix(es) and a final fresh verification — see verification.md",
                Phase.FIX,
                kind="budget",
            )
        else:
            self.state.advance(Phase.FIX, "verification failed")
        self._save()
        return verdict

    def _finish(self) -> None:
        self.state.advance(Phase.DONE, "verified")
        self.state.transition_lifecycle(
            Lifecycle.COMPLETED,
            "all acceptance criteria verified",
            "git merge then claudex clean",
        )
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
    def _reconcile_gate_lifecycle(self) -> None:
        """Migrate legacy/manual gate state before applying a resume action."""
        phase = Phase(self.state.phase)
        current = Lifecycle(self.state.lifecycle)
        if current is not Lifecycle.RUNNING:
            return
        if phase is Phase.AWAIT_GUIDANCE:
            self.state.transition_lifecycle(
                Lifecycle.PAUSED,
                "reconciled legacy guidance gate",
                "claudex resolve or claudex continue",
            )
        elif phase is Phase.PAUSED_BUDGET:
            self.state.transition_lifecycle(
                Lifecycle.PAUSED_BUDGET,
                "reconciled legacy run-budget gate",
                "claudex resume --add-...",
            )

    def resolve_guidance(self, notes: str) -> None:
        """Record a persistent human decision and resume the interrupted
        phase. A budget is expanded only when budget exhaustion caused the
        gate; an early decision does not silently buy extra cycles."""
        self._require_current_plan_protocol()
        if Phase(self.state.phase) is not Phase.AWAIT_GUIDANCE:
            raise OrchestratorError(
                f"run is in phase {self.state.phase}, not awaiting guidance"
            )
        if not notes.strip():
            raise OrchestratorError("guidance notes must not be empty")
        self._reconcile_gate_lifecycle()
        self.state.binding_guidance.append(notes.strip())
        self.state.guidance_notes = ""
        self._post("HUMAN", "guidance", "GUIDANCE", notes)
        ret = Phase(self.state.return_phase or Phase.PLAN_REVISE.value)
        if self.state.gate_kind == "budget" and self._budget_exhausted(ret):
            self._extend_active_budget(ret)
        elif (
            ret is Phase.PLAN_REVISE
            and self.state.plan_revisions
            >= self.cfg.max_plan_rounds + self.state.plan_cap_extra
        ):
            # A real choice can surface during the final audit. Its answer
            # must buy enough room for the corresponding revision and audit.
            self._extend_active_budget(ret)
        self._clear_gate()
        self.state.transition_lifecycle(
            Lifecycle.RUNNING,
            "binding guidance recorded",
            "claudex resume",
        )
        self.state.advance(ret, "guidance received")
        self._save()

    def continue_after_budget(self, responses: int = 1) -> bool:
        """Resume an exhausted quality budget without fabricating a human
        design decision. Real decision gates still require resolve. Returns
        whether the ceiling actually needed extension; migrated v0.2 gates
        may merely need the final lead response they previously denied."""
        self._require_current_plan_protocol()
        if Phase(self.state.phase) is not Phase.AWAIT_GUIDANCE:
            raise OrchestratorError(
                f"run is in phase {self.state.phase}, not awaiting guidance"
            )
        if self.state.gate_kind != "budget":
            raise OrchestratorError(
                "this gate requires a decision; use `claudex resolve --notes ...` "
                "or `claudex resolve --notes-file PATH`"
            )
        if responses < 1:
            raise OrchestratorError("additional response budget must be at least 1")
        self._reconcile_gate_lifecycle()
        ret = Phase(self.state.return_phase or Phase.PLAN_REVISE.value)
        extended = self._budget_exhausted(ret)
        if extended:
            self._extend_active_budget(ret, responses)
        action = (
            f"quality budget extended by {responses} response(s)"
            if extended
            else "incomplete cycle resumed"
        )
        self._post("HUMAN", "guidance", "CONTINUE", action)
        self._clear_gate()
        self.state.transition_lifecycle(
            Lifecycle.RUNNING,
            action,
            "claudex resume",
        )
        self.state.advance(ret, action)
        self._save()
        return extended

    def resume_budget(
        self,
        additions: dict[str, int | float],
        acknowledged_currencies: list[str] | None = None,
    ) -> None:
        """Explicitly expand a run envelope and resume its exact phase."""
        if Phase(self.state.phase) is not Phase.PAUSED_BUDGET:
            raise OrchestratorError(
                f"run is in phase {self.state.phase}, not paused for run budget"
            )
        self._reconcile_gate_lifecycle()
        if not any(value > 0 for value in additions.values()) and not acknowledged_currencies:
            raise OrchestratorError(
                "resume requires a positive budget addition or --acknowledge-currency"
            )
        previous_overrides = dict(self.state.budget_overrides)
        previous_ack = list(self.state.acknowledged_cost_currencies)
        budgets.add_overrides(self.state, additions)
        for value in acknowledged_currencies or []:
            currency = value.strip().upper()
            if not currency or len(currency) != 3 or not currency.isalpha():
                self.state.budget_overrides = previous_overrides
                raise OrchestratorError(
                    f"currency must be a three-letter code, got {value!r}"
                )
            if currency not in self.state.acknowledged_cost_currencies:
                self.state.acknowledged_cost_currencies.append(currency)
        violations = budgets.budget_violations(self.cfg, self.state)
        if violations:
            self.state.budget_overrides = previous_overrides
            self.state.acknowledged_cost_currencies = previous_ack
            raise OrchestratorError(
                "override does not clear the exhausted envelope: "
                + "; ".join(violations)
            )
        ret = Phase(self.state.return_phase)
        detail = ", ".join(
            f"+{value} {key}" for key, value in additions.items() if value
        )
        if acknowledged_currencies:
            detail += (", " if detail else "") + "acknowledged " + ", ".join(
                value.upper() for value in acknowledged_currencies
            )
        self._clear_gate()
        self.state.transition_lifecycle(
            Lifecycle.RUNNING,
            f"run budget override: {detail}",
            "claudex resume",
        )
        self.state.advance(ret, f"run budget override: {detail}")
        self._save()

    def resume_interrupted_continue(self) -> bool:
        """Recognize a continuation whose transition was saved before the
        headless driver was interrupted. This makes the command idempotent.
        It also narrows the old v0.3 five-response extension when none of
        those responses has started yet."""
        if self.state.driver != "headless" or not self.state.events:
            return False
        phase = Phase(self.state.phase)
        last = self.state.events[-1].get("message", "")
        prefix = f"await_guidance -> {phase.value} ("
        normalized_plan = (
            phase is Phase.PLAN_REVISE
            and last
            == f"normalized unused legacy plan budget to {self.state.plan_revisions + 1}"
        )
        continued = last.startswith(prefix) and (
            "quality budget extended" in last or "incomplete cycle resumed" in last
        )
        if not (continued or normalized_plan):
            return False
        if phase is Phase.PLAN_REVISE:
            # No response artifact means the extension has not been used.
            # Legacy `continue` added cfg.max_plan_rounds at once; preserve
            # only enough ceiling for the pending response + fresh audit.
            if not self._plan_path(self.state.plan_round).exists():
                desired_limit = self.state.plan_revisions + 1
                current_limit = self.cfg.max_plan_rounds + self.state.plan_cap_extra
                if current_limit > desired_limit and last.endswith(
                    "(quality budget extended)"
                ):
                    self.state.plan_cap_extra = max(
                        0, desired_limit - self.cfg.max_plan_rounds
                    )
                    self.state.log(
                        f"normalized unused legacy plan budget to {desired_limit}"
                    )
                    self._save()
            return True
        # Other interrupted return phases are safe to resume, but there is
        # no artifact-independent way to infer whether a human/live fix was
        # already made, so their counters are left unchanged.
        return phase is Phase.FIX

    def _budget_exhausted(self, return_phase: Phase) -> bool:
        if return_phase is Phase.PLAN_REVISE:
            return self.state.plan_revisions >= (
                self.cfg.max_plan_rounds + self.state.plan_cap_extra
            )
        if self.state.fix_return == Phase.CHECKPOINT.value:
            used, limit = (
                self.state.checkpoint_fixes_used,
                self.cfg.max_checkpoint_rounds + self.state.checkpoint_cap_extra,
            )
        elif self.state.fix_return == Phase.TESTS.value:
            used, limit = (
                self.state.test_fixes_used,
                self.cfg.max_test_rounds + self.state.test_cap_extra,
            )
        elif self.state.fix_return == Phase.VERIFY.value:
            used, limit = (
                self.state.verify_fixes_used,
                self.cfg.max_verify_rounds + self.state.verify_cap_extra,
            )
        else:
            # Unknown legacy gate: extending is safer than replaying the
            # same gate without giving either role another response.
            return True
        return used >= limit

    def _extend_active_budget(self, return_phase: Phase, responses: int = 1) -> None:
        if return_phase is Phase.PLAN_REVISE:
            self.state.plan_cap_extra += responses
        elif self.state.fix_return == Phase.CHECKPOINT.value:
            self.state.checkpoint_cap_extra += responses
        elif self.state.fix_return == Phase.TESTS.value:
            self.state.test_cap_extra += responses
        elif self.state.fix_return == Phase.VERIFY.value:
            self.state.verify_cap_extra += responses

    def _clear_gate(self) -> None:
        self.state.gate_reason = ""
        self.state.gate_kind = ""
        self.state.return_phase = ""

    def abort(self) -> None:
        self.state.advance(Phase.ABORTED)
        self.state.transition_lifecycle(
            Lifecycle.CANCELLED,
            "operator aborted run",
            "start a new run or inspect retained artifacts",
        )
        self._save()

    def clean(self) -> None:
        wt = Path(self.state.worktree) if self.state.worktree else None
        if wt and wt.exists():
            gitops.remove_worktree(self.cfg.repo, wt, force=True)
            self.say(f"Removed worktree {wt}")
        else:
            self.say("No worktree to remove.")
