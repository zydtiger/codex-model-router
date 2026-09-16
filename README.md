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

Keep the runtime files together in `~/.local/lib/codex-model-router/`:
`codex-model-router`, `config.json`, and `catalog.json`. Install the build there;
upgrades replace only the binary and preserve your configuration and catalog.

```sh
router_dir="$HOME/.local/lib/codex-model-router"
mkdir -p "$router_dir"
cp bin/codex-model-router "$router_dir/codex-model-router.new"
mv "$router_dir/codex-model-router.new" "$router_dir/codex-model-router"
export PATH="$router_dir:$PATH"
```

On first setup, create `config.json` if it does not already exist:

```sh
test -e "$router_dir/config.json" ||
  codex-model-router catalog print-example --out "$router_dir/config.json"
```

Export the native catalog to a temporary directory when generating or refreshing
the combined catalog:

```sh
native_dir="$(mktemp -d)"
codex debug models --bundled > "$native_dir/native.json"
printf '%s\n' "$native_dir/native.json"
```

Edit the installed `config.json` before proceeding:

- Set `routes[].base_url` and exact `routes[].models` IDs for your server.
- Remove routes you do not use. If authentication is unnecessary, omit `auth`;
  otherwise set the environment variable named by `auth.api_key_env`.
- Set `catalog.native_catalog_file` to the absolute temporary path printed above.
  Keep `catalog.output_file` set to `catalog.json`, as in the example. Relative
  catalog paths resolve from the configuration file's directory.
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
codex -c "model_catalog_json=\"$router_dir/catalog.json\"" debug models
```

After successful generation, clear `catalog.native_catalog_file` to `""` in
`config.json` and remove the temporary directory with `rm -r "$native_dir"`.
The runtime reads the generated `catalog.json` instead of the native input.
Repeat the export and generation steps when you want to refresh native models.

The CLI defaults to the installed `config.json`, independent of the current
directory. `--config` or `CODEX_MODEL_ROUTER_CONFIG` can select another file.
Start the router in the foreground with `codex-model-router serve`, or use the
platform service commands below.

With the router running, edit the active Codex configuration yourself or ask your
agent to do it. Back up the file and review the diff. Set these root keys:

```toml
openai_base_url = "http://127.0.0.1:4317/v1"
model_catalog_json = "/absolute/home/.local/lib/codex-model-router/catalog.json"
```

Replace `/absolute/home` with your home directory and use the actual listener URL.
For native authentication, remove a root custom `model_provider` override if
present, preserving unrelated
settings. The router does not edit Codex configuration or manage its backups.
Restart Codex to reload the catalog. Keep the router running while Codex points
at it, including for native models. See [Codex setup](docs/codex-desktop.md).

For login startup and crash recovery, use `service preview`, `service install`,
`service status`, and `service uninstall` with the installed binary.
See [launchd setup](docs/launchd.md) or [systemd setup](docs/systemd.md) for paths
and environment handling.
Service configuration belongs to this repository. No Docker container is required.

## Routing and limits

Configured remote model IDs go to their assigned upstream. Explicit native IDs,
including non-remote IDs imported from the combined catalog, go to the native upstream.
Unknown IDs are rejected without contacting either provider. Matching is exact.

The native catalog is a generation input, not a startup dependency. Deploy the
binary, `config.json`, and generated `catalog.json` in the installation directory.
Export a fresh temporary native catalog from Codex when regenerating.
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

Keep personal runtime files in the installation directory, outside the checkout.
Root-level `config.json` and `catalog.json`, build outputs, and logs are ignored
by Git.

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

License, versioning, and release approval have not been established.
