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
  tokens, key/value assignments). Boundary wiring into state/transport persistence
  lands with subjects 01/02.
- Project documentation: architecture, decision log (D001–D016), and the
  harvested test-vector inventory (`docs/`).

### Changed
- Reshaped from a headless subprocess orchestrator into a coordinator for two
  human-interactive agent terminals (Claude Code + Codex).

### Removed
- The Python implementation and its packaging (`claudex/`, `pyproject.toml`) —
  preserved in git history, harvested as executable requirements.
