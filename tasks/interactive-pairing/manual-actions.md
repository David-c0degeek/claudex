# Manual actions — InteractivePairing

> Mirror of every human-owned (non-`agent`) box from the subject files (§5).
> Keep in sync with the owning subject file. Status: `TODO` / `DONE` /
> `DEFERRED` (a deferral needs a rationale).

| Box ID | Owner | Action | Source subject | Status | Deferral rationale |
|---|---|---|---|---|---|
| 00.9 | product-owner | Confirm base branch: branch from `main` vs continue on `reliability-observability-refactor` (D009) | 00 | DONE | Resolved 2026-07-16: fresh branch `interactive-pairing` from `main` |
| 07.7 | release-engineer | Manual interactive smoke — Windows AND Linux are both release gates (provision a Linux CI/VM if needed); launch real Claude + Codex TUIs via managed launch, confirm interactivity + a completed turn on each; only macOS may be deferred | 07 | TODO | |
| 07.9 | release-engineer | Signing decision: choose + provide a signing/notarization method (Windows Authenticode / macOS notarization) OR explicitly approve shipping unsigned with documented consequences; credentials never enter agent state. Blocks 07.8 | 07 | TODO | |
| 07.8 | release-engineer | Cut release: version bump + tag + publish binaries/checksums/SBOM per the 07.9 signing decision, after §7 gate passes | 07 | TODO | |
