"""claudex command-line interface.

    claudex init                    scaffold .claudex/task.md + config
    claudex run                     start or resume a run (executes until a gate)
    claudex status                  show run state and artifacts
    claudex approve plan <agent>    resolve gate 1 and continue
    claudex approve final           resolve gate 2 (marks DONE)
    claudex retry                   re-attempt the phase that failed
    claudex abort                   abort the active run
    claudex clean                   remove the run's worktree
    claudex doctor                  check git/claude/codex wiring
"""

from __future__ import annotations

import argparse
import subprocess
import sys
from importlib import resources
from pathlib import Path

from . import __version__, gitops
from .agents import AgentError, resolve_claude_bin, resolve_codex_bin
from .config import Config
from .phases import Orchestrator, OrchestratorError, draft_task, task_has_content
from .state import (
    RunState,
    clear_current_run,
    get_current_run,
    new_run_id,
    next_owner_auto,
    run_dir_for,
    set_current_run,
    task_file,
)


def _load_template() -> str:
    return (
        resources.files("claudex")
        .joinpath("templates/task.md")
        .read_text(encoding="utf-8")
    )


def _cfg(args) -> Config:
    overrides = {
        k: getattr(args, k, None)
        for k in (
            "owner",
            "mode",
            "auto_plan",
            "max_review_rounds",
            "agent_timeout",
            "claude_model",
            "codex_model",
        )
    }
    return Config.load(Path(args.repo), overrides)


def _active_orchestrator(cfg: Config) -> Orchestrator:
    rid = get_current_run(cfg.repo)
    if not rid:
        raise OrchestratorError("no active run — start one with `claudex run`")
    state = RunState.load(run_dir_for(cfg.repo, rid))
    return Orchestrator(cfg, state)


# ------------------------------------------------------------------ commands
def cmd_init(args) -> int:
    cfg = _cfg(args)
    task = task_file(cfg.repo)
    task.parent.mkdir(parents=True, exist_ok=True)
    if task.exists() and not args.force:
        print(f"already exists: {task} (use --force to overwrite)")
    else:
        task.write_text(_load_template(), encoding="utf-8")
        print(f"wrote {task}")
    cfg.save()
    print(f"wrote {cfg.config_path}")
    if gitops.is_git_repo(cfg.repo):
        if gitops.ensure_gitignore_entry(cfg.repo, ".claudex/"):
            print("added .claudex/ to .gitignore")
    else:
        print("WARNING: not a git repository — claudex requires git before `run`")
    print("\nNext: fill in .claudex/task.md by hand, or draft it from a")
    print('description with `claudex task "one paragraph of what you want"`,')
    print("then `claudex run`")
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


def cmd_run(args) -> int:
    cfg = _cfg(args)
    rid = get_current_run(cfg.repo)
    if rid:
        state = RunState.load(run_dir_for(cfg.repo, rid))
        if state.is_terminal():
            rid = None  # previous run finished; start a new one
    if rid:
        state = RunState.load(run_dir_for(cfg.repo, rid))
        print(f"resuming run {rid} (phase: {state.phase})")
    else:
        owner = cfg.owner
        if owner == "auto":
            owner = next_owner_auto(cfg.repo)
        state = RunState(run_id=new_run_id(), repo=str(cfg.repo), owner=owner)
        set_current_run(cfg.repo, state.run_id)
        state.save(run_dir_for(cfg.repo, state.run_id))
        print(
            f"new run {state.run_id}: owner={owner} (implements), "
            f"reviewer={state.reviewer} (challenges/verifies)"
        )
    orch = Orchestrator(cfg, state)
    orch.run_until_gate()
    return 0 if state.phase != "failed" else 1


def cmd_status(args) -> int:
    cfg = _cfg(args)
    rid = get_current_run(cfg.repo)
    if not rid:
        print("no active run")
        return 0
    state = RunState.load(run_dir_for(cfg.repo, rid))
    rd = run_dir_for(cfg.repo, rid)
    print(f"run:      {rid}")
    print(f"phase:    {state.phase}   mode: {state.mode}")
    print(f"owner:    {state.owner}   reviewer: {state.reviewer}")
    if state.plan_author:
        print(f"plan:     authored by {state.plan_author}")
    if state.branch:
        print(f"branch:   {state.branch}")
    if state.worktree:
        print(f"worktree: {state.worktree}")
    if state.error:
        print(f"error:    {state.error}")
    print(f"run dir:  {rd}")
    arts = sorted(p.name for p in rd.glob("*") if p.is_file() and p.name != "state.json")
    if arts:
        print("artifacts:")
        for a in arts:
            print(f"  - {a}")
    print("recent events:")
    for e in state.events[-8:]:
        print(f"  {e['ts']}  {e['message']}")
    return 0


