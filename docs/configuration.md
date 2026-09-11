# Configuration

The router reads one JSON file. There is no environment-variable configuration
besides the two lookup points described below, and there is no second config
format.

## Where the file lives

In order of precedence:

1. `--config <path>` on any subcommand.
2. `$CODEX_MODEL_ROUTER_CONFIG`.
3. `$XDG_CONFIG_HOME/codex-model-router/config.json`, or
   `~/.config/codex-model-router/config.json`.

`codex-model-router catalog print-example --out <path>` writes a documented
starting point. `codex-model-router validate` checks a file without opening a
port and prints the routing table it compiled.

Unknown keys are an error, not a warning. A misspelled key is how a security
setting silently stops applying, so the parser rejects the file instead.

## Endpoint map

These are the only paths the router serves, and knowing them explains most of the
error messages:

| Request                              | Result                                                  |
| ------------------------------------ | -------------------------------------------------------- |
| `POST <base_path>/responses`         | routed to the selected upstream            |
| `POST <base_path>/responses/*`       | routed as well, for a future subresource                  |
| `GET /healthz`                       | health document; the one route outside `<base_path>`      |
| anything else                        | `404` (`405` for another method on the responses path)    |
| an `Upgrade: websocket` request      | `426 Upgrade Required`                                    |
| `Host` that is not loopback          | `421 Misdirected Request`                                 |
| a non-loopback peer address          | `403 Forbidden`                                           |

## Complete example

```json
{
  "listen": { "host": "127.0.0.1", "port": 4317 },
  "base_path": "/v1",
  "max_request_bytes": 33554432,
  "log": { "level": "info", "requests": false },
  "native": {
    "chatgpt_base_url": "https://chatgpt.com/backend-api/codex",
    "api_base_url": "https://api.openai.com/v1",
    "preserve_client_auth": true,
    "models": []
  },
  "catalog": {
    "native_catalog_file": "/absolute/path/to/native-catalog.json",
    "output_file": "/absolute/path/to/model-catalog.json",
    "native_model_ids_from_catalog": true,
    "base_instructions_file": "",
    "description": "",
    "models": [
      {
        "id": "qwen3-32b",
        "route": "sglang",
        "display_name": "Qwen3 32B (lab)",
        "description": "Self-hosted on the lab GPU box",
        "context_window": 131072,
        "input_modalities": ["text"],
        "default_reasoning_level": "medium",
        "tool_capable": true,
        "base_instructions": "",
        "extra_fields": {}
      }
    ]
  },
  "routes": [
    {
      "name": "sglang",
      "base_url": "http://127.0.0.1:30000/v1",
      "models": ["qwen3-32b"],
      "auth": {
        "api_key_env": "SGLANG_API_KEY",
        "header": "Authorization",
        "scheme": "bearer"
      },
      "extra_headers": {},
      "reasoning": {
        "adapter": "sglang_chat_template",
        "supported_efforts": ["none", "low", "medium", "high"],
        "unknown_effort": "drop",
        "forward_reasoning": false,
        "chat_template_kwargs": {
          "enable_thinking": { "none": false, "low": true, "medium": true, "high": true },
          "reasoning_effort": { "low": "low", "medium": "medium", "high": "high" },
          "preserve_thinking": true
        }
      },
      "input": {
        "reasoning_items": "drop",
        "compaction_items": "drop",
        "custom_tools": "map_to_function_calls",
        "developer_role_as_system": true,
        "unknown_items": "drop"
      },
      "response_header_timeout_seconds": 120
    }
  ]
}
```

## `listen`

| Key            | Default       | Meaning                                                        |
| -------------- | ------------- | -------------------------------------------------------------- |
| `listen.host`  | `127.0.0.1`   | Bind address. Must resolve to loopback; anything else is refused |
| `listen.port`  | `4317`        | Port. `0` picks a free port, which only works for one-off runs   |

`host: "localhost"` is accepted and resolves to loopback. `::1` binds the IPv6
loopback. A LAN address is rejected at load time and again after `bind()`.

A port of `0` chooses a random port; `healthcheck` then needs `--port`.
Use a fixed port for a service and for the URL configured in Codex. The router
does not validate or edit Codex's TOML file; see [Codex setup](codex-desktop.md).

## `base_path`

The URL prefix the router serves, and the suffix inside the
`openai_base_url` you hand to Codex. It defaults to `/v1`, which is what Codex
expects; setting it to `/` is the escape hatch for a build that appends
`/v1/responses` itself, and then this router serves `/responses`.

The path must start with `/` and must not contain a query, fragment, host, or
percent escapes. Keep `base_path` and the router's own URL prefix identical:
Codex appends `responses` to the URL and the router matches on the configured
prefix, so the two only have to agree with each other. Check that agreement with
`codex-model-router validate`, which prints the exact URL a request would take;
if Codex reports a 404 for `/v1/responses`, that printout is where to compare.

