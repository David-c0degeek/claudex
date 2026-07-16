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
- Transport protocol: the client-facing coordination layer over a role-addressed
  file mailbox. Restricted canonical JSON with a safe-integer domain and a strict
  parser backing whitespace/order-independent sha256 digests (`internal/canonjson`);
  a keyword-strict, version-keyed embedded JSON-schema registry serving both
  provider instruction and coordinator validation (`internal/protocol`); and the
  `pull`/`submit`/`wait`/`status` verbs (`internal/transport`) — a read-only,
  snapshot-bound assignment projection; accept-once submit under the state CAS
  with idempotent receipts and stale/conflict typing; a bounded lock-free
  long-poll; and a lock-free status projection with honest two-tier capability
  labels. Durable writes are content-addressed immutable artifacts published
  no-clobber under an `os.Root`, role-addressed session inboxes validated on read,
  and a single append-only human-readable mailbox mirror re-derived and
  re-validated from the accepted-artifact ledger — all over rooted,
  capability-split, Windows-retrying atomic-file primitives (`internal/atomicfile`).
- Project documentation: architecture, decision log, and the harvested
  test-vector inventory (`docs/`).

### Changed
- Reshaped from a headless subprocess orchestrator into a coordinator for two
  human-interactive agent terminals (Claude Code + Codex).

### Removed
- The Python implementation and its packaging (`claudex/`, `pyproject.toml`) —
  preserved in git history, harvested as executable requirements.
