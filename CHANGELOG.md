# Changelog

All notable changes to claudex are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Greenfield **Go** rewrite of claudex as an interactive-pairing coordinator. The
prior Python implementation is retired (git history only). Nothing is released
yet; the sections below track work toward the first tagged binary.

### Added
- Go module skeleton (`cmd/claudex`, `internal/buildinfo`) on Go 1.25 with a
  `version`/`help` command surface and a green build/vet/test baseline.
- `internal/redact` — credential-redaction leaf (provider API keys, bearer
  tokens, key/value assignments), wired at the state persistence boundary so the
  durable digest is taken over the redacted bytes.
- Durable state core: an append-only immutable-generation store
  (`internal/genstore`) that never overwrites in place and recovers by
  validating the checksummed generation chain; typed run state with
  compare-and-swap transitions, accept-once turns, and a ledger projection
  (`internal/state`); a repository run catalog; and a prepared-transaction
  journal (`internal/txn`) that survives a crash between the git-ref move and the
  state CAS. Supporting leaves: atomic file writes (`internal/atomicfile`),
  OS advisory locks with crash-stale reclaim (`internal/oslock`), a
  local-filesystem classifier (`internal/fsclass`), and the run
  input/policy contract (`internal/config`).
- `claudex inspect-legacy <path>` — a read-only, redacted inspector for a
  pre-pivot Python run (`internal/legacy`); a pre-pivot `state.json` is refused,
  never resumed, as an attach run.
- Project documentation: architecture, decision log, and the harvested
  test-vector inventory (`docs/`).

### Changed
- Reshaped from a headless subprocess orchestrator into a coordinator for two
  human-interactive agent terminals (Claude Code + Codex).

### Removed
- The Python implementation and its packaging (`claudex/`, `pyproject.toml`) —
  preserved in git history, harvested as executable requirements.
