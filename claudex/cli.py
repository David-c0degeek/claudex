"""claudex command-line interface.

    claudex init                    scaffold .claudex/task.md + config
    claudex task "..."              draft the task contract from a description
    claudex run [--lead claude|codex]
                                    headless pair run (drives both agents)
    claudex pair start              live run: YOUR session is the lead
    claudex pair plan --file f      submit your plan for pair critique
    claudex pair checkpoint         request pair review of your commits
    claudex pair verify             test gate + fresh-context verification
    claudex resolve --notes "..."   answer a concrete decision gate
    claudex resolve --notes-file p   answer from a file (safe for multiline text)
    claudex continue                extend an exhausted quality budget
    claudex resume [--add-...]      continue the same run/checkpoint
    claudex restart [--fresh-plan] replace execution identity without losing state
    claudex status                  show run state and artifacts
    claudex retry                   re-attempt the phase that failed
    claudex cancel                  cancel the active process tree or idle run
    claudex clean                   remove the run's worktree
    claudex doctor                  check git/claude/codex wiring
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import shutil
import subprocess
import sys
import time
from importlib import resources
from pathlib import Path

from . import __version__, gitops
from . import budgets, recovery
from .agents import AgentError
from .config import Config
from .lifecycle import Lifecycle
from .processes import terminate_process_tree
from .providers import ProviderError, resolve_provider
from .terminal import next_action, terminal_reason, watch_run
from .phases import (
    Orchestrator,
    OrchestratorError,
    build_agents,
    draft_task,
    task_has_content,
    validate_run_start,
    validate_plan_shape,
)
from .state import (
    AGENTS,
    Phase,
    RunLockError,
    RunState,
    acquire_run_lock,
    clear_current_run,
    get_current_run,
    new_run_id,
    release_run_lock,
    run_dir_for,
    set_current_run,
    task_file,
)


def _template(name: str) -> str:
    return (
        resources.files("claudex")
        .joinpath(f"templates/{name}")
        .read_text(encoding="utf-8")
    )


def _cfg(args, *, persist_migration: bool = True) -> Config:
    overrides = {
        k: getattr(args, k, None)
        for k in (
            "lead",
            "mode",
            "test_command",
            "max_plan_rounds",
            "max_checkpoint_rounds",
            "agent_timeout",
            "claude_model",
            "codex_model",
            "max_invocation_cost_usd",
            "max_invocation_turns",
            "max_invocation_output_tokens",
            "max_invocation_tool_calls",
            "max_run_invocations",
            "max_run_input_tokens",
            "max_run_output_tokens",
            "max_run_cost_usd",
            "max_run_tool_calls",
            "max_run_wall_seconds",
            "planning_effort",
            "implementation_effort",
            "verification_effort",
            "claude_planning_model",
            "claude_implementation_model",
            "claude_verification_model",
            "codex_planning_model",
            "codex_implementation_model",
            "codex_verification_model",
            "disable_nested_agents",
            "allow_expensive_profiles",
            "max_evidence_bytes",
            "max_evidence_file_bytes",
            "max_evidence_requests",
        )
    }
    return Config.load(
        Path(args.repo), overrides, persist_migration=persist_migration
    )


def _active_orchestrator(cfg: Config) -> Orchestrator:
    rid = get_current_run(cfg.repo)
    if not rid:
        raise OrchestratorError("no active run — start one with `claudex run` or `claudex pair start`")
    state = RunState.load(run_dir_for(cfg.repo, rid))
    return Orchestrator(cfg, state)


def _locked(cfg: Config, orch: Orchestrator, fn) -> int:
    """Serialize state-mutating work across processes: a live `claudex pair`
    call must never race a headless `claudex run` on the same counters."""
    acquire_run_lock(orch.run_dir)
    try:
        return fn()
    finally:
        release_run_lock(orch.run_dir)


# ------------------------------------------------------------------ commands
def cmd_init(args) -> int:
    cfg = _cfg(args)
    task = task_file(cfg.repo)
    task.parent.mkdir(parents=True, exist_ok=True)
    if task.exists() and not args.force:
        print(f"already exists: {task} (use --force to overwrite)")
    else:
        task.write_text(_template("task.md"), encoding="utf-8")
        print(f"wrote {task}")
    cfg.save()
    print(f"wrote {cfg.config_path}")
    if gitops.is_git_repo(cfg.repo):
        if gitops.ensure_gitignore_entry(cfg.repo, ".claudex/"):
            print("added .claudex/ to .gitignore")
    else:
        print("WARNING: not a git repository — claudex requires git before `run`")
    if args.with_skill:
        skill_dir = cfg.repo / ".claude" / "skills" / "claudex-pair"
        skill_dir.mkdir(parents=True, exist_ok=True)
        for src, dst in (
            ("SKILL.md", "SKILL.md"),
            ("codex-lead-prompt.md", "codex-lead-prompt.md"),
        ):
            (skill_dir / dst).write_text(_template(src), encoding="utf-8")
        print(f"wrote {skill_dir} (live-mode pairing skill)")
    print("\nNext: fill in .claudex/task.md by hand, or draft it from a")
    print('description with `claudex task "one paragraph of what you want"`,')
    print("then `claudex run` (headless) or `claudex pair start` (you lead)")
    return 0


def cmd_task(args) -> int:
    cfg = _cfg(args)
    task = task_file(cfg.repo)
    if (
        task.exists()
        and task_has_content(task.read_text(encoding="utf-8"))
        and not args.force
    ):
        print(f"{task} already has content (use --force to redraft)")
        return 1
    target = draft_task(cfg, args.description, agent_name=args.agent)
    print(f"drafted {target}\n")
    _print_task_summary(target.read_text(encoding="utf-8"))
    print("Review and edit it — the draft is a proposal, not a decision.")
    print("Open questions in it are yours to answer. Then: claudex run")
    return 0


def _print_task_summary(task_md: str) -> None:
    """Surface the mission in the terminal — the goal line decides what the
    whole pipeline does, so the human must see it without opening the file."""
    import re

    def section(name: str) -> str:
        m = re.search(rf"^# {name}\n+(.*?)(?=\n# |\Z)", task_md, re.S | re.M)
        return m.group(1).strip() if m else ""

    goal = " ".join(section("Goal").split())
    print(f"GOAL: {goal}\n")
    questions = [
        q.strip("- ").strip()
        for q in section("Open questions").splitlines()
        if q.strip().startswith("-")
    ]
    if questions:
        print(f"OPEN QUESTIONS ({len(questions)}) — answer these in the file:")
        for q in questions:
            print(f"  ? {q}")
        print()


def _print_economic_summary(cfg: Config, state: RunState) -> None:
    limits = budgets.budget_limits(cfg, state)
    print(
        "economic envelope: "
        f"{limits['invocations']} calls · {limits['input_tokens']} reported input · "
        f"{limits['output_tokens']} reported output · ${limits['cost_usd']:.2f} "
        f"reported USD · {limits['wall_seconds']}s wall time"
    )
    print(
        "per invocation: "
        f"{state.run_policy.get('agent_timeout', cfg.agent_timeout)}s · "
        f"{state.run_policy.get('max_invocation_output_tokens', cfg.max_invocation_output_tokens)} "
        "reported output · "
        f"{state.run_policy.get('max_invocation_tool_calls', cfg.max_invocation_tool_calls)} "
        "observable tools · "
        f"${state.run_policy.get('max_invocation_cost_usd', cfg.max_invocation_cost_usd):.2f} "
        "Claude native cap"
    )
    for profile in ("planning", "implementation", "verification"):
        effort = state.run_policy.get(f"{profile}_effort", getattr(cfg, f"{profile}_effort"))
        claude_model = state.run_policy.get(f"claude_{profile}_model") or state.run_policy.get("claude_model") or "default"
        codex_model = state.run_policy.get(f"codex_{profile}_model") or state.run_policy.get("codex_model") or "default"
        marker = " [EXPLICIT HIGH-COST OPT-IN]" if effort in budgets.EXPENSIVE_EFFORTS else ""
        print(
            f"profile {profile}: effort={effort}{marker} · "
            f"claude={claude_model} · codex={codex_model}"
        )
    nested = state.run_policy.get("disable_nested_agents", cfg.disable_nested_agents)
    print(f"nested provider agents: {'disabled' if nested else 'ENABLED (explicit opt-in)'}")


def _load_or_new_run(
    cfg: Config, driver: str, lead: str | None
) -> tuple[RunState, bool, dict | None]:
    """Return state/newness/prebuilt agents. A terminal previous run rolls off."""
    rid = get_current_run(cfg.repo)
    if rid:
        state = RunState.load(run_dir_for(cfg.repo, rid))
        if not state.is_terminal():
            return state, False, None
    lead = lead or cfg.lead
    if lead not in AGENTS:
        raise OrchestratorError(f"lead must be one of {AGENTS}, got {lead!r}")
    # Everything above is read-only. Do not mint a run ID, move the current
    # pointer, create state, launch watchers, or probe providers until the
    # repository can produce an exact base snapshot.
    validate_run_start(cfg)
    # Provider discovery/capability probing is bounded and non-billable. Do it
    # before allocating state so a missing/incompatible CLI cannot strand an
    # empty RUNNING identity or empty watcher windows.
    agents = build_agents(cfg)
    state = RunState(
        run_id=new_run_id(),
        repo=str(cfg.repo),
        lead=lead,
        driver=driver,
        run_policy=budgets.capture_run_policy(cfg),
        created_at=time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        started_epoch_s=time.time(),
    )
    state.record_lifecycle_start()
    set_current_run(cfg.repo, state.run_id)
    state.save(run_dir_for(cfg.repo, state.run_id))
    return state, True, agents


def _replacement_state(
    cfg: Config, old: RunState, *, fresh_plan: bool = False
) -> RunState:
    return recovery.clone_state(old, new_run_id(), fresh_plan=fresh_plan)


def cmd_run(args) -> int:
    cfg = _cfg(args)
    state, is_new, agents = _load_or_new_run(
        cfg, "headless", getattr(args, "lead", None)
    )
    if not is_new and state.driver == "live":
        print(
            f"run {state.run_id} is a live run (your session is the lead) — "
            "drive it with `claudex pair ...`, or `claudex cancel` it first"
        )
        return 1
    if is_new:
        print(
            f"new run {state.run_id}: lead={state.lead} (plans, implements), "
            f"pair={state.pair} (critiques, reviews, verifies)"
        )
        _print_economic_summary(cfg, state)
    else:
        print(f"resuming run {state.run_id} (phase: {state.phase})")
    orch = Orchestrator(cfg, state, agents=agents)
    if getattr(args, "open_terminals", False):
        open_watch_terminals(cfg.repo, state.run_id)
    return _drive_headless(cfg, orch)


def _run_headless(orch: Orchestrator) -> int:
    orch.run_until_gate()
    lifecycle = Lifecycle(orch.state.lifecycle)
    if lifecycle is Lifecycle.CANCELLED:
        return 130
    if lifecycle is Lifecycle.RATE_LIMITED:
        return 75
    return 0 if orch.state.phase != "failed" else 1


def _wait_until_rate_reset(
    run_dir: Path,
    reset_at: float,
    *,
    clock=time.time,
    sleeper=time.sleep,
) -> bool:
    """Wait without holding run.lock. False means cancellation was requested."""
    while clock() < reset_at:
        if (run_dir / "cancel.requested").exists():
            return False
        sleeper(min(0.25, max(0.0, reset_at - clock())))
    return not (run_dir / "cancel.requested").exists()


def _drive_headless(cfg: Config, orch: Orchestrator) -> int:
    """Run one durable pass; optional rate waiting occurs outside run.lock."""
    while True:
        result = _locked(cfg, orch, lambda: _run_headless(orch))
        if (
            Lifecycle(orch.state.lifecycle) is not Lifecycle.RATE_LIMITED
            or not cfg.wait_on_limits
        ):
            return result
        reset_at = orch.state.rate_limit_reset_at
        print(
            "[claudex] autonomous rate-limit wait enabled; run lock released "
            f"until approximately {time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(reset_at))}"
        )
        completed_wait = _wait_until_rate_reset(orch.run_dir, reset_at)

        def resume_after_wait() -> int:
            orch.state = RunState.load(orch.run_dir)
            if not completed_wait or (orch.run_dir / "cancel.requested").exists():
                orch.run_until_gate()
                return _run_headless(orch)
            if Lifecycle(orch.state.lifecycle) is Lifecycle.RATE_LIMITED:
                orch.state.resume_rate_limit("configured autonomous wait completed")
                orch.state.save(orch.run_dir)
            return _run_headless(orch)

        result = _locked(cfg, orch, resume_after_wait)
        if Lifecycle(orch.state.lifecycle) is not Lifecycle.RATE_LIMITED:
            return result


# ------------------------------------------------------------------ live mode
def _assert_live(orch: Orchestrator) -> None:
    if orch.state.driver != "live":
        raise OrchestratorError(
            f"run {orch.state.run_id} is headless — `claudex pair` subcommands "
            "only drive live runs (start one with `claudex pair start`)"
        )


def _assert_phase(orch: Orchestrator, *allowed: Phase) -> Phase:
    phase = Phase(orch.state.phase)
    if phase not in allowed:
        raise OrchestratorError(
            f"run is in phase {phase.value}, expected one of "
            f"{[p.value for p in allowed]} — `claudex status` shows what to do next"
        )
    return phase


def cmd_pair_start(args) -> int:
    cfg = _cfg(args)
    rid = get_current_run(cfg.repo)
    if rid:
        prev = RunState.load(run_dir_for(cfg.repo, rid))
        if not prev.is_terminal():
            print(f"run {rid} is still {prev.phase} — finish or `claudex cancel` it first")
            return 1
    # In live mode YOU are the lead; --lead names which agent you are so the
    # remaining one becomes the pair.
    state, _, agents = _load_or_new_run(cfg, "live", getattr(args, "lead", None))
    orch = Orchestrator(cfg, state, agents=agents)

    def go() -> int:
        orch.phase_init()
        orch.state.save(orch.run_dir)
        print(f"live run {state.run_id}: you are the LEAD ({state.lead}); "
              f"pair={state.pair}")
        _print_economic_summary(cfg, state)
        print(f"run dir: {orch.run_dir}")
        if orch.report_mode:
            print(f"worktree: {state.worktree}")
            print("report mode — write and COMMIT the report in the worktree,")
            print("then: claudex pair checkpoint")
        else:
            print("next: draft your plan as JSON (plan_markdown, steps[{title,")
            print("description, files, tests}], risks, open_questions), then:")
            print(f"  claudex pair plan --file <your-plan.json>")
        return 0

    return _locked(cfg, orch, go)


def _next_live_hint(orch: Orchestrator) -> None:
    phase = Phase(orch.state.phase)
    s = orch.state
    if phase is Phase.PLAN_REVISE:
        print(
            "pair wants revisions — read the critique above, revise your plan "
            "JSON (full re-emit), then: claudex pair plan --file <revised.json>"
        )
    elif phase is Phase.IMPLEMENT_STEP:
        step = s.steps[s.step_index]
        print(f"plan agreed. worktree: {s.worktree}")
        print(
            f"implement step {s.step_index + 1}/{len(s.steps)} "
            f"({step.get('title', '')}), COMMIT it, then: claudex pair checkpoint"
        )
    elif phase is Phase.FIX:
        print(
            f"fix the blocking/major findings in {s.findings_file}, COMMIT, then: "
            + (
                "claudex pair checkpoint"
                if s.fix_return == Phase.CHECKPOINT.value
                else "claudex pair verify"
            )
        )
    elif phase is Phase.TESTS:
        print("all steps agreed — next: claudex pair verify")
    elif phase is Phase.AWAIT_GUIDANCE:
        print(f"GATE: {s.gate_reason}")
        if s.gate_kind == "decision":
            print(
                "bring this to the human, then: claudex resolve --notes '...' "
                "(or --notes-file PATH)"
            )
        else:
            print("quality budget exhausted: claudex continue")
    elif phase is Phase.PAUSED_BUDGET:
        print(f"run budget paused: {s.gate_reason}")
        print("resume with: claudex resume --add-invocations N [other additions]")
    elif phase is Phase.DONE:
        print(f"DONE — merge with: git merge {s.branch}")
        used = budgets.budget_usage(s)
        print(
            f"usage: {used['invocations']} invocation(s), "
            f"{used['input_tokens']} reported input, "
            f"{used['output_tokens']} reported output, "
            f"${used['cost_usd']:.4f} reported USD, "
            f"{s.unknown_cost_attempts} unknown-cost attempt(s)"
        )


def cmd_pair_plan(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    _assert_live(orch)
    _assert_phase(orch, Phase.PLAN_DRAFT, Phase.PLAN_REVISE)
    plan_file = Path(args.file)
    if not plan_file.exists():
        raise OrchestratorError(f"plan file not found: {plan_file}")
    try:
        plan = json.loads(plan_file.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise OrchestratorError(f"plan file is not valid JSON: {exc}")
    validate_plan_shape(plan)

    def go() -> int:
        orch.submit_plan(plan)
        critique = orch.pair_plan_turn()
        print(f"\npair verdict: {critique.get('verdict', '?')}")
        for f in critique.get("findings", []):
            print(f"  [{f.get('severity', '?')}] {f.get('problem', '')}")
        for item in critique.get("implementation_checks", []):
            print(
                f"  [implementation:{item.get('action', 'add')}] "
                f"{item.get('key', '?')}: {item.get('description', '')}"
            )
        _next_live_hint(orch)
        return 0

    return _locked(cfg, orch, go)


def cmd_pair_checkpoint(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    _assert_live(orch)
    phase = _assert_phase(orch, Phase.IMPLEMENT_STEP, Phase.CHECKPOINT, Phase.FIX)
    if phase is Phase.FIX and orch.state.fix_return != Phase.CHECKPOINT.value:
        raise OrchestratorError(
            f"open findings came from {orch.state.fix_return}; after fixing, "
            "run `claudex pair verify` instead"
        )

    def go() -> int:
        if phase is Phase.FIX:
            orch.record_fix_completed()
        review = orch.pair_checkpoint_turn(lead_notes=args.notes or "")
        print(f"\npair verdict: {review.get('verdict', '?')}")
        for f in review.get("findings", []):
            loc = f.get("file") or ""
            loc = f" {loc}:{f['line']}" if loc and f.get("line") else (f" {loc}" if loc else "")
            print(f"  [{f.get('severity', '?')}]{loc} {f.get('problem', '')}")
        _next_live_hint(orch)
        return 0

    return _locked(cfg, orch, go)


def cmd_pair_verify(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    _assert_live(orch)
    phase = _assert_phase(orch, Phase.TESTS, Phase.VERIFY, Phase.FIX)
    if phase is Phase.FIX and orch.state.fix_return == Phase.CHECKPOINT.value:
        raise OrchestratorError(
            "open findings came from a checkpoint review; after fixing, "
            "run `claudex pair checkpoint` instead"
        )

    def go() -> int:
        if Phase(orch.state.phase) is Phase.FIX:
            # Fixes for test/verify findings re-enter their gate.
            orch.record_fix_completed()
            orch.state.advance(Phase(orch.state.fix_return))
        if Phase(orch.state.phase) is Phase.TESTS:
            if not orch.run_test_gate():
                print("test gate FAILED — see findings above")
                _next_live_hint(orch)
                return 1
        verdict = orch.pair_verify_turn()
        print(f"\nverification: {verdict.get('verdict', '?')}")
        for c in verdict.get("criteria", []):
            print(f"  {'PASS' if c.get('met') else 'FAIL'} {c.get('criterion', '')}")
        _next_live_hint(orch)
        return 0 if verdict.get("verdict") == "pass" else 1

    return _locked(cfg, orch, go)


def cmd_resolve(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    notes = _read_guidance_notes(args)

    def go() -> int:
        orch.resolve_guidance(notes)
        print(f"guidance recorded; run returns to {orch.state.phase}")
        if orch.state.driver == "live":
            _next_live_hint(orch)
        else:
            orch.run_until_gate()
        return 0

    return _locked(cfg, orch, go)


def _read_guidance_notes(args) -> str:
    """Read a decision without forcing multiline text through shell quotes.

    ``--notes-file -`` accepts redirected stdin; an interactive terminal is
    intentionally not prompted because its EOF keystroke is easy to miss on
    Windows and recreates the apparent "command will not close" failure.
    """
    if args.notes is not None:
        return args.notes
    source = args.notes_file
    if source == "-":
        if sys.stdin.isatty():
            raise OrchestratorError(
                "--notes-file - requires redirected stdin; use --notes-file PATH "
                "for multiline guidance"
            )
        return sys.stdin.read()
    try:
        return Path(source).read_text(encoding="utf-8")
    except OSError as exc:
        raise OrchestratorError(f"could not read guidance file {source}: {exc}") from exc


def cmd_continue(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)

    def go() -> int:
        if Phase(orch.state.phase) is not Phase.AWAIT_GUIDANCE:
            if not orch.resume_interrupted_continue():
                raise OrchestratorError(
                    f"run is in phase {orch.state.phase}, not awaiting guidance; "
                    "use `claudex run` to resume it"
                )
            print(f"interrupted continuation resumed at {orch.state.phase}")
            orch.run_until_gate()
            return 0 if orch.state.phase != Phase.FAILED.value else 1
        extended = orch.continue_after_budget(args.responses)
        action = (
            f"quality budget extended by {args.responses} response(s)"
            if extended
            else "incomplete cycle resumed"
        )
        print(f"{action}; run returns to {orch.state.phase}")
        if orch.state.driver == "live":
            _next_live_hint(orch)
        else:
            orch.run_until_gate()
        return 0

    return _locked(cfg, orch, go)


def cmd_resume(args) -> int:
    """Continue the same durable run; budget pauses require additions."""
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    additions = {
        "invocations": args.add_invocations,
        "input_tokens": args.add_input_tokens,
        "output_tokens": args.add_output_tokens,
        "cost_usd": args.add_cost_usd,
        "tool_calls": args.add_tool_calls,
        "wall_seconds": args.add_wall_seconds,
        "invocation_output_tokens": args.add_invocation_output_tokens,
        "invocation_tool_calls": args.add_invocation_tool_calls,
    }

    def go() -> int:
        has_override = any(value != 0 for value in additions.values()) or bool(
            args.acknowledge_currency
        )
        phase = Phase(orch.state.phase)
        lifecycle = Lifecycle(orch.state.lifecycle)
        if phase is Phase.PAUSED_BUDGET:
            orch.resume_budget(additions, args.acknowledge_currency)
            print(f"run budget expanded; run returns to {orch.state.phase}")
        elif has_override:
            raise OrchestratorError(
                "budget additions are accepted only while PAUSED_BUDGET"
            )
        elif lifecycle is Lifecycle.FAILED_RETRYABLE:
            orch.state.retry()
            orch.state.save(orch.run_dir)
            print(f"retryable failure resumed at {orch.state.phase}")
        elif lifecycle is Lifecycle.RATE_LIMITED:
            orch.state.resume_rate_limit("operator resumed rate-limited run")
            orch.state.save(orch.run_dir)
            print(f"rate-limited run resumed at {orch.state.phase}")
        elif lifecycle is Lifecycle.RUNNING:
            print(f"resuming run {orch.state.run_id} at {orch.state.phase}")
        elif lifecycle is Lifecycle.PAUSED:
            raise OrchestratorError(
                "run is paused for guidance/quality policy; use claudex resolve "
                "or claudex continue as shown by claudex status"
            )
        else:
            raise OrchestratorError(
                f"run lifecycle {lifecycle.value} is not resumable"
            )
        if orch.state.driver == "live":
            _next_live_hint(orch)
        else:
            orch.run_until_gate()
        return 0 if orch.state.phase != Phase.FAILED.value else 1

    return _locked(cfg, orch, go)


def cmd_restart(args) -> int:
    """Create a replacement identity from the latest durable checkpoint."""
    cfg = _cfg(args)
    old = _active_orchestrator(cfg)
    lifecycle = Lifecycle(old.state.lifecycle)
    if old.state.driver != "headless":
        raise OrchestratorError(
            "restart currently requires a headless run"
        )
    if lifecycle in (
        Lifecycle.COMPLETED,
        Lifecycle.CANCELLED,
        Lifecycle.FAILED_TERMINAL,
    ):
        raise OrchestratorError(f"cannot restart terminal lifecycle {lifecycle.value}")
    if args.fresh_plan and old.state.worktree:
        raise OrchestratorError(
            "--fresh-plan would discard an implementation checkpoint; cancel/clean "
            "or restart without --fresh-plan"
        )
    acquire_run_lock(old.run_dir)
    try:
        old_state_backup = old.run_dir / "state.pre-restart.json"
        if not old_state_backup.exists():
            backup_tmp = old_state_backup.with_suffix(
                old_state_backup.suffix + ".tmp"
            )
            backup_tmp.write_bytes((old.run_dir / "state.json").read_bytes())
            backup_tmp.replace(old_state_backup)
        before = recovery.checkpoint_digest(old.state, old.run_dir)
        state = _replacement_state(cfg, old.state, fresh_plan=args.fresh_plan)
        new_dir = run_dir_for(cfg.repo, state.run_id)
        recovery.copy_checkpoint_artifacts(old.run_dir, new_dir)
        if args.fresh_plan:
            recovery.discard_plan_artifacts(new_dir)
            if not (new_dir / "task.md").exists():
                state.phase = Phase.INIT.value
        recovery.remap_run_paths(state, old.run_dir, new_dir)
        if not args.fresh_plan:
            after = recovery.checkpoint_digest(state, new_dir)
            if after != before:
                raise OrchestratorError(
                    f"restart checkpoint mismatch: {before} != {after}; old run retained"
                )
        state.save(new_dir)
        old.retire()
        if state.lifecycle == Lifecycle.FAILED_RETRYABLE.value:
            state.retry()
            state.save(new_dir)
        set_current_run(cfg.repo, state.run_id)
    finally:
        release_run_lock(old.run_dir)
    print(
        f"retired {old.state.run_id}; restarting as {state.run_id} from "
        + ("an explicit fresh plan" if args.fresh_plan else "the canonical checkpoint")
        + f" with {len(state.binding_guidance)} preserved decision(s)"
    )
    orch = Orchestrator(cfg, state)
    return _drive_headless(cfg, orch)


# ---------------------------------------------------------------- inspection
def cmd_status(args) -> int:
    cfg = _cfg(args, persist_migration=False)
    rid = getattr(args, "run_id", None) or get_current_run(cfg.repo)
    if not rid:
        print("no active run")
        return 0
    state = RunState.load(run_dir_for(cfg.repo, rid), persist_migration=False)
    rd = run_dir_for(cfg.repo, rid)
    print(f"run:      {rid}   ({state.driver})")
    print(f"phase:    {state.phase}   mode: {state.mode}")
    print(f"lifecycle:{state.lifecycle}")
    print(f"elapsed:  {budgets.run_wall_seconds(state):.1f}s")
    print(f"lead:     {state.lead}   pair: {state.pair}")
    protocol = (
        "compact-plan v2+"
        if state.plan_protocol_version >= 2
        else "legacy exhaustive-plan v1 (restart required while planning)"
    )
    print(f"protocol: {protocol}")
    if state.canonical_plan_sha256:
        print(
            f"canonical: plan round {state.canonical_plan_round} · "
            f"sha256 {state.canonical_plan_sha256}"
        )
    if state.steps:
        print(f"step:     {min(state.step_index + 1, len(state.steps))}/{len(state.steps)}")
    if state.implementation_checks:
        print(f"checks:   {len(state.implementation_checks)} implementation obligation(s)")
    plan_files = []
    for path in rd.glob("plan-round-*.json"):
        try:
            plan_files.append((int(path.stem.rsplit("-", 1)[1]), path))
        except (IndexError, ValueError):
            continue
    if plan_files:
        plan_no, plan_path = max(plan_files, key=lambda item: item[0])
        print(
            f"plan size: {plan_path.stat().st_size / 1024:.1f} KiB "
            f"(round {plan_no}, diagnostic only)"
        )
    print(
        f"attempts: plan critiques {state.plan_round}"
        f" · checkpoint reviews {state.checkpoint_round}"
        f" · tests {state.test_round}"
        f" · verify {state.verify_round}"
    )
    print(
        f"responses: plan revisions {state.plan_revisions}/"
        f"{cfg.max_plan_rounds + state.plan_cap_extra}"
        f" · checkpoint fixes {state.checkpoint_fixes_used}/"
        f"{cfg.max_checkpoint_rounds + state.checkpoint_cap_extra}"
        f" · test fixes {state.test_fixes_used}/"
        f"{cfg.max_test_rounds + state.test_cap_extra}"
        f" · verify fixes {state.verify_fixes_used}/"
        f"{cfg.max_verify_rounds + state.verify_cap_extra}"
    )
    limits = budgets.budget_limits(cfg, state)
    used = budgets.budget_usage(state)
    remaining = budgets.remaining_budgets(cfg, state)
    print(
        "provider: "
        f"calls {used['invocations']}/{limits['invocations']} "
        f"(remaining {remaining['invocations']})"
        f" · input {used['input_tokens']}/{limits['input_tokens']} reported"
        f" (remaining {remaining['input_tokens']})"
        f" · output {used['output_tokens']}/{limits['output_tokens']} reported"
        f" (remaining {remaining['output_tokens']})"
    )
    usage = state.provider_usage
    print(
        "tokens:   "
        f"input uncached={usage.get('input_tokens', 0)}, "
        f"cached={usage.get('cached_input_tokens', 0)}, "
        f"cache-create={usage.get('cache_creation_input_tokens', 0)}"
        f" · output={usage.get('output_tokens', 0)}, "
        f"reasoning={usage.get('reasoning_output_tokens', 0)}"
    )
    print(
        "spend:    "
        f"${used['cost_usd']:.4f}/${limits['cost_usd']:.4f} provider-reported USD "
        f"(remaining ${remaining['cost_usd']:.4f})"
        f" · {state.unknown_cost_attempts} unknown-cost attempt(s)"
    )
    foreign = {
        key: value for key, value in state.provider_costs.items() if key != "USD"
    }
    if foreign:
        rendered = ", ".join(f"{value:.4f} {key}" for key, value in foreign.items())
        print(f"currency: separately reported, not converted: {rendered}")
    print(
        "activity: "
        f"tools {used['tool_calls']}/{limits['tool_calls']} where observable"
        f" (remaining {remaining['tool_calls']}; "
        f"{state.unknown_tool_attempts} attempt(s) unknown)"
        f" · provider time {used['provider_seconds']:.1f}s"
        f" · run wall time {used['wall_seconds']:.1f}/{limits['wall_seconds']}s"
        f" (remaining {remaining['wall_seconds']:.1f}s)"
    )
    invocation_output_limit = state.run_policy.get(
        "max_invocation_output_tokens", cfg.max_invocation_output_tokens
    ) + state.budget_overrides.get("invocation_output_tokens", 0)
    invocation_tool_limit = state.run_policy.get(
        "max_invocation_tool_calls", cfg.max_invocation_tool_calls
    ) + state.budget_overrides.get("invocation_tool_calls", 0)
    print(
        "last call: "
        f"output {budgets.total_reported_output(state.last_attempt_usage)}/"
        f"{invocation_output_limit} reported"
        f" · tools {state.last_attempt_tool_calls}/{invocation_tool_limit} observable"
    )
    if state.unknown_usage_attempts:
        print(f"usage:    {state.unknown_usage_attempts} attempt(s) did not report token usage")
    if state.active_attempt_id:
        print(f"active:   {state.active_attempt_id}")
    if state.last_attempt_id:
        print(f"last:     {state.last_attempt_id}")
    if state.last_invocation_policy:
        policy = state.last_invocation_policy
        effort = policy.get("effort", "?")
        requested = policy.get("requested_effort", effort)
        if requested != effort:
            effort = f"{effort} (requested {requested})"
        print(
            "policy:   "
            f"{policy.get('profile', '?')} · effort={effort}"
            f" · model={policy.get('model') or 'provider default'}"
            f" · turns={policy.get('max_turns', '?')}"
            f" · timeout={policy.get('timeout_seconds', '?')}s"
            f" · nested agents={'disabled' if policy.get('disable_nested_agents') else 'allowed'}"
        )
    if state.branch:
        print(f"branch:   {state.branch}")
    if state.worktree:
        print(f"worktree: {state.worktree}")
    if state.gate_reason:
        kind = state.gate_kind or "unknown"
        print(f"gate:     [{kind}] {state.gate_reason}")
    if Lifecycle(state.lifecycle) is Lifecycle.RATE_LIMITED:
        reset = time.strftime(
            "%Y-%m-%d %H:%M:%S", time.localtime(state.rate_limit_reset_at)
        )
        print(
            f"limit:    {state.rate_limit_agent}/{state.rate_limit_label} · "
            f"reset approximately {reset} · next: claudex resume"
        )
    if state.lifecycle_history:
        transition = state.lifecycle_history[-1]
        print(
            f"resume:   {transition.get('resume_instruction', '')}"
            f" · {transition.get('reason', '')}"
        )
    if state.error:
        print(f"error:    {state.error}")
    reason = terminal_reason(state)
    if reason and reason != state.error:
        print(f"reason:   {reason}")
    print(f"next:     {next_action(state)}")
    print(f"run dir:  {rd}")
    mb = rd / "mailbox.md"
    if mb.exists():
        print(f"mailbox:  {mb}")
    arts = sorted(
        p.name
        for p in rd.glob("*")
        if p.is_file() and p.name not in ("state.json", "run.lock")
    )
    if arts:
        print("artifacts:")
        for a in arts:
            print(f"  - {a}")
    print("recent events:")
    for e in state.events[-8:]:
        print(f"  {e['ts']}  {e['message']}")
    return 0


def _watcher_argv(repo: Path, run_id: str, agent: str) -> list[str]:
    return [
        sys.executable,
        "-m",
        "claudex",
        "watch",
        run_id,
        "--agent",
        agent,
        "--repo",
        str(repo),
    ]


def open_watch_terminals(
    repo: Path,
    run_id: str,
    *,
    platform_name: str | None = None,
    which=shutil.which,
    popen=subprocess.Popen,
) -> bool:
    """Open two Windows Terminal views. Watchers never own run state."""
    platform_name = os.name if platform_name is None else platform_name
    watcher_commands = [
        _watcher_argv(repo, run_id, "claude"),
        _watcher_argv(repo, run_id, "codex"),
    ]
    terminal = which("wt.exe") if platform_name == "nt" else None
    if not terminal:
        print("Windows Terminal is unavailable; open these watcher commands:")
        for command in watcher_commands:
            print("  " + (subprocess.list2cmdline(command) if platform_name == "nt" else shlex.join(command)))
        return False
    try:
        for agent, command in zip(("Claude", "Codex"), watcher_commands):
            popen(
                [
                    terminal,
                    "-w",
                    "new",
                    "new-tab",
                    "--title",
                    f"Claudex · {agent} · {run_id}",
                    *command,
                ],
                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
                close_fds=True,
            )
        print(
            "opened Claude and Codex watcher windows; closing a watcher does "
            "not cancel or mutate the run"
        )
        return True
    except OSError as exc:
        print(f"could not open Windows Terminal ({exc}); use these watcher commands:")
        for command in watcher_commands:
            print("  " + subprocess.list2cmdline(command))
        return False


def cmd_watch(args) -> int:
    cfg = _cfg(args, persist_migration=False)
    rid = args.run_id or get_current_run(cfg.repo)
    if not rid:
        raise OrchestratorError("no run to watch; pass RUN_ID or start a run")
    try:
        return watch_run(
            run_dir_for(cfg.repo, rid),
            agent=args.agent,
            follow=args.follow,
            raw=args.raw,
            color=args.color,
        )
    except KeyboardInterrupt:
        return 130


def cmd_retry(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)

    def go() -> int:
        try:
            orch.state.retry()
        except ValueError as exc:
            print(str(exc))
            return 1
        orch.state.save(orch.run_dir)
        if orch.state.driver == "live":
            print(f"rewound to {orch.state.phase}")
            _next_live_hint(orch)
            return 0
        orch.run_until_gate()
        return 0

    return _locked(cfg, orch, go)


def cmd_cancel(args) -> int:
    """Request cancellation without waiting for the active run lock."""
    cfg = _cfg(args)
    rid = get_current_run(cfg.repo)
    if not rid:
        print("no active run")
        return 0
    run_dir = run_dir_for(cfg.repo, rid)
    state = RunState.load(run_dir)
    if state.is_terminal() or Lifecycle(state.lifecycle) is Lifecycle.CANCELLED:
        print(f"run {rid} is already terminal ({state.lifecycle})")
        return 0
    request = run_dir / "cancel.requested"
    if not request.exists():
        tmp = request.with_suffix(".requested.tmp")
        tmp.write_text(
            json.dumps(
                {
                    "requested_at": time.time(),
                    "pid": os.getpid(),
                    "reason": args.reason,
                },
                indent=2,
            ),
            encoding="utf-8",
        )
        tmp.replace(request)
    if state.active_attempt_id:
        attempt = run_dir / "attempts" / state.active_attempt_id
        (attempt / "cancel.requested").touch(exist_ok=True)
        control = attempt / "control.json"
        if control.exists():
            try:
                value = json.loads(control.read_text(encoding="utf-8"))
                pid = int(value.get("pid", 0) or 0)
                if pid > 0:
                    terminate_process_tree(pid, str(value.get("job_name", "")))
            except (OSError, ValueError, json.JSONDecodeError):
                # The running coordinator also polls the durable request.
                pass
        print(
            f"cancellation requested for run {rid}, attempt {state.active_attempt_id}; "
            "partial artifacts are retained"
        )
        return 0

    acquire_run_lock(run_dir)
    try:
        state = RunState.load(run_dir)
        if Lifecycle(state.lifecycle) is not Lifecycle.CANCELLED:
            state.cancel(args.reason)
            state.save(run_dir)
    finally:
        release_run_lock(run_dir)
    print(f"cancelled idle run {rid}; partial artifacts are retained")
    return 0


def cmd_clean(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    if not orch.state.is_terminal():
        print(f"run {orch.state.run_id} is still {orch.state.phase}; cancel it first")
        return 1

    def go() -> int:
        orch.clean()
        clear_current_run(cfg.repo)
        return 0

    return _locked(cfg, orch, go)


def cmd_export(args) -> int:
    cfg = _cfg(args)
    rid = args.run_id or get_current_run(cfg.repo)
    if not rid:
        raise OrchestratorError("no run to export; pass RUN_ID or start a run")
    output = recovery.export_run(run_dir_for(cfg.repo, rid), Path(args.output))
    print(f"exported compact recovery bundle: {output}")
    print("raw stdout/stderr/last-message streams were intentionally excluded")
    return 0


def cmd_doctor(args) -> int:
    cfg = _cfg(args)
    ok = True

    def check(name: str, fn):
        nonlocal ok
        try:
            print(f"  {name}: {fn()}")
        except Exception as exc:  # noqa: BLE001 - diagnostic surface
            print(f"  {name}: FAIL — {exc}")
            ok = False

    def provider_report(name: str, configured: str) -> str:
        info = resolve_provider(name, configured or None)
        caps = ", ".join(
            f"{key}={'yes' if value else ('coordinator-only' if key == 'budget' else 'no')}"
            for key, value in info.capabilities.items()
        )
        return f"{info.version} [{info.path}; {info.source}] · {caps}"

    print("claudex doctor:")
    check("git", lambda: gitops.git(cfg.repo, "--version", check=True) or "ok")
    check(
        "git repo",
        lambda: "yes" if gitops.is_git_repo(cfg.repo) else (_ for _ in ()).throw(
            RuntimeError(f"{cfg.repo} is not a git repository")
        ),
    )
    check("claude", lambda: provider_report("claude", cfg.claude_bin))
    check("codex", lambda: provider_report("codex", cfg.codex_bin))
    check("task.md", lambda: str(task_file(cfg.repo)) if task_file(cfg.repo).exists() else "missing (run `claudex init`)")
    print("note: doctor checks wiring only; model/account issues surface on first run")
    return 0 if ok else 1


# --------------------------------------------------------------------- parser
def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="claudex",
        description="Pair-programming orchestrator for Claude Code and OpenAI Codex",
    )
    p.add_argument("--version", action="version", version=f"claudex {__version__}")
    sub = p.add_subparsers(dest="command", required=True)

    def common(sp):
        sp.add_argument("--repo", default=".", help="target repository (default: cwd)")

    sp = sub.add_parser("init", help="scaffold .claudex/task.md and config")
    common(sp)
    sp.add_argument("--force", action="store_true", help="overwrite existing task.md")
    sp.add_argument("--with-skill", dest="with_skill", action="store_true",
                    help="also install the live-mode pairing skill into .claude/skills/")
    sp.set_defaults(func=cmd_init)

    sp = sub.add_parser(
        "task", help="draft the task contract from a one-paragraph description"
    )
    common(sp)
    sp.add_argument("description", help="one-paragraph task description")
    sp.add_argument("--agent", choices=["claude", "codex"], default="claude",
                    help="which agent drafts the contract (default: claude)")
    sp.add_argument("--force", action="store_true", help="overwrite a filled task.md")
    sp.set_defaults(func=cmd_task)

    def run_flags(sp):
        sp.add_argument("--lead", choices=["claude", "codex"], default=None,
                        help="who holds the pen (default: config, claude)")
        sp.add_argument("--mode", choices=["auto", "report", "change"], default=None,
                        help="pipeline shape: report (deliverable is analysis) or "
                             "change (deliverable is a diff); auto detects from the "
                             "goal's [REPORT]/[CHANGE] prefix")
        sp.add_argument("--test-command", dest="test_command", default=None,
                        help="mechanical test gate command, run by the coordinator")
        sp.add_argument("--max-plan-rounds", dest="max_plan_rounds", type=int, default=None)
        sp.add_argument("--max-checkpoint-rounds", dest="max_checkpoint_rounds", type=int, default=None)
        sp.add_argument("--timeout", dest="agent_timeout", type=int, default=None,
                        help="seconds per agent invocation")
        sp.add_argument("--claude-model", dest="claude_model", default=None)
        sp.add_argument("--codex-model", dest="codex_model", default=None)
        sp.add_argument("--max-invocation-cost-usd", type=float, default=None)
        sp.add_argument("--max-invocation-turns", type=int, default=None)
        sp.add_argument("--max-invocation-output-tokens", type=int, default=None)
        sp.add_argument("--max-invocation-tool-calls", type=int, default=None)
        sp.add_argument("--max-run-invocations", type=int, default=None)
        sp.add_argument("--max-run-input-tokens", type=int, default=None)
        sp.add_argument("--max-run-output-tokens", type=int, default=None)
        sp.add_argument("--max-run-cost-usd", type=float, default=None)
        sp.add_argument("--max-run-tool-calls", type=int, default=None)
        sp.add_argument("--max-run-wall-seconds", type=int, default=None)
        effort = ("low", "medium", "high", "xhigh", "max")
        sp.add_argument("--planning-effort", choices=effort, default=None)
        sp.add_argument("--implementation-effort", choices=effort, default=None)
        sp.add_argument("--verification-effort", choices=effort, default=None)
        sp.add_argument("--claude-planning-model", default=None)
        sp.add_argument("--claude-implementation-model", default=None)
        sp.add_argument("--claude-verification-model", default=None)
        sp.add_argument("--codex-planning-model", default=None)
        sp.add_argument("--codex-implementation-model", default=None)
        sp.add_argument("--codex-verification-model", default=None)
        sp.add_argument("--max-evidence-bytes", type=int, default=None)
        sp.add_argument("--max-evidence-file-bytes", type=int, default=None)
        sp.add_argument("--max-evidence-requests", type=int, default=None)
        sp.add_argument(
            "--allow-expensive-profiles",
            action="store_true",
            default=None,
            help="explicitly permit maximum-cost effort profiles",
        )
        nested = sp.add_mutually_exclusive_group()
        nested.add_argument(
            "--allow-nested-agents",
            dest="disable_nested_agents",
            action="store_false",
            default=None,
            help="opt in to provider sub-agent features",
        )
        nested.add_argument(
            "--disable-nested-agents",
            dest="disable_nested_agents",
            action="store_true",
            default=None,
            help="explicitly retain the safe default",
        )

    sp = sub.add_parser("run", help="start or resume a headless pair run")
    common(sp)
    run_flags(sp)
    sp.add_argument(
        "--open-terminals",
        action="store_true",
        help="open separate Windows Terminal watcher windows for Claude and Codex",
    )
    sp.set_defaults(func=cmd_run)

    pair = sub.add_parser("pair", help="live pairing: your session is the lead")
    pair_sub = pair.add_subparsers(dest="pair_command", required=True)

    sp = pair_sub.add_parser("start", help="start a live run (you are the lead)")
    common(sp)
    run_flags(sp)
    sp.set_defaults(func=cmd_pair_start)

    sp = pair_sub.add_parser("plan", help="submit your plan JSON for pair critique")
    common(sp)
    sp.add_argument("--file", required=True, help="path to your plan JSON")
    sp.set_defaults(func=cmd_pair_plan)

    sp = pair_sub.add_parser("checkpoint", help="request pair review of your commits")
    common(sp)
    sp.add_argument("--notes", default="", help="context for the pair, recorded in the mailbox")
    sp.set_defaults(func=cmd_pair_checkpoint)

    sp = pair_sub.add_parser("verify", help="run the test gate and fresh verification")
    common(sp)
    sp.set_defaults(func=cmd_pair_verify)

    sp = sub.add_parser("resolve", help="answer a concrete human-decision gate")
    common(sp)
    guidance = sp.add_mutually_exclusive_group(required=True)
    guidance.add_argument(
        "--notes", help="your decision — becomes persistent binding guidance"
    )
    guidance.add_argument(
        "--notes-file",
        metavar="PATH",
        help="read the decision from a UTF-8 file; use - for redirected stdin",
    )
    sp.set_defaults(func=cmd_resolve)

    sp = sub.add_parser(
        "continue", help="allow another response and fresh audit without adding guidance"
    )
    common(sp)
    sp.add_argument(
        "--responses",
        type=int,
        default=1,
        help="additional responses before the next fresh audit (default: 1)",
    )
    sp.set_defaults(func=cmd_continue)

    sp = sub.add_parser(
        "resume", help="continue the same durable run; add capacity for budget pauses"
    )
    common(sp)
    sp.add_argument("--add-invocations", type=int, default=0)
    sp.add_argument("--add-input-tokens", type=int, default=0)
    sp.add_argument("--add-output-tokens", type=int, default=0)
    sp.add_argument("--add-cost-usd", type=float, default=0.0)
    sp.add_argument("--add-tool-calls", type=int, default=0)
    sp.add_argument("--add-wall-seconds", type=int, default=0)
    sp.add_argument("--add-invocation-output-tokens", type=int, default=0)
    sp.add_argument("--add-invocation-tool-calls", type=int, default=0)
    sp.add_argument(
        "--acknowledge-currency",
        action="append",
        default=[],
        metavar="CODE",
        help="resume without converting a separately reported non-USD cost",
    )
    sp.set_defaults(func=cmd_resume)

    sp = sub.add_parser(
        "restart",
        help="replace execution identity while preserving the canonical checkpoint",
    )
    common(sp)
    sp.add_argument(
        "--fresh-plan",
        action="store_true",
        help="explicitly discard planning artifacts while preserving decisions/budgets",
    )
    sp.set_defaults(func=cmd_restart)

    sp = sub.add_parser("status", help="show run state and artifacts")
    common(sp)
    sp.add_argument("run_id", nargs="?", help="run to inspect (default: current)")
    sp.set_defaults(func=cmd_status)

    sp = sub.add_parser("watch", help="replay and tail normalized run events")
    common(sp)
    sp.add_argument("run_id", nargs="?", help="run to watch (default: current)")
    sp.add_argument("--agent", choices=["claude", "codex", "all"], default="all")
    follow = sp.add_mutually_exclusive_group()
    follow.add_argument("--follow", dest="follow", action="store_true", default=True)
    follow.add_argument("--no-follow", dest="follow", action="store_false")
    format_group = sp.add_mutually_exclusive_group()
    format_group.add_argument("--raw", action="store_true", help="emit stable JSONL")
    format_group.add_argument(
        "--readable", dest="raw", action="store_false", help="emit labeled text"
    )
    sp.set_defaults(raw=False)
    sp.add_argument("--color", choices=["auto", "always", "never"], default="auto")
    sp.set_defaults(func=cmd_watch)

    sp = sub.add_parser("retry", help="re-attempt the failed phase")
    common(sp)
    sp.set_defaults(func=cmd_retry)

    sp = sub.add_parser(
        "cancel", help="idempotently cancel an active provider/test process tree"
    )
    common(sp)
    sp.add_argument("--reason", default="operator cancelled run")
    sp.set_defaults(func=cmd_cancel)

    sp = sub.add_parser("clean", help="remove the finished run's worktree")
    common(sp)
    sp.set_defaults(func=cmd_clean)

    sp = sub.add_parser("export", help="export a compact redacted recovery bundle")
    common(sp)
    sp.add_argument("run_id", nargs="?", help="run to export (default: current)")
    sp.add_argument("--output", required=True, help="destination .zip path")
    sp.set_defaults(func=cmd_export)

    sp = sub.add_parser("doctor", help="check git/claude/codex wiring")
    common(sp)
    sp.set_defaults(func=cmd_doctor)

    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except (
        OrchestratorError,
        AgentError,
        gitops.GitError,
        ProviderError,
        RunLockError,
        ValueError,
        json.JSONDecodeError,
    ) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