## `max_request_bytes`

Default 33554432 (32 MiB). It bounds the request body *after* gzip or zstd
decompression, and the number of simultaneous zstd contexts is capped separately.
A request over the limit gets `413`. The limit is enforced on the
streaming read, not on `Content-Length`, so a lying header does not help.

## `log`

| Key            | Default | Meaning                                                     |
| -------------- | ------- | ------------------------------------------------------------ |
| `log.level`    | `info`  | `debug`, `info`, `warn`, or `error`                          |
| `log.requests` | `false` | One line per request: method, path, model, route, status, duration |

Bodies and headers are never logged, at any level. Values in `--log-level` and
`log.level` are the only knobs. Model IDs and route metadata appear in the
per-request line; the log is written to stderr, which launchd redirects to your
own log directory (see [launchd](launchd.md)), so keep that directory off shared
storage. `service install` writes the plist `0600` when `--env` carries a key.

## `native`

These fields control the trusted upstreams, which are the only destination that
may ever see a client credential.

| Key                        | Default                                     | Meaning                                                     |
| -------------------------- | ------------------------------------------- | ------------------------------------------------------------ |
| `native.chatgpt_base_url`  | `https://chatgpt.com/backend-api/codex`     | Account-backed endpoint                                       |
| `native.api_base_url`      | `https://api.openai.com/v1`                 | Plain API-key endpoint                                        |
| `native.preserve_client_auth` | `true`                                   | Forward the client's credential, to native upstreams only      |
| `native.api_key_env`       | unset                                       | Environment variable holding a fallback native key            |
| `native.models`            | `[]`                                        | Model IDs that must go native                                 |

`preserve_client_auth: true` keeps the incoming `Authorization` and `Cookie`
headers and sends them, plus `ChatGPT-Account-ID`, `X-Client-Request-Id`,
`X-Session-Id`, and `Version`, to the native upstream. With `false` the router
refuses the request unless `api_key_env` provides a key. `false` requires https
URLs; a loopback URL is allowed for a local mock.

There is deliberately no setting that sends unlisted IDs native: an ID that matches
no route and no native list is refused with `400` and nothing is sent anywhere, so a
self-hosted ID cannot borrow a native credential. Matching is exact, including case
and surrounding whitespace. A configured native URL is used as-is for `/responses`: if you
point it at `http://127.0.0.1:30000/v1` it receives `POST /v1/responses`, and if
you point at `http://127.0.0.1:30000` with no suffix it receives
`POST /responses`. The default `https://chatgpt.com/backend-api/codex` appends
`/responses`, which is what the ChatGPT backend exposes, so leave the two default
values alone unless you route native models somewhere else.

Set `api_key_env` (not a literal key) if you want native requests to work when
the client has no account credential. The router reads that variable when it
starts, which under launchd means the launchd environment (see
[launchd](launchd.md)); it never logs the value.

## `routes[]`

Each route is one self-hosted server. Model IDs are matched exactly,
case-sensitively, and an ID may not appear in two places: routing is decided
before any request is parsed, so an ID that is both routed and native is rejected
at load time, as are two routes claiming the same ID.

| Key                                 | Default    | Meaning                                                  |
| ----------------------------------- | ---------- | --------------------------------------------------------- |
| `routes[].name`                     | required   | Label used in logs and `/healthz`                        |
| `routes[].base_url`                 | required   | Server root; `/responses` is appended to it              |
| `routes[].models`                   | required   | Exact model IDs served by this route                     |
| `routes[].auth.api_key_env`         | unset      | Variable holding this route's key                        |
| `routes[].auth.header`              | `Authorization` | Header the credential is sent in                  |
| `routes[].auth.scheme`              | `bearer`   | `bearer` prefixes `Bearer `; `header` sends the raw value |
| `routes[].extra_headers`            | `{}`       | Fixed headers added to every request to this route        |
| `routes[].reasoning`                | `none`     | Reasoning translation; see below                         |
| `routes[].input`                    | see below  | Policy for items a generic server cannot use             |
| `routes[].response_header_timeout_seconds` | `120` | Bound on waiting for response headers; `-1` disables |

`base_url` may be `http://127.0.0.1:30000`, `http://127.0.0.1:30000/v1`, or an
`https://` URL for a hosted endpoint. It must carry a scheme and a host, and a
query, fragment, or embedded userinfo is rejected. A path segment is kept, so a
server mounted under `/v1` works without a rewrite, and it is used verbatim: a
`base_url` of `http://127.0.0.1:30000/v1` receives `POST /v1/responses`.

Plain `http` is accepted for any host, because a lab server on a private network
is the ordinary case. That is a decision you should make on purpose: if the route
has a key in `auth.api_key_env`, that key travels over whatever connection
`base_url` describes, so use `https` or your own tunnel once the host is not your
own machine.

