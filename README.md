# codex-model-router

A Go loopback proxy and catalog generator for using native GPT models and
self-hosted Responses API models from one Codex model selector.

## Status

Implemented: exact model routing, native credential forwarding, isolated remote
credentials, streaming, request limits, reasoning and namespace-tool adaptation,
custom-tool bridging to function-only upstreams, combined
catalog generation and platform service lifecycle commands (macOS LaunchAgent
and Linux systemd user service).

Routes with `tools.schema_loading: "on_demand"` get a compact namespace
directory that lets the model load only needed groups of function schemas,
instead of receiving the full MCP inventory on every request. Function-call
responses and replayed history retain Codex's namespace identities across JSON
and SSE. See [tools](docs/configuration.md#routestools).

Routes can also opt into [text checkpoints for remote compaction v2](docs/configuration.md#text-checkpoints-for-remote-compaction-v2), using the selected upstream model to summarize context while Codex owns history replacement and persistence.

Validation includes mock upstream tests, race tests, and a real SGLang function
call/result loop. With Codex CLI 0.153.4, an isolated configuration successfully
loaded the combined catalog and used a self-hosted model to run a read-only shell
tool. The CLI fell back from WebSocket to HTTP after the router returned 426.
The desktop picker UI, real GPT upstream requests, and actual LaunchAgent
installation have not been verified. Recheck integration after Codex updates.

## Build

```sh
GOTOOLCHAIN=go1.27.1 go build -o bin/codex-model-router ./cmd/codex-model-router
```

Use `--help` on each subcommand for its flags.

## Setup

The router configuration defaults to
`$XDG_CONFIG_HOME/codex-model-router/config.json`, or
`~/.config/codex-model-router/config.json`. Its final catalog defaults to
`$XDG_DATA_HOME/codex-model-router/catalog.json`, or
`~/.local/share/codex-model-router/catalog.json`. Relative XDG values are
ignored so interactive and service invocations share stable paths.

When `catalog.output_file` is empty, `setup` and `service install` pass the
effective absolute `XDG_DATA_HOME` into the service definition. A service
`--env XDG_DATA_HOME=...` value must name that same absolute directory; a
different or relative value is rejected before installation.

For a manual installation, build into your normal user `PATH`:

```sh
mkdir -p "$HOME/.local/bin"
GOTOOLCHAIN=go1.27.1 go build -o "$HOME/.local/bin/codex-model-router" ./cmd/codex-model-router
```

A package manager keeps its own executable path. `setup` uses the executable
that was invoked, while `service install --bin /path/to/codex-model-router`
selects one explicitly.

Run the first-time setup:

```sh
codex-model-router setup --dry-run
codex-model-router setup --configure-codex
```

`setup` creates a valid native-only configuration only when the target does not
exist; it preserves an existing custom configuration. It exports bundled native
models using a temporary isolated `CODEX_HOME`, atomically generates the final
catalog, reloads and validates it, installs/restarts the user service, then
waits for `/healthz`. It never downloads or copies a binary. `--dry-run` creates
no files, runs no exporter or service command, and makes no configuration change.

`--configure-codex` is the opt-in for editing Codex's root configuration
(`$CODEX_HOME/config.toml`, or `~/.codex/config.toml`). It changes only
`openai_base_url` and `model_catalog_json`, preserves unrelated TOML and
comments where possible, and saves exact pre-change bytes once as
`config.toml.codex-model-router.bak`. It refuses a root `model_provider`
override rather than silently configuring a bypassed route. Restart Codex after
success.

To add a self-hosted route, edit the JSON configuration, then rerun setup with
the required service credentials:

```sh
codex-model-router setup --env LAB_MODEL_API_KEY="$LAB_MODEL_API_KEY"
```

Repeat `--env` values on later `setup` or `service install` calls; the service
definition is replaced with the values passed then. Its mode becomes `0600` when
it carries environment values, but it is not a secret store.

`catalog generate` remains available for a catalog-only refresh. With no
`catalog.native_catalog_file`, it invokes `codex debug models --bundled` in the
same isolation; `--codex /path/to/codex` selects a particular executable,
including a Nix build. An explicit `catalog.native_catalog_file` preserves the
offline raw-file workflow and bypasses the exporter. Generation never changes
the binary, service, router configuration, or Codex TOML; a failure preserves
the old catalog.

## Legacy layout migration

Earlier builds used `~/.local/lib/codex-model-router/` for the binary,
configuration, and catalog. Detect that bounded legacy layout with:

```sh
test -e "$HOME/.local/lib/codex-model-router/config.json" &&
  printf '%s\n' 'legacy codex-model-router configuration found'
```

Nothing migrates automatically. Keep a legacy configuration working with an
explicit path, preserving its custom routes and relative catalog location:

```sh
codex-model-router setup --config "$HOME/.local/lib/codex-model-router/config.json" \
  --bin /path/to/codex-model-router
```

To move deliberately, back up and copy the JSON, set `catalog.output_file` to
`""`, generate a new catalog, then run `setup --configure-codex`. Do not delete
legacy files until the new service and catalog have been checked.

## Self-hosted route configuration

Edit the router configuration before proceeding:

- Set `routes[].base_url` and exact `routes[].models` IDs for your server.
- Remove routes you do not use. If authentication is unnecessary, omit `auth`;
  otherwise set the environment variable named by `auth.api_key_env`.
- Leave `catalog.native_catalog_file` empty for the normal bundled export, or
  set it to a raw catalog for offline generation. Leave `catalog.output_file`
  empty for the XDG data default. Relative paths resolve from the configuration
  file's directory.
- Set each catalog model's `id` and `route` to the corresponding route. Set
  `display_name` to the label you want in the selector, independently of its ID.
  For example, `Qwen-3.8 Flash Next` can label
  `nvidia/Qwen3.8-Flash-Next-NVFP4`.
- Supply `base_instructions` or `catalog.base_instructions_file` when your native
  catalog has no instructions to inherit. Set `routes[].reasoning.supported_efforts`
  once for the reasoning adapter and the generated picker options. Set each catalog
  model’s `default_reasoning_level` to a member of that list. Remove the former
  `catalog.models[].reasoning_levels` field when upgrading.

```sh
codex-model-router validate
codex-model-router catalog generate
catalog_data_home=${XDG_DATA_HOME:-"$HOME/.local/share"}; case "$catalog_data_home" in /*) ;; *) catalog_data_home="$HOME/.local/share" ;; esac
codex -c "model_catalog_json=\"$catalog_data_home/codex-model-router/catalog.json\"" debug models
```

The XDG default is independent of the current directory. Relative `--config`
and `CODEX_MODEL_ROUTER_CONFIG` values are explicit overrides and resolve from
the current directory. Start the router in the foreground with
`codex-model-router serve`, or use the standalone service commands for custom
service definitions. See [Codex setup](docs/codex-desktop.md).

For login startup and crash recovery, use `service preview`, `service install`,
`service status`, and `service uninstall` with the selected binary.
See [launchd setup](docs/launchd.md) or [systemd setup](docs/systemd.md) for paths
and environment handling.
Service configuration belongs to this repository. No Docker container is required.

## Routing and limits

Configured remote model IDs go to their assigned upstream. Explicit native IDs,
including non-remote IDs imported from the combined catalog, go to the native upstream.
Unknown IDs are rejected without contacting either provider. Matching is exact.

The native catalog is a generation input, not a startup dependency. The router
reads its generated final catalog from the configured path at startup. Refresh
it with `catalog generate` or `setup` when Codex's bundled models change.
When catalog ID import is enabled, generate `catalog.output_file` before starting
the service. Missing or malformed combined catalogs stop startup.

Native requests use the incoming `ChatGPT-Account-ID` header to choose ChatGPT
or the OpenAI API endpoint. Remote routes receive allowlisted headers and their
own configured credential; incoming native credentials are removed. The router
does not read Codex authentication files.

At startup, the ChatGPT upstream optionally reads proxy settings from
`$CODEX_HOME/.env`, defaulting to `~/.codex/.env`. Supported names are
`HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and `NO_PROXY`, including lowercase
variants. Existing process variables take precedence over file entries with the
same name; uppercase nonempty values take precedence over lowercase variants.
`ALL_PROXY` is the fallback when a scheme-specific proxy is unset.

A missing file or a file without proxy keys leaves existing transport behavior
unchanged. The file supports dotenv assignments, quoting, comments, and `export`;
it is parsed as data, never executed as a shell script. Unreadable or malformed
files stop startup with an error that does not include their contents. Only
proxy settings are used: other file variables are not exported or used as
credentials. File settings affect only the native ChatGPT upstream, including
its configured endpoint override; API-key and self-hosted routes retain their
existing process-environment proxy behavior. Restart the router after edits.

The listener and request peers must be loopback. Supported endpoints are
`GET /healthz`, `POST <base_path>/responses` plus Responses subpaths, and
`POST <base_path>/alpha/search` for the standalone web tool; the default base
path is `/v1`. Standalone web requests preserve their payload and use native
authentication routing, without requiring a model or contacting self-hosted routes. WebSocket upgrades return 426; HTTP fallback depends on the
client. The router requires a Responses API upstream and does not implement a
Chat Completions bridge.

Request bodies are bounded before and after gzip/zstd decompression. SSE is
flushed incrementally, and client cancellation propagates upstream. Provider
reasoning and compaction state handling is configurable; review those settings
before switching providers mid-conversation.

Request bodies and authentication headers are not intentionally logged. There is
no general secret-redaction filter. Request logs include model and route metadata
when enabled; inspect logs before sharing them. Success request logging defaults
to off, and log rotation is not provided.

## Documentation

- [Configuration reference](docs/configuration.md)
- [Codex integration](docs/codex-desktop.md)
- [LaunchAgent lifecycle](docs/launchd.md)
- [Systemd user service](docs/systemd.md)

Keep personal runtime files outside the checkout. Root-level `config.json` and
`catalog.json`, build outputs, and logs are ignored by Git.

## Development

```sh
export GOTOOLCHAIN=go1.27.1
prek run --all-files
prek run --all-files --hook-stage pre-push
go test -race -timeout 60s ./...
```

Pass new, untracked paths explicitly with `prek run --files <paths>` in both
stages until they are staged. Normal tests use local mocks and require no accounts.
See [AGENTS.md](AGENTS.md) for repository conventions. CI covers Ubuntu and macOS.
See [service verification](docs/service-verification.md) for the opt-in real
service-manager test.

## Releases

Releases use SemVer annotated tags (`vX.Y.Z`). Before `1.0.0`, breaking public
CLI, configuration, catalog, or service behavior increments the minor version;
compatible changes increment the patch version. [CHANGELOG.md](CHANGELOG.md) is
the sole release-note source: copy the released section into the GitHub release.

From clean, synchronized `main`, validate the exact release commit, create the
approved annotated tag, then package the tagged source:

```sh
scripts/package-release.sh
```

The script derives the binary version from `HEAD`'s exact tag and creates
darwin/arm64 and linux/amd64 archives plus `SHA256SUMS` in `dist/`. It refuses a
dirty worktree, missing SemVer tag, or existing artifact. Publishing the tag and
GitHub release remains a single explicit approval-gated action. Published `v*`
tags are immutable.

## License

Distributed under the [MIT License](LICENSE).
