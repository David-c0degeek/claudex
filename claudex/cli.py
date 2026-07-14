"""claudex command-line interface.

    claudex init                    scaffold .claudex/task.md + config
    claudex task "..."              draft the task contract from a description
    claudex run [--lead claude|codex]
                                    headless pair run (drives both agents)
    claudex pair start              live run: YOUR session is the lead
    claudex pair plan --file f      submit your plan for pair critique
    claudex pair checkpoint         request pair review of your commits
    claudex pair verify             test gate + fresh-context verification
    claudex resolve --notes "..."   answer an AWAIT_GUIDANCE gate
    claudex status                  show run state and artifacts
    claudex retry                   re-attempt the phase that failed
    claudex abort                   abort the active run
    claudex clean                   remove the run's worktree
    claudex doctor                  check git/claude/codex wiring
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from importlib import resources
from pathlib import Path

from . import __version__, gitops
from .agents import AgentError, resolve_claude_bin, resolve_codex_bin
from .config import Config
from .phases import (
    Orchestrator,
    OrchestratorError,
    draft_task,
    task_has_content,
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


def _cfg(args) -> Config:
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
        )
    }
    return Config.load(Path(args.repo), overrides)


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


def _load_or_new_run(cfg: Config, driver: str, lead: str | None) -> tuple[RunState, bool]:
    """Returns (state, is_new). A terminal previous run rolls off."""
    rid = get_current_run(cfg.repo)
    if rid:
        state = RunState.load(run_dir_for(cfg.repo, rid))
        if not state.is_terminal():
            return state, False
    lead = lead or cfg.lead
    if lead not in AGENTS:
        raise OrchestratorError(f"lead must be one of {AGENTS}, got {lead!r}")
    state = RunState(
        run_id=new_run_id(), repo=str(cfg.repo), lead=lead, driver=driver
    )
    set_current_run(cfg.repo, state.run_id)
    state.save(run_dir_for(cfg.repo, state.run_id))
    return state, True


def cmd_run(args) -> int:
    cfg = _cfg(args)
    state, is_new = _load_or_new_run(cfg, "headless", getattr(args, "lead", None))
    if not is_new and state.driver == "live":
        print(
            f"run {state.run_id} is a live run (your session is the lead) — "
            "drive it with `claudex pair ...`, or `claudex abort` it first"
        )
        return 1
    if is_new:
        print(
            f"new run {state.run_id}: lead={state.lead} (plans, implements), "
            f"pair={state.pair} (critiques, reviews, verifies)"
        )
    else:
        print(f"resuming run {state.run_id} (phase: {state.phase})")
    orch = Orchestrator(cfg, state)
    return _locked(cfg, orch, lambda: _run_headless(orch))


def _run_headless(orch: Orchestrator) -> int:
    orch.run_until_gate()
    return 0 if orch.state.phase != "failed" else 1


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
            print(f"run {rid} is still {prev.phase} — finish or `claudex abort` it first")
            return 1
    # In live mode YOU are the lead; --lead names which agent you are so the
    # remaining one becomes the pair.
    state, _ = _load_or_new_run(cfg, "live", getattr(args, "lead", None))
    orch = Orchestrator(cfg, state)

    def go() -> int:
        orch.phase_init()
        orch.state.save(orch.run_dir)
        print(f"live run {state.run_id}: you are the LEAD ({state.lead}); "
              f"pair={state.pair}")
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
        print("bring this to the human, then: claudex resolve --notes '...'")
    elif phase is Phase.DONE:
        print(f"DONE — merge with: git merge {s.branch}")


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

    def go() -> int:
        orch.resolve_guidance(args.notes)
        print(f"guidance recorded; run returns to {orch.state.phase}")
        if orch.state.driver == "live":
            _next_live_hint(orch)
        else:
            orch.run_until_gate()
        return 0

    return _locked(cfg, orch, go)


# ---------------------------------------------------------------- inspection
def cmd_status(args) -> int:
    cfg = _cfg(args)
    rid = get_current_run(cfg.repo)
    if not rid:
        print("no active run")
        return 0
    state = RunState.load(run_dir_for(cfg.repo, rid))
    rd = run_dir_for(cfg.repo, rid)
    print(f"run:      {rid}   ({state.driver})")
    print(f"phase:    {state.phase}   mode: {state.mode}")
    print(f"lead:     {state.lead}   pair: {state.pair}")
    if state.steps:
        print(f"step:     {min(state.step_index + 1, len(state.steps))}/{len(state.steps)}")
    print(
        f"rounds:   plan {state.plan_round}/{cfg.max_plan_rounds + state.plan_cap_extra}"
        f" · checkpoint {state.checkpoint_round}/{cfg.max_checkpoint_rounds + state.checkpoint_cap_extra}"
        f" · tests {state.test_round}/{cfg.max_test_rounds + state.test_cap_extra}"
        f" · verify {state.verify_round}/{cfg.max_verify_rounds + state.verify_cap_extra}"
    )
    if state.branch:
        print(f"branch:   {state.branch}")
    if state.worktree:
        print(f"worktree: {state.worktree}")
    if state.gate_reason:
        print(f"gate:     {state.gate_reason}")
    if state.error:
        print(f"error:    {state.error}")
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


def cmd_abort(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)

    def go() -> int:
        orch.abort()
        print(
            f"aborted run {orch.state.run_id} (worktree kept; `claudex clean` to remove)"
        )
        return 0

    return _locked(cfg, orch, go)


def cmd_clean(args) -> int:
    cfg = _cfg(args)
    orch = _active_orchestrator(cfg)
    if not orch.state.is_terminal():
        print(f"run {orch.state.run_id} is still {orch.state.phase}; abort it first")
        return 1

    def go() -> int:
        orch.clean()
        clear_current_run(cfg.repo)
        return 0

    return _locked(cfg, orch, go)


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

    sp = sub.add_parser("run", help="start or resume a headless pair run")
    common(sp)
    run_flags(sp)
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

    sp = sub.add_parser("resolve", help="answer an AWAIT_GUIDANCE gate")
    common(sp)
    sp.add_argument("--notes", required=True, help="your decision — becomes binding guidance")
    sp.set_defaults(func=cmd_resolve)

    sp = sub.add_parser("status", help="show run state and artifacts")
    common(sp)
    sp.set_defaults(func=cmd_status)

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
    except (OrchestratorError, AgentError, gitops.GitError, RunLockError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
