# claudex

Pair-programming orchestrator for **Claude Code** and **OpenAI Codex**.

Two AI engineers work as **one team on one plan and one implementation**:
the **LEAD** (you choose it at start) drafts the plan, implements, and
fixes; the **PAIR** critiques the plan, reviews every step's commits, and
verifies the result with fresh context. They converge by agreement — not by
facing off and having a winner picked.

An external, deterministic coordinator (Python stdlib only) owns everything
the models must not own: phase order, edit permissions, response budgets,
convergence detection, diff extraction, and the transcript. Neither model
is ever "the boss" of the other.

## The loop

```
INIT
  → PLAN_DRAFT       lead drafts the plan, grounded in the repo (read-only)
  → PLAN_CRITIQUE    pair critiques it against the repo
  → PLAN_REVISE      lead accepts each finding or rebuts it with evidence
        ↺ until the pair AGREEs (zero blocking/major findings); each response
          budget includes the lead's final revision and a fresh plan audit
        content facts that fit an existing step become persistent
        implementation checks instead of forcing another plan rewrite
  → IMPLEMENT_STEP   lead implements exactly one plan step, commits
  → CHECKPOINT       pair reviews that step's exact diff
  → FIX              lead fixes blocking/major findings, commits (loops)
        ↺ next step, until all steps are agreed
  → TESTS            coordinator runs your test command itself — exit code
                     decides, never agent testimony
  → VERIFY           pair with FRESH context checks every acceptance
                     criterion in the task contract
  → DONE             automatically on verify pass
AWAIT_GUIDANCE       actual choice → `claudex resolve --notes "..."`;
                     exhausted quality budget → `claudex continue`
```

**Stop conditions** (the "good enough" metric — they can never loop
forever):
- convergence = pair verdict `AGREE` with zero blocking/major findings;
- plan findings are limited to architecture, scope, step safety, and
  validation strategy; volatile facts/content details are carried forward as
  implementation checks and reviewed against committed artifacts;
- lead-response budgets: plan revisions (5), checkpoint fixes per step (3),
  test fixes (2), verify fixes (2) — all configurable; every budget permits
  the final lead response before a fresh review. Exhaustion is reported as a
  quality-budget stop, not falsely labeled a disagreement;
- a critique can request human guidance early only for a concrete unresolved
  value choice or repeated evidence-backed disagreement;
- mechanical test gate: your configured command must exit 0;
- final verification runs with a fresh session — no shared history with
  the implementation.

## Two ways to run it

### Headless — `claudex run`

The coordinator drives **both** agents as subprocesses through the whole
loop. You come back for gates (if any) and the merge.

```bash
claudex init                       # scaffold .claudex/task.md + config
claudex task "one paragraph..."    # optional: agent-drafted task contract
claudex run --lead claude          # or --lead codex — the lead is YOUR call
# ...
claudex status                     # phase, step k/N, response budgets
claudex resolve --notes "..."      # answer a concrete decision gate
claudex resolve --notes-file decision.md  # multiline/shell-safe alternative
claudex continue                   # allow one response + fresh audit, no guidance
claudex continue --responses 2     # larger extension only when explicitly chosen
claudex restart                    # retire a legacy/stuck plan; preserve decisions
git merge claudex/<run_id>         # DONE prints the exact command
claudex clean
```

### Live — `claudex pair` (your session is the lead)

You work inside an interactive Claude Code (or codex) session as the lead;
claudex runs only the **pair's** turns, enforcing the same state machine,
caps, and artifacts.

```bash
claudex init --with-skill          # installs .claude/skills/claudex-pair
claudex pair start                 # you lead; --lead codex if you are codex
claudex pair plan --file plan.json # pair critiques; revise until AGREE
# implement step 1 in the printed worktree, commit, then:
claudex pair checkpoint --notes "step 1: ..."
# ...steps advance automatically on AGREE...
claudex pair verify                # test gate + fresh verification → DONE
```

The installed skill (`.claude/skills/claudex-pair/SKILL.md`) teaches an
interactive Claude session the full protocol; `codex-lead-prompt.md` is the
mirror for codex-led sessions. Two things the skill insists on: run pair
turns in the background (they invoke the other model and can take many
minutes plus usage-limit waits), and remember the worktree is a **sibling
directory** of your repo (grant access with `--add-dir` or work from a
shell).

## The mailbox

Every turn — plans, critiques, commits, test results, guidance, DONE — is
appended by the coordinator to `.claudex/runs/<run_id>/mailbox.md` in an
append-only block format:

```
===== [LEAD] turn 3 | plan | STATUS: REVISE =====
plan round 1: ...\plan-round-1.json
steps (4):
  1. ...
----- end [LEAD] turn 3 -----

===== [PAIR] turn 4 | plan | STATUS: AGREE =====
no findings
----- end [PAIR] turn 4 -----
```

`tail -f` it to watch the pairing live. It is the run's full transcript;
every claim in it is backed by a JSON artifact in the same directory.

## Roles

| | LEAD (you pick at start) | PAIR (the other one) |
|---|---|---|
| plan | drafts, revises, rebuts with evidence | critiques against the repo |
| code | implements one step per commit, fixes | reviews each step's exact diff |
| finish | — | fresh-context verification |
| write access | worktree only, implement/fix turns only | never |

The lead is whoever you start the run with — `--lead claude|codex`
(headless) or which session you lead from (live). No rotation, no
winner-picking: ownership is resolved by initiation.

## Enforcement, not prompt discipline

| guarantee | mechanism |
|---|---|
| critiques/reviews can't edit | Claude `--permission-mode plan`; Codex `-s read-only` (OS sandbox) |
| implementation isolated | dedicated git worktree (sibling dir) on a run branch |
| reviews see exact code | coordinator extracts `git diff` itself; dirty worktrees refused |
| structured findings | Claude `--json-schema`, Codex `--output-schema` (strict mode) |
| convergence is checkable | `AGREE` + zero blocking/major, evaluated by the coordinator |
| planning stays planning | plan-level finding categories are schema-constrained; content obligations persist separately as implementation checks |
| inconclusive reviews don't cause churn | tool/evidence failures retry the reviewer fresh without charging a lead response |
| no infinite loops | explicit lead-response budgets plus a final fresh-context audit |
| guidance stays binding | persistent guidance ledger included in every later agent turn |
| crash safety | state.json written after every round; artifact-presence skip on retry |
| no cross-process races | run-dir lockfile around every state-mutating command |
| verification is independent | verify turn never resumes any session |
| test results are real | coordinator runs `test_command` itself, exit code decides |

Session continuity without contamination: each role keeps its own session
lineage (`lead_plan`, `pair_plan` in the repo; `lead_impl`, `pair_review`
in the worktree — resume pins the original cwd, so lineages never cross),
and the verifier gets none of them.

## Report mode

Start the task contract's goal with `[REPORT]` (or `--mode report`) when
the deliverable IS analysis: the report draft becomes the single step —
lead writes and commits it in the worktree, the pair critiques it through
the same checkpoint loop, fresh verification checks the contract. No
plan-about-the-work layer.

## Configuration

`.claudex/config.json` (written by `claudex init`, overridable per-run by
flags):

```jsonc
{
  "lead": "claude",             // who holds the pen by default
  "mode": "auto",               // auto | change | report
  "max_plan_rounds": 5,       // plan-revision budget
  "max_checkpoint_rounds": 3, // fix budget per step
  "max_test_rounds": 2,       // fixes after test failures
  "max_verify_rounds": 2,     // fixes after verification failures
  "test_command": "",           // mechanical gate, run by the coordinator
  "agent_timeout": 3600,
  "wait_on_limits": true,       // wait out provider usage limits and resume
  "claude_model": "",           // pin models if you want reproducibility
  "codex_model": ""
}
```

Binary discovery: `CLAUDEX_CLAUDE_BIN` / `CLAUDEX_CODEX_BIN` env vars win;
on Windows the Codex desktop-app binary is preferred over npm shims.

## Install

```bash
pip install -e .
claudex doctor        # checks git + both CLIs
```

Requires Python 3.10+, git, `claude` CLI, `codex` CLI. No third-party
Python dependencies.

## Design decisions

- **Pair, not face-off.** One plan, co-owned. The pair's AGREE means "I
  co-own this plan and its implementation", not "you win".
- **Lead by initiation.** Whoever you start with holds the pen. This
  resolves ownership without rotation schemes or model-vs-model authority.
- **Agreement with teeth.** AGREE is only accepted with zero blocking/major
  findings. Complete revision/fix cycles have explicit budgets. A real choice
  becomes persistent human guidance; simple budget exhaustion can continue
  without inventing a decision.
- **The coordinator is code, not a model.** Phase transitions, caps,
  diffs, test results, and convergence are computed deterministically.
  Models argue; the state machine decides what happens next.
- **Evidence beats testimony.** Rebuttals require file-level evidence;
  the verifier treats all prior claims as unverified; the test gate is a
  subprocess exit code.