def cmd_approve(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    if args.what == "plan":
        if orch.state.phase != "await_plan_selection":
            print(f"run is in phase {orch.state.phase}, not awaiting plan selection")
            return 1
        if args.choice not in ("claude", "codex"):
            print("choose the plan author: claudex approve plan claude|codex")
            return 1
        orch.select_plan(args.choice, notes=args.notes or "")
        orch.state.save(orch.run_dir)
        orch.run_until_gate()
    else:  # final
        orch.approve_final()
    return 0


def cmd_retry(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    try:
        orch.state.retry()
    except ValueError as exc:
        print(str(exc))
        return 1
    orch.state.save(orch.run_dir)
    orch.run_until_gate()
    return 0


def cmd_abort(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    orch.abort()
    print(f"aborted run {orch.state.run_id} (worktree kept; `claudex clean` to remove)")
    return 0


def cmd_clean(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    if not orch.state.is_terminal():
        print(f"run {orch.state.run_id} is still {orch.state.phase}; abort it first")
        return 1
    orch.clean()
    clear_current_run(cfg.repo)
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

    def version_of(binary: str) -> str:
        from .agents import _wrap_script  # noqa: PLC0415

        out = subprocess.run(
            [*_wrap_script(binary), "--version"],
            capture_output=True,
            text=True,
            timeout=60,
        )
        return f"{out.stdout.strip() or out.stderr.strip()}  [{binary}]"

    print("claudex doctor:")
    check("git", lambda: gitops.git(cfg.repo, "--version", check=True) or "ok")
    check(
        "git repo",
        lambda: "yes" if gitops.is_git_repo(cfg.repo) else (_ for _ in ()).throw(
            RuntimeError(f"{cfg.repo} is not a git repository")
        ),
    )
    check("claude", lambda: version_of(resolve_claude_bin(cfg.claude_bin or None)))
    check("codex", lambda: version_of(resolve_codex_bin(cfg.codex_bin or None)))
    check("task.md", lambda: str(task_file(cfg.repo)) if task_file(cfg.repo).exists() else "missing (run `claudex init`)")
    print("note: doctor checks wiring only; model/account issues surface on first run")
    return 0 if ok else 1


# --------------------------------------------------------------------- parser
def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="claudex",
        description="Deterministic co-engineering orchestrator for Claude Code and OpenAI Codex",
    )
    p.add_argument("--version", action="version", version=f"claudex {__version__}")
    sub = p.add_subparsers(dest="command", required=True)

    def common(sp):
        sp.add_argument("--repo", default=".", help="target repository (default: cwd)")

    sp = sub.add_parser("init", help="scaffold .claudex/task.md and config")
    common(sp)
    sp.add_argument("--force", action="store_true", help="overwrite existing task.md")
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

    sp = sub.add_parser("run", help="start or resume a run")
    common(sp)
    sp.add_argument("--owner", choices=["claude", "codex", "auto"], default=None,
                    help="implementation owner (auto alternates per task)")
    sp.add_argument("--mode", choices=["auto", "report", "change"], default=None,
                    help="pipeline shape: report (deliverable is analysis) or "
                         "change (deliverable is a diff); auto detects from the "
                         "goal's [REPORT]/[CHANGE] prefix")
    sp.add_argument("--auto-plan", dest="auto_plan", action="store_true", default=None,
                    help="skip the human plan-selection gate")
    sp.add_argument("--max-review-rounds", dest="max_review_rounds", type=int, default=None)
    sp.add_argument("--timeout", dest="agent_timeout", type=int, default=None,
                    help="seconds per agent invocation")
    sp.add_argument("--claude-model", dest="claude_model", default=None)
    sp.add_argument("--codex-model", dest="codex_model", default=None)
    sp.set_defaults(func=cmd_run)

    sp = sub.add_parser("status", help="show run state and artifacts")
    common(sp)
    sp.set_defaults(func=cmd_status)

    sp = sub.add_parser("approve", help="resolve a human gate")
    common(sp)
    sp.add_argument("what", choices=["plan", "final"])
    sp.add_argument("choice", nargs="?", help="plan author: claude|codex (for `approve plan`)")
    sp.add_argument("--notes", default="", help="selection rationale, recorded in the plan")
    sp.set_defaults(func=cmd_approve)

    sp = sub.add_parser("retry", help="re-attempt the failed phase")
    common(sp)
    sp.set_defaults(func=cmd_retry)

    sp = sub.add_parser("abort", help="abort the active run")
    common(sp)
    sp.set_defaults(func=cmd_abort)

    sp = sub.add_parser("clean", help="remove the finished run's worktree")
    common(sp)
    sp.set_defaults(func=cmd_clean)

    sp = sub.add_parser("doctor", help="check git/claude/codex wiring")
    common(sp)
    sp.set_defaults(func=cmd_doctor)

    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except (OrchestratorError, AgentError, gitops.GitError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
