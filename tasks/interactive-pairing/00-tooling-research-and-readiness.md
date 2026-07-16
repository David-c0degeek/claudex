# 00 — Evidence Harvest, Go Toolchain And Readiness

## Goal
Prepare a greenfield **Go** rebuild (D013). Harvest the language-neutral assets
from the old Python code as executable requirements (D014) — schemas, fixtures,
golden outputs, and the 17 refactor-branch test files / crash-cut scenarios —
without porting a line of the old implementation. Stand up the Go module,
package skeleton, and toolchain; research the OS primitives, canonical-JSON, and
provider-launch facts the later subjects depend on; and write the protocol ADR.

## Boxes
> Subject 00 is required. No implementation subject starts until this subject is
> `DONE`, `ABANDONED`, or waived by decision. A non-green Go skeleton baseline
> from 00.3 blocks implementation subjects unless a §4 row accepts it.

- [ ] **00.1** (agent) Read repo instructions and authoritative docs (`README.md`, `docs/architecture.md`, `docs/decisions.md`, `tasks/lessons.md`, `tasks/claudex-refactor/`); list the intended pair-programming direction + constraints so later subjects flag drift. Record the vision→code gap (README describes the loop; the Python code drove subprocesses).
- [ ] **00.2** (agent) **Evidence harvest (D014), no code porting.** From `reliability-observability-refactor` git history (`git show <rev>:<path>`), catalog the reusable *assets*: the 17 test files, fixtures/golden outputs, JSON schemas, and crash-cut/adversarial scenarios. Turn the subtle-behaviour cases into a Go test-vector inventory — especially git **tree identity** (tracked/untracked/ignored, symlinks, submodules, clean/CRLF filters, case-only paths, staged dirt, racing edits), **process cancellation**, **redaction boundaries**, and **usage normalization**. Output: a mapping from each harvested vector → the Go subject that must satisfy it. Explicitly record that the Python modules are reference-only.
- [ ] **00.3** (agent) Stand up the Go module + skeleton and a green baseline: `go.mod` (module path + Go version), package layout (`cmd/claudex`, `internal/state`, `internal/transport`, `internal/gitx`, `internal/osprim`, …), and a hello-world CLI so `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...` all pass. Remove the Python implementation from the working tree on this branch (kept in git history per D014). Confirm/correct the §2 Verification-commands table.
- [ ] **00.4** (agent) Research + choose the OS-primitive mechanisms (D015) for Windows + POSIX, each destined for a build-tagged package: advisory locks with **process-death semantics**, a **crash-stale reclaim** signal (OS-held lock, or nonce + process-start-time identity), atomic rename + directory durability + sharing-violation retry, and process-tree kill (Windows create-suspended→assign-job→resume; POSIX process groups). Also pick the canonical-JSON approach (RFC 8785 or restricted equivalent). Record which mechanism 01.4 / 04 implement.
- [ ] **00.5** (agent) Research the interactive-terminal + provider facts: how the Claude Code and Codex CLIs behave as **interactive** sessions; whether each can be launched with inherited stdio + pinned cwd + sandbox/tool flags (managed tier); and whether any exposes a **usage/cost telemetry side-channel** usable without breaking TUI semantics (informs D007 / subject 07 — do not promise metered caps otherwise). Note ConPTY (Windows) vs PTY (POSIX) requirements. Cross-check `[[codex-cli-binary-quirks]]`.
- [ ] **00.6** (agent) Research applicable Go tooling + skills/MCP (`golangci-lint`, `goreleaser` for the cross-build/release, race-detector CI needs, fuzzing) and assistant skills already in use (`plan-from-template`, `verify`, `cleanup-audit`, `captain-hindsight`); classify each `adopt`/`defer`/`reject` with rationale, trust notes, source, setup cost.
- [ ] **00.7** (agent) Set up only approved Go tooling/config; **pin + audit the dependency set** (`golang.org/x/sys` + any JSON-schema / canonicalization libs) — static distribution is not dependency-free. Anything adding credentials, network services, or repo-config grants needs a human-owned box mirrored into `manual-actions.md`. Record provenance; never commit secrets.
- [ ] **00.8** (agent) Write the protocol/architecture **ADR** (into `docs/decisions.md` or a new ADR) capturing **D001–D015** + the transport/state/git-transaction contract + the **legacy inspect/export-only** behavior (D011: pre-pivot Python `state.json`/`current` runs get a read-only inspect/export utility, never a legacy execution engine). Bake findings into §2/§6/§7, subject boxes, and lessons. Inventory doc surfaces (README rewrite, architecture doc, skill docs). End with an implementation-readiness summary.
- [x] **00.9** (product-owner) Base decision (D009) — RESOLVED 2026-07-16: fresh branch `interactive-pairing` from `main`; Go greenfield (D013). Mirrored in `manual-actions.md`.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice. Date · slice number · box IDs touched · what shipped · how verified · checkpoint commit/push status.

- 2026-07-16 · slice 1 · 00.3 (partial), 00.9 · Baseline on fresh `interactive-pairing` branch (from `main`, D009 resolved). `python -m compileall claudex` OK (Python 3.14.6); `unittest discover -s tests` finds 0 tests — main has no suite. Discovered main is the lean v2 baseline; the 12-module reliability engine + 17 test files live only on the refactor branch (see lessons). Superseded by the Go pivot (D013/D014): the Python baseline is now reference-only; 00.3 is re-scoped to a Go skeleton. Plan committed (dfed322); not yet pushed.
