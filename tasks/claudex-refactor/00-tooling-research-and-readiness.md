# 00 — Tooling Research And Readiness

## Goal

Research the actual Claudex repository, Python/subprocess/Git stack, installed
Claude and Codex CLI contracts, official provider guidance, relevant assistant
skills, and available local tooling before implementation. Convert findings
into concrete gates and references so later subjects work from verified current
facts and preserve the dirty starting workspace.

## Boxes

> This subject is required unless explicitly waived in the plan's §4 Decision
> log. No implementation subject starts until it is `DONE`, `ABANDONED`, or
> waived. A failing baseline in 00.3 blocks implementation unless §4 records a
> deliberate acceptance.

- [x] **00.1** (agent) Read all repo instructions and authoritative project
      docs—including `README.md`, `refactor.md`, packaging metadata, any vision,
      ADR, roadmap, planning, and lessons files—and record applicable constraints,
      provenance rules, conventions, and intended architecture in this Progress
      log or linked durable docs.
- [x] **00.2** (agent) Inventory the Python/runtime/package/CI/repo structure,
      current `main` HEAD, remote/checkpoint feasibility, and every pre-existing
      modified or untracked path; map provider execution, artifacts, state,
      lifecycle, prompts, CLI, Git operations, and tests to concrete symbols so
      later subjects extend the current pair-programming rewrite rather than
      overwriting or duplicating it.
- [x] **00.3** (agent) Run the baseline build, test, lint/format, and CLI smoke
      commands; confirm or correct the plan's §2 table and record exact output,
      including whether failures predate this plan.
- [x] **00.4** (agent) Research current primary-source practices for Python
      subprocess streaming, concurrent stdout/stderr draining, Windows process
      trees/job objects, atomic/versioned state, JSONL journaling, and temporary
      Git worktree testing; record only findings that change a box, principle,
      test, or implementation seam.
- [x] **00.5** (agent) Verify the installed and supported Claude/Codex CLI
      versions and their official streamed-output, partial-message, JSON schema,
      usage/cost, budget, sandbox/permission, cancellation, session, and
      multi-agent controls; record version-sensitive links and build a capability
      matrix that later becomes `claudex doctor` expectations.
- [x] **00.6** (agent) Research applicable `SKILL.md` workflows, assistant
      skills, MCP servers, and local tools; classify each `adopt`, `defer`, or
      `reject` with rationale, trust/source, permissions, setup command, and cost.
- [x] **00.7** (agent) Set up only approved local tooling/config required for
      coding. User confirmation is required before adding credentials,
      network-reachable services, or repo-level permission grants; mirror any
      resulting human box into `manual-actions.md`. Record provenance and never
      commit secrets.
- [x] **00.8** (agent) Bake every adopted finding into §2 commands, §6
      principles, §7 gates, subject boxes, §4 decisions, or `lessons.md`; list all
      affected user and contributor documentation surfaces and finish with a
      concise implementation-readiness summary.

## Hindsight checkpoint

> Run after all boxes are complete and before marking the subject `DONE` in §5.
> Use the plan's embedded Captain Hindsight prompt. Record Keep, Fix before
> closing, Record, Risk, and Verdict. `DO NOT CLOSE` leaves this subject open.

- [x] Captain Hindsight review recorded
- [x] Verdict is `CLOSE`

## Progress log

> One line per slice: date · per-subject slice number · boxes touched · outcome ·
> verification · checkpoint commit/push status. Add a `lessons.md` entry whenever
> the slice teaches something worth retaining during the run.

- 2026-07-15 · slice 1 · 00.1–00.7 · inventoried the complete dirty baseline,
  architecture, runtime, commands, provider binaries/capabilities, primary-source
  practices, skills, and local tools in `readiness.md`; preserved the passing
  pre-plan workspace as `a23224a` · verified Python 3.14.6, compileall, 24/24
  unittests, `git diff --check`, CLI 0.4.0, Claude 2.1.210, Codex PATH 0.144.4
  versus selected desktop 0.144.2, Git 2.54.0, and Windows Terminal 1.24 ·
  checkpoint commit `a23224a`; push accompanies subject-close checkpoint.
- 2026-07-15 · slice 2 · 00.8 · baked provider/event/process/Git findings into
  §2, §4, the implementation boxes, documentation inventory, and readiness
  conclusion; installed nothing and required no manual action · plan structure,
  sources, commands, and dirty-baseline preservation reviewed · checkpoint
  `042a227` pushed; its cached whitespace check exposed Markdown hard-break/EOF
  whitespace, corrected in the immediate follow-up checkpoint before 01.

### Captain Hindsight — closing review

1. **Keep:** The research used the actual installed CLIs plus primary sources,
   preserved the user's dirty work before refactoring, and mapped every new
   concern to an existing production seam. The offline-first test decision and
   explicit unknown-cost rule should remain.
2. **Fix before closing:** The first documentation checkpoint reported trailing
   Markdown hard-break spaces and extra EOF blank lines. They were removed in a
   follow-up commit without rewriting pushed history. The stale editable package
   metadata is a known release task, not a readiness blocker; it is recorded in
   `readiness.md` and covered by 06.5.
3. **Record:** D001–D003 capture the baseline, provider-boundary, and test-policy
   decisions. `readiness.md` records the provider versions and time-sensitive
   sources. No additional lesson is durable yet.
4. **Risk:** Provider event schemas can still drift; raw event retention,
   capability checks, tolerant parsing, and versioned fake fixtures constrain
   that risk. Windows job assignment may need a tested taskkill fallback.
5. **Verdict:** CLOSE.
