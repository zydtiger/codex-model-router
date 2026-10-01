# Repository Guidelines

## Purpose and ownership

Implement a configurable local model router for Codex. Keep routing, model
catalog generation, and service installation responsibilities explicit as they
are introduced. Read `README.md` before changing behavior.

- `cmd/codex-model-router/` owns the executable entry point.
- Add implementation packages under `internal/` when needed.
- Keep tests beside source as `_test.go`; use `testdata/` for fixtures.
- Keep service templates and installation logic in this repository.
- Keep personal paths, endpoints, credentials, and local configuration out of Git.

## Installation layout

The default router configuration is
`$XDG_CONFIG_HOME/codex-model-router/config.json`, or
`~/.config/codex-model-router/config.json`; only absolute XDG values apply. The
generated catalog defaults to `$XDG_DATA_HOME/codex-model-router/catalog.json`,
or `~/.local/share/codex-model-router/catalog.json`. Preserve explicit
`--config` and `catalog.output_file` locations.

For manual installation, use `~/.local/bin/codex-model-router`. A package
manager keeps its own executable path; `setup` uses its current executable by
default and `service install --bin` selects an explicit one. Never copy a
package-managed binary. Keep native catalog generation inputs temporary; they
are not runtime dependencies. Keep service definitions and logs in the
platform's service/log locations. Do not put personal deployment files in Git.

## Toolchain and validation

Use the exact development toolchain declared in `go.mod` by setting
`GOTOOLCHAIN` to its `toolchain` value. The `go` directive states the minimum
supported version. Commit module metadata changes together; a standard-library
only module does not require `go.sum`.

`.pre-commit-config.yaml` owns mechanical commands and scope. Install `prek`
per machine and run `prek install` per clone. CI invokes the same stages.

- Full validation: `prek run --all-files` followed by
  `prek run --all-files --hook-stage pre-push`.
- Targeted validation: `prek run --files <paths>` followed by
  `prek run --files <paths> --hook-stage pre-push`.
- Documentation-only validation: `prek run --files <document-paths>`.
- Build: `go build -o bin/codex-model-router ./cmd/codex-model-router`.

Use local mock upstreams for tests; normal validation must not require live
accounts or external inference services. Add concurrency and protocol tests as
those behaviors are implemented. Avoid speculative dependencies and checks.

## Git and publication

The base branch is `main`. Use focused Conventional Commit subjects with an
optional lowercase scope and an imperative lowercase summary. Allowed prefixes:
`feat`, `fix`, `docs`, `refactor`, `test`, `build`, `ci`, `perf`, `chore`, `revert`.
Use corresponding semantic prefixes for task branches.

Small isolated edits may stay on the base branch; substantive implementation
uses a task branch and PR. Preserve unrelated work. Editing does not authorize
commit, push, merge, or release; follow the user's explicit scope for each.

The repository is public on GitHub. Keep its instructions and development
workflow self-contained. Never include personal infrastructure in examples.

Releases use SemVer `vX.Y.Z` tags. Before `1.0.0`, breaking public CLI, config,
catalog, or service behavior increments the minor version; compatible changes
increment the patch version. Git tags are the Go binary version source: release
packaging passes the exact tag with `-ldflags`, while development builds report
`dev`. [CHANGELOG.md](CHANGELOG.md) is the sole version history; copy its
released section into GitHub release notes. From clean, synchronized `main`,
create an annotated tag only with explicit release approval, then run
`scripts/package-release.sh` to produce darwin/arm64 and linux/amd64 archives
and checksums. Published `v*` tags are immutable; do not move, replace, or
delete them. The project is distributed under the MIT License in `LICENSE`.