`routes[].auth.api_key_env` names an environment variable the router reads when
it starts. If the variable is unset, requests for that route get `503` with
`no credential is configured for this router` rather than an anonymous request.
`extra_headers` cannot override `Authorization`, `X-Api-Key`, `Cookie`, or
`Host`; put those concerns in `auth` or drop them.

`response_header_timeout_seconds` bounds only the wait for upstream headers,
never the generation itself: headers arrive before the model finishes thinking,
so timing out there would kill working requests. Zero uses 120 seconds and `-1`
disables the bound.

## `routes[].reasoning`

| Key                                    | Default  | Meaning                                                    |
| -------------------------------------- | -------- | ------------------------------------------------------------ |
| `adapter`                              | `none`   | `none` or `sglang_chat_template`                            |
| `supported_efforts`                    | inferred | Efforts this route accepts; keys the per-effort maps        |
| `unknown_effort`                       | `drop`   | `drop` uses the server default; `error` refuses the request |
| `forward_reasoning`                    | `false`  | Keep the `reasoning` object for the upstream                |
| `chat_template_kwargs`                 | unset    | Required by `sglang_chat_template`                          |

`adapter: "none"` forwards the body unchanged, which is right for a server that
already speaks the Responses API: the `reasoning` object, including `effort`,
reaches that server as Codex sent it.

`chat_template_kwargs` values are either literal, sent unchanged for every
effort, or per-effort maps keyed by effort. A key mapped to `null` is omitted for
that effort, which is how `"enable_thinking": {"none": false}` turns thinking off
instead of merely adding a key. Unknown keys in `chat_template_kwargs` are an
error at load time, so a typo cannot quietly do nothing. `reasoning_effort` is
the string the chat template reads; it is a template variable and not
independently validated against your server's vocabulary, so check what your
build expects before relying on it.

With `supported_efforts` set, an effort outside that list follows
`unknown_effort`: `drop` uses the server default, `error` refuses the request
with `400`. Leaving it unset uses the keys present in a per-effort map, or every
effort when all values are literal.

`sglang_chat_template` deletes the `reasoning` object unless
`forward_reasoning: true`, because some strict servers reject a top-level field
they do not model; the effort is then expressed only through
`chat_template_kwargs`. Independently of the adapter, an `include` value naming
encrypted provider state (`reasoning.encrypted_content`, or anything containing
`reasoning`) is removed from routed requests: that server did not produce the
state and cannot return it. Both behaviours are covered by tests.

To check what your server expects, send the request with `curl` and watch the
server log, or ask the server for its template:

```sh
curl -s http://127.0.0.1:30000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen3-32B","messages":[{"role":"user","content":"hi"}],
       "chat_template_kwargs":{"enable_thinking":false},"stream":false}'
```

If the reply changes with `enable_thinking`, the router injects that key for the
efforts you map to `false`.

## `routes[].input`

Codex sends conversation state that a generic server cannot interpret. Each
policy is `drop` or `reject`, and `reject` returns `400` naming the item type. A
drop logs a warning the first time it happens.

| Key                        | Default                 | Allowed values                       | Guards                                                |
| -------------------------- | ----------------------- | ------------------------------------ | ----------------------------------------------------- |
| `reasoning_items`          | `drop`                  | `drop`, `keep`, `reject`             | `reasoning` items carrying `encrypted_content`         |
| `compaction_items`         | `drop`                  | `drop`, `reject`                     | `compaction` items                                     |
| `custom_tools`             | `map_to_function_calls` | `map_to_function_calls`, `keep`, `drop`, `reject` | `custom_tool_call` and `custom_tool_call_output` items |
| `developer_role_as_system` | `true`                  | any boolean                          | Rewrites the `developer` role to `system`              |
| `unknown_items`            | `drop`                  | `drop`, `keep`, `reject`             | Item types the router does not model                   |

`encrypted_content` is opaque state from a *different* provider: the target
server cannot decrypt it, and replaying it can be rejected outright. `drop` is
the safe default; `keep` sends it through, which is only useful when the upstream
is the same provider that produced it. The cost of `drop` is that cross-turn
reasoning context does not carry over, which is a deliberate loss rather than
something a proxy can fix. `unknown_items` exists because Codex may add item
types after this release: dropping keeps a session working, and `reject` is the
choice when silent loss is worse than a visible failure.

## `catalog`

| Key                               | Default | Meaning                                                     |
| --------------------------------- | ------- | ------------------------------------------------------------ |
| `catalog.native_catalog_file`     | `""`    | `codex debug models --bundled` output to merge with           |
| `catalog.output_file`             | `""`    | Where `catalog generate` writes                               |
| `catalog.native_model_ids_from_catalog` | `true` | Import non-remote slugs from the combined output at startup          |
| `catalog.base_instructions_file`  | `""`    | File used for `base_instructions`                             |

