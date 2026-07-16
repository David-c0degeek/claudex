# 04 — Git Transaction And Review Evidence

## Goal
Coordinator-owned git as the concrete participant in the 01 prepared-transaction
machine. On an implementation `submit`, build the exact worktree snapshot in a
temporary index, create a commit from that tree, CAS-update the run branch from
the expected old HEAD, **synchronize the checked-out index to the new tree**,
and record the committed object as immutable review evidence. Reviews read a
hashed evidence packet materialized from the committed Git object — never the
live worktree. The mechanical test gate runs as a separate coordinator-owned
subprocess **outside the mutation lock**.

## Integration analysis
> **Superseded by D013/D014 (Go greenfield):** implement fresh in Go (`internal/gitx`, `internal/evidence`) shelling out to native `git` via `exec.CommandContext` with argv (§6.24) — no go-git. Tree-identity, snapshot, and cancellation behaviours are pinned by the harvested test vectors from 00.2 (the old tests/counterexamples, not the old code). `git update-ref <new> <old>` supplies the native ref CAS. Re-fill in Go terms before ticking any box.
> Fill/confirm against the 00.2 harvest before ticking any box.
- **Existing code found** — `claudex/gitops.py` (worktree, diff, dirty/tree identity incl. untracked bytes per lessons); `claudex/processes.py` streamed timeout/cancel primitive (reuse for the test gate only); `claudex/evidence.py` (**currently copies selected files from the live `cfg.repo`** — must be changed to materialize from a committed Git object); `claudex/artifacts.py` (non-atomic — see 02).
- **Behaviour to preserve** — tree identity semantics; test result decided by subprocess exit code + unchanged exact tree, never agent testimony; worktree is a sibling dir on a run branch.
- **Reuse / extend** — reuse `gitops.py` plumbing for snapshot/tree-hash; reuse the `processes.py` streamed timeout/cancel for the test command; **rewrite** `evidence.py` to build a committed-object packet; drive commits through 01's journal.
- **Do not duplicate** — no second git or subprocess primitive; the retired provider-invocation path in `processes.py` is out of scope (07).
- **Integration point + why** — `gitops.py` owns git; extend it with temp-index snapshot-commit + ref CAS + index sync, wrapped by 01's journal.
- **Vision fit** — realizes D006 and §6.19 (immutable evidence, never live worktree).
- **Risks** — the checked-out index describing old HEAD after commit (would make the worktree perpetually "dirty" and deadlock 04.4); reviewer evidence leaking from the live worktree; a test subprocess held under the run lock breaking concurrency; symlink/submodule/filter edge cases.

## Boxes
- [ ] **04.1** (agent) Snapshot-and-commit with a defined scope: on an IMPLEMENT/FIX submit, the worktree dirt **is the input** (not refused); snapshot = Git-relevant **tracked + non-ignored untracked** content (ignored files are outside the tree identity), with documented symlink/submodule/filter behavior. Build the snapshot in a temporary index, create a commit from that tree with a deterministic plan-agnostic message, no hooks. In every *other* phase, worktree dirt is refused (read-only phases per 03.7). Test: an IMPLEMENT snapshot captures tracked+untracked; a CHECKPOINT-phase dirty worktree is refused.
- [ ] **04.2** (agent) Ref CAS + index sync + journal: update the run branch only if HEAD still equals the expected old HEAD; then **synchronize the checked-out worktree's real index to the new tree without overwriting files** (safe mixed/index reset) and re-verify clean — otherwise the index still describes old HEAD and every successful commit immediately reads dirty, deadlocking 04.4. Wrap ref move + index sync + state CAS in the 01 journal; record pre/post identities; the index step is a named crash-cut. Test: after commit the worktree reads clean; stale HEAD rejected; crash between any cut reconciles.
- [ ] **04.3** (agent) Immutable review evidence packet: rewrite `evidence.py` to **materialize selected files from the committed Git object** (plus the diff/tree IDs), hash the whole packet, and **never copy from the live `cfg.repo`**. The pair's review assignment references this packet/root only. Test: the evidence packet is byte-stable regardless of later worktree edits, and contains files read from the commit, not the worktree.
- [ ] **04.4** (agent) Post-snapshot edit detection: edits racing after the snapshot remain uncommitted and block the next lead `submit` with a clear protocol-violation status; never silently absorbed. (Depends on 04.2 index sync being correct, else this trips falsely.) Test: a genuine post-snapshot edit blocks; a clean post-commit worktree does not.
- [ ] **04.5** (agent) Mechanical test gate outside the lock: persist an active test attempt under the lock, **release the lock**, stream/cancel the `test_command` via the `processes.py` primitive, then reacquire and finalize the result via revision CAS. `status` and `cancel` stay usable throughout. Exit code + unchanged exact tree decide. Test: a long test does not block `status`/`cancel`; a stale finalizer is rejected; failing command fails the gate.
- [ ] **04.6** (agent) Merge gating: on DONE, print the exact `git merge <run branch>` command; refuse to advance if the tree/tests didn't pass. Test: DONE only reachable through a passed gate.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
