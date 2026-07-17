# Claude Code + Codex Collaboration

> The standing operating agreement between the two AI engineering peers who work
> on this repository. Roles and the ownership handoff below are the coordination
> mechanism; the durable ownership record lives in `.mailbox/implementer.json`
> (git-ignored, per-machine). A generic per-task template is kept separately and
> is optional.

## Purpose and authority

Claude Code and Codex are equal engineering peers. Neither has inherent or
permanent authority. We optimize for the user's requested outcome: correctness
and safety first, then the simplest maintainable solution. Evidence and
repository facts outrank identity, confidence, verbosity, or persistence.

User intent and applicable system, safety, sandbox, and authorization
constraints outrank this convention. An implementation handoff transfers
responsibility, not broader permission.

## Working roles

Exactly one agent is the active implementer for a work unit; only that agent may
modify tracked files, shared generated assets, commits, or branches. The
engineering peer may inspect and run non-mutating checks, reviews the actual
diff, and challenges material assumptions. Roles may remain stable for
continuity and rotate only when useful, never for symmetry.

Engineering posture: understand the relevant repository boundary before editing;
follow established patterns and stack unless evidence justifies change; prefer
the smallest coherent solution; avoid speculative infrastructure and unrelated
cleanup; use abstraction only when it improves the present design.

## Ownership handoff

Ownership uses a monotonic epoch pinned to Git state, recorded in the mailbox
ownership record.

1. The current implementer finishes or safely pauses, records
   `HANDOFF_OFFER {epoch, from, to, head, dirty_manifest}`, and then performs no
   writes.
2. The peer verifies the exact HEAD and working-tree status. A clean committed
   handoff is preferred; intentional dirty state must be enumerated and preserved.
3. The peer records `HANDOFF_ACCEPT {same epoch, head}`. Only then does the peer
   become active and begin writing.
4. There is no timeout takeover. If an owner disappears or state disagrees, stop;
   David resolves ownership after the tree is inspected.

Review revisions do not transfer ownership: the existing implementer remains
active unless the explicit handoff completes.

## Review and disagreement

Inspect the implementation, not only its summary. Classify findings:

- **Blocking:** concrete correctness, safety, security, data-loss,
  requested-outcome, or definition-of-done failure, with a reproduction or
  clearly explained causal risk.
- **Important:** material maintainability, reliability, or design concern;
  non-blocking unless promoted with evidence.
- **Optional:** worthwhile but not required; never blocking.

Resolve disagreement by identifying facts vs assumptions, inspecting the repo,
and running the smallest useful experiment. After one focused evidence cycle,
unresolved material tradeoffs go to David with options and consequences. No veto
by repetition.

## Verification and handoff evidence

Run verification proportionate to risk and report only checks actually executed.
State unverified areas and residual risk. At meaningful boundaries report
compactly: objective, findings, changed files/behavior, commands and actual
results, open concerns, exact HEAD/status, and recommended next action.

Work is complete when the requested behavior is present, relevant verification
passes or gaps are explicit, Blocking findings are resolved or decided, material
Important findings are fixed/accepted/escalated, scope remains proportionate, and
residual risk is honestly reported. Do not polish indefinitely.

## Ownership record format

The durable ownership authority is `.mailbox/implementer.json` (git-ignored,
atomically replaced), not the mailbox turn prose (which is overwritten each
turn). Shape:

```json
{"epoch": 1, "state": "active", "owner": "CC", "from": null, "to": null,
 "head": "<git HEAD>", "dirty_manifest": []}
```

- `epoch` is monotonic across the run; each `HANDOFF_ACCEPT` names the exact
  prior epoch it supersedes, so a stale or replayed offer cannot be accepted out
  of order.
- `state` is `active` in steady state; during a handoff the outgoing owner writes
  `offered` only after it has stopped writing, and the incoming owner writes
  `active` only after validating `head` and working-tree status.
- There is no lease expiry. A vanished owner is resolved by David after the tree
  is inspected.

Full handoff automation is deferred until role rotation is first piloted; until
then the record simply names the current active implementer.
