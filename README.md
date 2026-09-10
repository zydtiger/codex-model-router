# codex-model-router

A planned local model router for Codex, written in Go.

## Status

This repository currently contains the project scaffold, development conventions,
and CI. The executable prints a status message; it does not proxy requests or
modify Codex configuration.

The intended implementation combines a model catalog with request routing so
self-hosted models and native models can be selected in Codex. Provider
compatibility and desktop integration still require implementation and testing.
This is an unofficial integration.

## Development

Install the Go toolchain declared in `go.mod` and [prek](https://prek.j178.dev/).
From the repository root, select the development toolchain and activate hooks:

```sh
export GOTOOLCHAIN=$(awk '$1 == "toolchain" { print $2 }' go.mod)
prek install
prek run --all-files
prek run --all-files --hook-stage pre-push
```

Build the scaffold with `go build -o bin/codex-model-router ./cmd/codex-model-router`.
Mechanical validation commands are defined in `.pre-commit-config.yaml`; CI
invokes the same hook stages.

## Service deployment

A future macOS service will use a user LaunchAgent. Its template and installation
logic will live in this repository. No service is installed by this scaffold.
Keep deployment-specific endpoints and credentials out of committed files.

## Releases

No releases or release automation are configured yet. Choose a license and a
release contract before distributing the implementation.
