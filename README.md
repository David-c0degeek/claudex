# claudex

**Pair-programming coordinator for two interactive AI agent terminals — Claude
Code and Codex.**

You open two terminals — one Claude Code, one Codex — and pair-program with both
at once. You pick the **LEAD** simply by which terminal you drive; the other is
the **PAIR**. Both terminals stay fully interactive, so either agent can talk to
you at any phase. They work one plan and one implementation together, checking
each other's assumptions and work, and **converge by agreement** — not by facing
off and having a winner picked.

An external, deterministic coordinator (this binary) owns everything the models
must not: phase order, edit permissions, response budgets, convergence
detection, the git transaction, and the transcript. Neither model is ever "the
boss" of the other.

## Status: greenfield rebuild in progress

claudex is being rebuilt from scratch in **Go** as a single distributable
binary (Windows + Linux first-class, macOS best-effort). The earlier Python
implementation is retired and lives only in git history; its test suite and
schemas are being harvested as executable requirements for the new build (see
[`docs/evidence-harvest.md`](docs/evidence-harvest.md)).

Today the binary ships the attach protocol — `attach`, `pull`, `submit`, `wait`,
and `status` — over the durable coordinator, alongside `inspect-legacy`, a
read-only view of a pre-pivot Python run. Human gates and the operator surface
land over the subsequent milestones.

## Build

```bash
go build ./...          # build everything
go test ./...           # run the suite
go run ./cmd/claudex version
```

Requires Go 1.25+ and `git`. No third-party runtime services; claudex shells out
to native `git` and to the `claude` / `codex` CLIs.

## How it will work

```
INIT → PLAN_DRAFT → PLAN_CRITIQUE → PLAN_REVISE
     → IMPLEMENT_STEP → CHECKPOINT → FIX      (per step)
     → TESTS → VERIFY → DONE
```

Convergence is the pair's `AGREE` with zero blocking/major findings. The
coordinator computes transitions, caps, diffs, test results, and convergence
deterministically — models argue, the state machine decides.

Two tiers:
- **BYO attach** — coordinate your own already-running Claude Code + Codex
  terminals over a durable file protocol (`protocol-only` enforcement).
- **Managed launch** — claudex starts each terminal with a pinned working
  directory and provider sandbox flags, still fully interactive, with
  individually-labelled enforcement capabilities.

## Design

- Architecture: [`docs/architecture.md`](docs/architecture.md)
- Decisions (ADR log): [`docs/decisions.md`](docs/decisions.md)
- Harvested requirements: [`docs/evidence-harvest.md`](docs/evidence-harvest.md)

## Design principles

- **Pair, not face-off.** One plan, co-owned. `AGREE` means "I co-own this",
  not "you win".
- **Lead by initiation.** Whoever you start with holds the pen. No rotation, no
  model-vs-model authority.
- **The coordinator is code, not a model.** Phase transitions, caps, diffs, test
  results, and convergence are computed deterministically.
- **Evidence beats testimony.** Reviews read committed git objects; the test
  gate is a subprocess exit code; the verifier treats prior claims as unverified.
