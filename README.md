# claudex

A deterministic co-engineering orchestrator for **Claude Code** and **OpenAI
Codex**. An external coordinator — not either model — drives both agents
through an explicit protocol: independent investigation, disagreement
analysis, adversarial plan review, single-owner implementation in an isolated
git worktree, commit-based review, remediation, and fresh-context
verification.

Neither agent is ever "the boss" of the other. The coordinator owns:

* **who owns the current change** (one implementation owner per run, roles
  alternate between runs),
* **what the other agent reviews** (exact artifacts: the task contract, the
  agreed plan, the literal diff — never a paraphrase),
* **how disagreements are resolved** (surfaced explicitly, human selects the
  plan unless `--auto-plan`),
* **when either agent may edit** (only the owner, only in the worktree,
  only during implement/remediate; every other phase is enforced read-only),
* **what evidence is required before completion** (schema-validated findings,
  commit-based diffs, a fresh-context verifier that treats all prior claims
  as testimony).

## Pipeline

```
DEFINE TASK (.claudex/task.md — both agents get the identical contract)
    │
    ├── CLAUDE INVESTIGATES ──┐   read-only, parallel, blind to each other,
    └── CODEX INVESTIGATES  ──┤   schema-validated analyses
                              ▼
                   DISAGREEMENT ANALYSIS      reviewer checks conflicting claims
                              ▼               against the repo itself
              ┌─ GATE 1: HUMAN SELECTS PLAN   (or --auto-plan)
              ▼
                ADVERSARIAL PLAN REVIEW       non-author attacks the plan
                              ▼
                     PLAN FINALIZE            author addresses blocking
                              ▼               corrections (skipped on clean approve)
                  OWNER IMPLEMENTS            isolated worktree + branch, commits
                              ▼
                   REVIEWER REVIEWS           reads exact diff at the commit,
                              ▼               read-only, severity-tagged findings
                  OWNER REMEDIATES            resumes its own session, commits
                              ▼               (loops, bounded by max_review_rounds)
                 FRESH VERIFICATION           reviewer, NEW session, checks every
                              ▼               acceptance criterion with evidence
              ┌─ GATE 2: HUMAN APPROVAL
              ▼
                        DONE                  `git merge claudex/<run-id>`
```

## Install

```powershell
pip install -e .
claudex --version
```

Requires Python ≥ 3.10 (stdlib only), git, a logged-in `claude` CLI, and a
logged-in `codex` CLI.

## Usage

```powershell
cd your-project
claudex init                 # scaffolds .claudex/task.md + config, gitignores .claudex/
# … fill in .claudex/task.md (the task contract) by hand, or draft it:
claudex task "users report the export button hangs on files >10MB; fix it without changing the export format"
# … review/edit the drafted contract — answer its open questions …
claudex doctor               # verify git/claude/codex wiring
claudex run                  # runs until gate 1
# … read claude-analysis.md, codex-analysis.md, disagreement.md …
claudex approve plan claude --notes "codex missed the cache invalidation path"
# … pipeline continues: plan review → implement → review → verify → gate 2 …
claudex approve final
git merge claudex/<run-id>
claudex clean                # removes the worktree
```

Other commands: `claudex status`, `claudex retry` (re-attempt a failed
phase), `claudex abort`.

Useful flags on `run`: `--owner claude|codex|auto` (auto alternates per
task, recorded in `.claudex/history.json`), `--auto-plan`,
`--max-review-rounds N`, `--timeout SECONDS`, `--claude-model X`,
`--codex-model Y`.

## How permissions are enforced

| Phase | Agent | Claude flags | Codex flags |
|---|---|---|---|
| investigate / disagreement / plan review / finalize | both / reviewer / author | `--permission-mode plan` | `-s read-only` (OS sandbox) |
| implement / remediate | owner only | `--permission-mode acceptEdits --allowedTools Edit,Write,NotebookEdit,TodoWrite,Bash` | `-s workspace-write` |
| review / verify | reviewer | `--permission-mode plan` | `-s read-only` |

Write phases run **only** in the run's dedicated worktree
(`<repo>.claudex.<run-id>` next to your checkout, branch
`claudex/<run-id>`), so the main checkout is never touched and two runs can
never collide. Note that Claude's write phase allowlists `Bash` so the owner
can run tests and `git commit` unattended — scope what that means for your
machine before running on sensitive projects, and tighten
`claude_write_allowed_tools` in `.claudex/config.json` if needed.

## Structured findings, not prose

Every phase result is schema-validated at the CLI layer (Claude
`--json-schema`, Codex `--output-schema`), so the coordinator routes typed
JSON: analyses, disagreement reports, plan reviews, severity-tagged code
review findings, per-criterion verification verdicts. Human-readable `.md`
renders sit next to each `.json` in `.claudex/runs/<run-id>/`, and every raw
agent invocation (command, stdin, stdout, stderr) is logged under
`.claudex/runs/<run-id>/logs/` for audit.

Schemas use OpenAI strict mode (`additionalProperties: false`, all
properties required) because Codex enforces that server-side; Claude accepts
the same schemas.

## Design decisions

* **External deterministic coordinator, no MCP cross-wiring.** Making either
  model "the boss" of the other lets it distort the task before delegating,
  selectively summarize dissent, and blur permission boundaries. Here the
  state machine is plain Python; every transition and artifact is auditable.
* **Same contract, no retelling.** Both agents read the identical
  `task.md` snapshot (copied into the run dir at start, so mid-run edits
  can't skew it). Reviewers receive artifact file paths, never summaries.
* **The human owns the task definition.** `claudex run` refuses an unfilled
  template — steering must come from you. `claudex task "..."` can draft the
  contract from one paragraph (an agent expands it, grounded in the repo),
  but the draft is a proposal: you review it, answer its open questions, and
  edit it before any run starts.
* **Independence before comparison.** Investigations run in parallel and
  blind, then a disagreement pass checks conflicting claims against the
  repository — the second opinion can't just validate the first framing.
* **One owner per change set, alternating.** `--owner auto` flips
  owner/reviewer roles between runs to prevent one model from becoming the
  permanent planner and the other a rubber stamp.
* **Review commits, not moving files.** The reviewer gets the base commit,
  the implementation commits, the implementer's report, and the exact diff
  the coordinator extracted itself.
* **Fresh-context verification.** The verifier never resumes any session.
  The owner's remediation, by contrast, deliberately *does* resume the
  implementation session — same engineer, same context.
* **Sessions are resumable, state is durable.** Every transition is
  persisted to `state.json` before proceeding; `claudex run` resumes
  mid-pipeline, `claudex retry` re-attempts a failed phase.

## Machine notes (binary resolution)

`claudex` resolves binaries in this order:

1. `CLAUDEX_CLAUDE_BIN` / `CLAUDEX_CODEX_BIN` environment variables,
2. `claude_bin` / `codex_bin` in `.claudex/config.json`,
3. for Codex on Windows: the desktop app binary under
   `%LOCALAPPDATA%\OpenAI\Codex\bin\*\codex.exe` (npm-distributed builds can
   lag behind what your account's configured model requires),
4. `PATH` (`.cmd`/`.bat`/`.ps1` shims are wrapped automatically).

`claudex doctor` shows what resolved and its version.
