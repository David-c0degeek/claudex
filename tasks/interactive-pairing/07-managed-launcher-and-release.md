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
> against the **07.10 provider research** (relocated here from 00.5) and the
> harvested requirements.
- **New Go packages** — `internal/launch` (managed launch via `os/exec` with **inherited stdio**), `internal/provider` (capability/flag table + `doctor`), and `internal/pty` **only if** 07.3 needs a telemetry proxy/capture path (ConPTY/PTY is not required for plain inherited-stdio interactivity). The Python `cli.py:open_watch_terminals` + `agents.py`/`providers.py` flags are **reference-only** for *what* each provider needs.
- **Behaviour to preserve** (as requirements) — provider capability discovery (`doctor`); Claude tool restrictions + Codex sandbox/network flags; honest capability labels.
- **Reuse / extend** — harvest the provider flag matrix + `doctor` capability list as a Go table; build on 07.10's inherited-stdio + telemetry findings.
- **Do not duplicate** — one launcher, one capability-discovery path.
- **Integration point + why** — `internal/launch` inherits the real terminal handles (no allocated PTY by default) and applies flags; `doctor` reports the capability matrix **and** the 01.8 filesystem classification (enforcement already happened at 03 bootstrap — 07 only reports).
- **Vision fit** — realizes D005 (managed tier) + D007 (per-capability labels) + D015 (cross-platform, local-fs).
- **Risks** — inherited stdio does NOT yield usage telemetry by itself; over-claiming metered caps; command-argument unit tests cannot prove interactivity (need a manual smoke); ConPTY vs PTY differences; binary trust/signing.

## Boxes
- [ ] **07.10** (agent) Provider interactive-launch research (relocated from 00.5; runs first, feeds 07.1/07.3): record how current Claude Code + Codex CLIs behave as **interactive** sessions, whether each accepts inherited-stdio launch with a pinned cwd + sandbox/tool flags, versioned capability probing, and the **telemetry side-channel verdict** (does a trustworthy usage/cost channel exist without breaking TUI semantics? — D007). Cross-check `[[codex-cli-binary-quirks]]`. Output: a provider capability/flags table + a yes/no telemetry verdict that 07.3 consumes. Test/artefact: the recorded table drives 07.1/07.2.
- [ ] **07.1** (agent) Managed launch with **direct inherited stdio** (`os/exec` inherits the existing terminal handles — no allocated PTY), using the 07.10 flags: fully interactive, cwd pinned to the coordinator worktree, provider sandbox/tool flags applied at launch, run/session/role identity passed in so the session can `pull`/`submit` without a manual `attach`. Define provider exit/reconnect behavior (a closed TUI is a replaceable session per 03.2, not a bricked run). Test: launched session is interactive (real terminal) + pinned; identity + flags present; no PTY allocated.
- [ ] **07.2** (agent) Per-capability labeling: `status`/`doctor` report each enforcement capability separately (`sandboxed`, `cwd-pinned`, `telemetry-observed`, …) with the mechanism that backs it; no blanket "enforced" bit; BYO stays `protocol-only`. Test: labels match actual mechanisms.
- [ ] **07.3** (agent) Usage metering — only if the 07.10 telemetry verdict found a real channel. If that channel needs a terminal proxy, implement `internal/pty` (ConPTY/PTY) here and only here, verifying it preserves TUI semantics. If no channel exists, ship `telemetry-observed: false`, label usage caps `requires-telemetry`/`unavailable` (managed inherited-stdio alone is still unmetered — NOT `requires-managed`), and do NOT build `internal/pty`. Record the decision. Test: metered caps enforced only when the channel exists; `internal/pty` built only if used.
- [ ] **07.4** (agent) Cross-build + distribution (D015): produce release binaries for `windows/amd64`, `linux/amd64`, `linux/arm64`, and `darwin/arm64`+`amd64` (best-effort) via `go build`/`goreleaser` if adopted; emit checksums + an SBOM/provenance artifact. **Reproducibility is a claim only if tested**: either prove byte-identity across two clean builds with documented flags (`-trimpath`, VCS/build-id policy, pinned toolchain + deps) — then call them reproducible — or call them "release binaries" and make no reproducibility claim. (The local-filesystem preflight ships in 01.8/03.1; `doctor` reports it — not built here.) Signing/notarization identities are NOT chosen here — that is release-owner action 07.9; this box must not embed or assume credentials. Test: the matrix builds; checksums + SBOM verify; reproducibility test present iff the claim is made.
- [ ] **07.5** (agent) Docs/release: rewrite `README.md` (attach model, two tiers, honesty labels, Go install/binary), update `docs/architecture.md` + `docs/decisions.md` (promote D001–D016 + protocol ADR), refresh CHANGELOG; `doctor` reflects the matrix. Behaviour-verify the documented commands.
- [ ] **07.6** (agent) Managed-launch capability matrix: document + test which capabilities each tier truly provides (BYO `protocol-only`; managed `sandboxed`/`cwd-pinned`; `telemetry-observed` only if 07.3's channel exists), so no doc or `status` output over-claims. Test: the matrix matches shipped labels.
- [ ] **07.7** (release-engineer) Manual interactive smoke — **Windows + Linux are both release gates** (first-class per D013/D015); only macOS may be deferred. Launch a **real** Claude Code and Codex interactive TUI via managed launch and confirm interactivity + a completed turn on Windows AND Linux (provision a Linux CI/VM if the release workstation lacks one — not an availability deferral). Command-argument unit tests cannot prove this. Mirrored in `manual-actions.md`.
- [ ] **07.9** (release-engineer) Signing decision: choose + provide a signing/notarization method (Windows Authenticode, macOS notarization) OR explicitly approve shipping unsigned with documented SmartScreen/trust consequences. Credentials never enter agent state/repo. Mirrored in `manual-actions.md`; blocks 07.8.
- [ ] **07.8** (release-engineer) Release cut/tag/version + publish binaries + checksums + SBOM per the 07.9 signing decision; mirrored in `manual-actions.md`.

## Hindsight checkpoint
- [ ] Captain Hindsight review recorded
- [ ] Verdict is `CLOSE`

## Progress log
> One line per slice.
