# Changelog

This file is the version history for codex-model-router. Copy the relevant
released section verbatim into the corresponding GitHub release notes.

## [0.1.0] - Unreleased

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