`base_instructions` is not optional in practice. Codex requires `base_instructions` or
`model_messages.instructions` on every entry, and the Codex model cache carries neither
for the entries it stores, so with a real cache each local model needs its own
`base_instructions` (or one shared file through `base_instructions_file`) or generation
refuses to write the file.
| `catalog.description`             | `""`    | Default picker description for generated entries              |
| `catalog.models`                  | `[]`    | The self-hosted entries                                       |

Each entry in `catalog.models` needs `id` and `route`. `display_name`,
`context_window`, `default_reasoning_level`, `tool_capable`,
`priority`, `description`, and `base_instructions` are described in
[the README](../README.md#model-catalog). `route` must name one of `routes[]`,
which is what ties picker entry to upstream. `id` must not also appear in
`native.models`. Picker reasoning levels come from the route’s
`reasoning.supported_efforts`, in the declared order. Declare that list once for
both request validation and catalog generation; the former catalog model field
`reasoning_levels` is no longer accepted. Levels are checked against `none`, `minimal`, `low`,
`medium`, `high`, and `xhigh`, and `default_reasoning_level` must be one of them;
`extra_fields` merges arbitrary keys into the entry for a server that needs
something this program does not model.

Generation is deterministic and refuses to write a catalog that would replace a
native model.

## Paths and permissions

- The router reads only the file it is pointed at. It never writes to its own
  configuration. `catalog print-example` writes a new example; catalog generation,
  and service install/uninstall also write their target files.
- Configuration carries environment variable *names*, never secret values. The
  values come from the process environment, which under launchd means the plist
  (see [launchd](launchd.md)).
- `catalog.native_catalog_file`, `catalog.output_file`,
  and `catalog.base_instructions_file` paths are expanded
  (`~`) and resolved relative to the configuration file's directory, and
  `codex-model-router validate` prints them absolute. `catalog.output_file` must
  be an absolute path, because Codex has to read it from wherever it runs.

## After editing

`codex-model-router validate` checks syntax, unknown keys, loopback safety, model
ID uniqueness, adapter names, policy values, and path existence, then prints the
compiled routing table. It is the fastest way to see what your configuration
actually means, and it never contacts an upstream:

```sh
codex-model-router validate --config /path/to/config.json
```

## Migration and rollback

There is no stored state to migrate: the config is read at start-up and
`catalog generate` overwrites `output_file` atomically, keeping the old bytes
until the new file is fully written. `service install` overwrites the plist only
if the existing file was written by this tool, otherwise it refuses without
`--force`.

Undo the Codex configuration changes manually or through your agent, preserving
unrelated edits, and restart Codex before stopping the router. Then use
`codex-model-router service uninstall` to remove its LaunchAgent. Uninstall leaves
router configuration and catalogs alone. See [rollback](codex-desktop.md#rollback).

## Use an HTTP proxy you control

The router sends upstream requests through the standard library transport, so the
usual `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` variables are honoured, and it
does not send credentials anywhere you did not list as an upstream. If you put a
proxy between the router and a native upstream, keep it inside your own machine
or network, and keep credentials out of its URL.

## Errors you will see

The `error.code` and message below come from the router itself, so you can match
on the code in a script.

| Code                              | Message you will see                                              | Cause                                                    |
| --------------------------------- | ------------------------------------------------------------------- | --------------------------------------------------------- |
| `unknown_model`                   | `model "<id>" is not served by this router; add a route for it or list it under native.models` | The ID is in neither place                |
| `model_required`                  | `the request body must name a model`                                 | The body is valid JSON with no `model`                    |
| `route_unavailable`               | `route "<name>" is not available`                                    | The config changed underneath a running router            |
| `route_credentials_missing`       | names the variable                                                   | `auth.api_key_env` points at an unset variable            |
| `native_credentials_missing`      | `native.api_key_env names $<VAR>, which is not set`                  | A native request had no client credential to forward      |
| `request_too_large`               | `request body exceeds N bytes`                                       | Raise `max_request_bytes`                                 |
| `invalid_reasoning`               | `reasoning effort "<x>" is not supported by this route`              | `unknown_effort: "error"` with an unmapped effort          |
| `upgrade_required`                | `this router serves the HTTP Responses transport`                    | The client tried WebSocket; it should retry over HTTP      |
| `host_not_loopback`               | `the Host header must name the loopback address`                     | Something reached the listener by another name             |

Configuration errors are reported together at load time, each prefixed with the
JSON key it belongs to, and `validate` prints them without opening a port.

The native catalog and base instructions file are read only during generation.
Service startup reads `catalog.output_file` when catalog ID import is enabled,
excluding IDs assigned to remote routes. Generate that file before starting the
service; native input files need not be installed. Route/native collisions in
the generation input are rejected during catalog generation.
