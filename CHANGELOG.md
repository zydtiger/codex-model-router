# Changelog

This file is the version history for codex-model-router. Copy the relevant
released section verbatim into the corresponding GitHub release notes.

## [Unreleased]

## [0.3.0] - 2026-10-26

### Changed

- Leave request and response size limits to upstreams, including gzip/zstd
  decoding, tool adaptation and compaction checkpoints. Preserve native
  compressed bodies and upstream HTTP errors.

### Breaking

- Remove `max_request_bytes`; remove the old configuration key when upgrading.
  Configuration validation rejects this obsolete key.

## [0.2.2] - 2026-10-13

### Fixed

- Proxy native image generation and editing endpoints, allowing self-hosted
  chat models to use Codex's image tool through ChatGPT authentication.
- Preserve image payloads, streaming, cancellation, and upstream errors; log
  rejected endpoint methods and paths for diagnosis.

## [0.2.1] - 2026-10-10

### Fixed

- Log upstream transport and response-stream failures through the configured
  logger at WARN, including when per-request logging is disabled.
- Include bounded upstream response metadata, byte counts, TLS alerts, and
  typed network errors without recording request bodies or credentials.
- Count failures before response headers in health metrics and distinguish
  client cancellation from upstream failures.

## [0.2.0] - 2026-10-02

### Added

- Opt-in `routes[].input.developer_role_as_user` to wrap developer messages as
  tagged user messages at their original positions, with a fixed convention
  appended to top-level instructions. The setting defaults to `false` and does
  not affect native ChatGPT or OpenAI API requests.
- Coverage for message order, string and content-block preservation, stable
  translated prefixes, route isolation, and JSON/SSE tool-call round trips.

### Breaking

- Removed `routes[].input.developer_role_as_system`. Delete this field from
  existing configurations before upgrading; validation rejects it. Enable
  `developer_role_as_user` where tagged-user conversion is needed. With the new
  setting disabled, developer messages and top-level instructions are preserved.

## [0.1.0] - 2026-09-30

### Added

- Initial public release of the loopback Responses API router, native catalog
  merge, self-hosted routes, and macOS LaunchAgent/Linux systemd user-service
  lifecycle commands.
- `setup` for safe first-run configuration, bundled native catalog generation,
  service installation, health verification, and optional Codex configuration.
- Local release packaging for darwin/arm64 and linux/amd64 artifacts.
- MIT License.

### Changed

- Router configuration now follows XDG configuration paths and generated
  catalogs follow XDG data paths.

### Breaking

- The former `~/.local/lib/codex-model-router/` combined installation is no
  longer the default. Existing custom configuration and catalog paths remain
  supported through `--config` and explicit `catalog.output_file` values; see
  the migration instructions in the configuration reference.
