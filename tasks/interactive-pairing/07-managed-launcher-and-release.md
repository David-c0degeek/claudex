# 07 — Managed Launcher, Distribution And Release

## Goal
The enforcement-upgrade tier and the ship. A managed-interactive launcher starts
each provider TUI with inherited stdio (fully human-interactive), a cwd pinned to
the coordinator worktree, and provider sandbox/tool flags — recovering
launch-time enforcement that BYO attach cannot. Enforcement capabilities are
labeled **individually** (`sandboxed`, `cwd-pinned`, `telemetry-observed`),
never a blanket "enforced". Then cross-build, sign, and distribute the Go binary
for Windows + Linux (+ macOS best-effort), and rewrite the docs.

## Integration analysis
> Greenfield Go (D013/D014) — there is no Python launch surface to extend. Build
> against the 00.5 provider-launch research and the harvested requirements.
- **Existing code found** — none to port; the Python `cli.py:open_watch_terminals` (Windows Terminal launcher) and provider flag definitions in `agents.py`/`providers.py` are **reference-only** for *what* flags/launch semantics each provider needs. The managed launcher is new Go in `internal/launch` using `os/exec` + build-tagged ConPTY (Windows) / PTY (POSIX) from `internal/osprim`.
- **Behaviour to preserve** (as requirements) — provider capability discovery (`doctor`); Claude tool restrictions + Codex sandbox/network flags; best-effort Windows Terminal launch semantics; the honest capability labels.
- **Reuse / extend** — harvest the provider flag matrix + `doctor` capability list as a Go table; reuse the 00.5 findings on inherited-stdio interactivity + telemetry.
- **Do not duplicate** — one launcher, one capability-discovery path, one OS-primitive package.
- **Integration point + why** — `internal/launch` composes `internal/osprim` (PTY/ConPTY, process-tree) + the provider table; `doctor` reports the capability matrix.
- **Vision fit** — realizes D005 (managed tier) + D007 (per-capability labels) + D015 (cross-platform, local-fs).
- **Risks** — inherited stdio does NOT yield usage telemetry by itself; over-claiming metered caps; command-argument unit tests cannot prove interactivity (need a manual smoke); ConPTY vs PTY differences; binary trust/signing.

## Boxes
- [ ] **07.1** (agent) Managed launch: `claudex` starts each provider TUI with inherited stdin/stdout (fully interactive), cwd pinned to the coordinator worktree, provider sandbox/tool flags applied at launch, and the run/session/role identity passed into the session so it can `pull`/`submit` without a manual `attach`. Define provider exit/reconnect behavior (a closed TUI is a replaceable session per 03.2, not a bricked run). Uses build-tagged ConPTY/PTY from `internal/osprim`. Test: launched session is interactive + pinned; identity + flags present.
- [ ] **07.2** (agent) Per-capability labeling: `status`/`doctor` report each enforcement capability separately (`sandboxed`, `cwd-pinned`, `telemetry-observed`, …) with the mechanism that backs it; no blanket "enforced" bit; BYO stays `protocol-only`. Test: labels match actual mechanisms.
- [ ] **07.3** (agent) Usage metering — only if 00.5 found a real telemetry channel (provider side-channel or PTY/ConPTY proxy preserving TUI semantics). If none, ship `telemetry-observed: false` and label usage caps `requires-telemetry`/`unavailable` (managed-with-inherited-stdio alone is still unmetered — NOT `requires-managed`); record the decision. Test: metered caps enforced only when the channel exists.
- [ ] **07.4** (agent) Cross-build + distribution (D015): produce reproducible binaries for `windows/amd64`, `linux/amd64`, `linux/arm64`, and `darwin/arm64`+`amd64` (best-effort) via `go build`/`goreleaser` if adopted; emit checksums; record a signing/notarization decision (Windows SmartScreen/Authenticode, macOS notarization) and an SBOM/provenance approach. Ship the local-filesystem-only preflight so state on OneDrive/SMB/NFS is refused. Test: the matrix builds; checksums verify; preflight rejects a non-local state dir.
- [ ] **07.5** (agent) Docs/release: rewrite `README.md` (attach model, two tiers, honesty labels, Go install/binary), update `docs/architecture.md` + `docs/decisions.md` (promote D001–D015 + protocol ADR), refresh CHANGELOG; `doctor` reflects the matrix. Behaviour-verify the documented commands.
- [ ] **07.6** (agent) Managed-launch capability matrix: document + test which capabilities each tier truly provides (BYO `protocol-only`; managed `sandboxed`/`cwd-pinned`; `telemetry-observed` only if 07.3's channel exists), so no doc or `status` output over-claims. Test: the matrix matches shipped labels.
- [ ] **07.7** (release-engineer) Manual interactive smoke on the supported-platform matrix: define the supported platforms and which the release environment actually has; launch a **real** Claude Code and Codex interactive TUI via managed launch and confirm interactivity + a completed turn on **each available** platform (command-argument unit tests cannot prove this). A platform the release environment lacks is recorded as an explicit support deferral, not an uncloseable gate. Mirrored in `manual-actions.md`.
- [ ] **07.8** (release-engineer) Release cut/tag/version + publish signed binaries + checksums per repo convention; mirrored in `manual-actions.md`.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
