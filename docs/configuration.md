# Configuration

The router reads one JSON file. There is no environment-variable configuration
besides the two lookup points described below, and there is no second config
format.

## Where the file lives

In order of precedence:

1. `--config <path>` on any subcommand.
2. `$CODEX_MODEL_ROUTER_CONFIG`.
3. `~/.local/lib/codex-model-router/config.json`.

The installation directory contains the binary, `config.json`, and generated
`catalog.json`. Set `catalog.output_file` to `catalog.json` so the output stays
beside the configuration. See [setup](../README.md#setup) for installation and
temporary native catalog inputs.

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
| `POST <base_path>/alpha/search` | standalone web tool; native upstream only, no model required |
| `GET /healthz`                       | health document; the one route outside `<base_path>`      |
| anything else                        | `404` (`405` for another method on a supported endpoint)    |
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
    "native_catalog_file": "/absolute/temporary/directory/native.json",
    "output_file": "catalog.json",
    "native_model_ids_from_catalog": true,
    "base_instructions_file": "",
    "description": "",
    "models": [
      {
        "id": "qwen3-32b",
        "route": "lab-qwen3-32b",
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
      "name": "lab-qwen3-32b",
      "base_url": "http://127.0.0.1:30000/v1",
      "models": ["qwen3-32b"],
      "auth": {
        "api_key_env": "LAB_QWEN3_API_KEY",
        "header": "Authorization",
        "scheme": "bearer"
      },
      "extra_headers": {},
      "reasoning": {
        "adapter": "reasoning_to_chat_template",
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
      "tools": {
        "namespace_adapter": "namespace_to_functions",
        "schema_loading": "on_demand"
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
| `routes[].tools`                    | see below  | Namespace tool translation; independent of `reasoning`  |
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
| `adapter`                              | `none`   | `none` or `reasoning_to_chat_template`                    |
| `supported_efforts`                    | inferred | Efforts this route accepts; keys the per-effort maps        |
| `unknown_effort`                       | `drop`   | `drop` uses the server default; `error` refuses the request |
| `forward_reasoning`                    | `false`  | Keep the `reasoning` object for the upstream                |
| `chat_template_kwargs`                 | unset    | Required by `reasoning_to_chat_template`                    |

The adapter only converts reasoning to `chat_template_kwargs`; it never changes
tool declarations. The former `sglang_chat_template` adapter name, which also
enabled namespace flattening and on-demand schema loading, is no longer
accepted: configuring it fails with a pointer to `reasoning.adapter:
"reasoning_to_chat_template"`, `tools.namespace_adapter:
"namespace_to_functions"`, and `tools.schema_loading: "on_demand"`, which is
the equivalent explicit configuration.

`adapter: "none"` leaves the reasoning fields unchanged, which is right for a
server that already speaks the Responses API: the `reasoning` object, including
`effort`, reaches that server as Codex sent it.

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

`reasoning_to_chat_template` deletes the `reasoning` object unless
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

## `routes[].tools`

| Key                 | Default | Meaning                                                      |
| ------------------- | ------- | ------------------------------------------------------------ |
| `namespace_adapter` | `none`  | `none` or `namespace_to_functions`                           |
| `custom_adapter`    | `none`  | `none` or `custom_to_functions`                              |
| `schema_loading`    | `all`   | `all` sends every flattened schema; `on_demand` adds a loader |

`namespace_adapter: "namespace_to_functions"` translates Responses `namespace`
tool groups into top-level `function` tools. This handles servers, including
SGLang versions whose Responses-to-chat conversion skips namespace groups, that
would otherwise make grouped MCP tools invisible even when their servers are
connected. The setting is independent of `routes[].reasoning`: a reasoning
adapter alone never flattens tools, and a tools adapter alone never touches
`chat_template_kwargs`. `schema_loading: "on_demand"` requires
`namespace_to_functions` and is refused at load time otherwise.

With `schema_loading: "all"` (the default), the whole flattened schema
inventory travels with every request and no loader tool exists.

With `schema_loading: "on_demand"`, the adapter initially sends core top-level
tools and a compact namespace
directory through a synthetic `router_load_tools` function. The model selects
one to eight namespace names; the router handles this schema-only call locally,
adds just those groups' function definitions, and continues the upstream
response. It never executes real tools: those still return to Codex with its
normal execution and approval boundaries. Loader calls and their results are
not exposed as executable calls to Codex.

Previously used groups are recovered from replayed function-call history.
Explicit `tool_choice` selections are loaded immediately, and `none` prevents
loader calls. Invalid selections return an internal error result so the model
can correct the names; there is no fallback that exposes the entire inventory.
At most four internal follow-up requests are allowed. New groups can require
an extra model pass; already-used groups do not require reloading. If a response
mixes loader and real calls, the real calls return immediately to Codex and the
loader calls are omitted; the next request can select additional groups again.

The adapter preserves parameter schemas and namespace/function descriptions.
Ordinary names become `<namespace>__<function>`. Collisions and names longer
than 64 bytes use short request-local aliases. The router restores the original
`name` and `namespace` in JSON responses and SSE function-call events, and maps
namespaced calls in replayed history and explicit `tool_choice` selectors back
to the upstream aliases. Call IDs, argument strings, and tool results are
preserved. Mapping state is isolated per request, including concurrent requests.

Only function children are supported; other namespace children return `400`
instead of silently disappearing. Native routes and other adapters keep their
existing tool representation. This implements router-managed schema discovery,
not OpenAI's native `tool_search` protocol. It does not add vision, code-mode or
remote-compaction support to a model.

`custom_adapter: "custom_to_functions"` bridges Responses custom tools, both
top-level declarations and custom children inside `namespace` groups, to plain
function tools for servers without custom-tool support. Each declaration
becomes a function with exactly one required string property `input`
(`additionalProperties: false`); the name and description are retained.
`format: {type: text}` and omitted formats map directly. A
`format: {type: grammar, syntax, definition}` travels verbatim in the function
description as model-facing guidance: the router performs no grammar engine,
constrained decoding, or grammar validation, so the upstream may still produce
input that does not satisfy the grammar. Other format types are refused with
`400`.

Replayed `custom_tool_call` and `custom_tool_call_output` history travels
upstream as function traffic with the exact input string, IDs, call IDs,
namespaces, and output data preserved; a historical call does not need an
active declaration in the current request. Explicit `custom` tool_choice
selectors, including entries inside `allowed_tools`, are translated to function
selectors; duplicate custom tool identities and selectors that name no declared
custom tool are refused rather than converted ambiguously. `none`, `auto`, and
`required` semantics are unchanged. Enabling the adapter requires
`input.custom_tools: "map_to_function_calls"` (the default); other policies are
rejected at load time. With the adapter off, existing behavior is unchanged
except that replayed custom calls now preserve their namespace.

Model-returned function calls are bridged back to `custom_tool_call` items only
when their `(namespace, name)` identity matches a custom declaration in the
current request; ordinary function tools are never reinterpreted. The bridged
`arguments` wrapper must be a JSON object containing exactly one string
`input`; a missing, mistyped, or extra-key wrapper fails with `502` (or aborts
an active stream) instead of emitting a malformed executable call. Responses
echo the original custom tool declarations and `tool_choice`. The adapter
composes with `namespace_to_functions` and both schema loading modes: custom
preparation runs before namespace flattening, and custom restoration runs after
namespace restoration. Compaction summary passes stay tool-free while custom
history survives summarization as function traffic.

In SSE streams, `response.output_item.added`/`done` items and the
`response.function_call_arguments` events of bridged calls are rewritten to
`custom_tool_call` items and `response.custom_tool_call_input.delta`/`done`
events. Call identity is tracked from the added item, so deltas that carry only
`item_id`/`output_index` are handled. Custom input is validated once the
complete arguments wrapper is available: the input is emitted as one delta
followed by the done event rather than streamed token by token, and per-call
argument buffering and retained call state share a `max_request_bytes` budget.
Each tracked call has a fixed metadata allowance as well as its string payload,
so even calls with empty input consume the budget. Suppressed
and injected events are compensated by renumbering `sequence_number`
monotonically across the adapted stream, and `event:` names stay in sync with
the rewritten payloads. A stream that ends with an incomplete bridged call, or
a completion that omits one, fails rather than reporting success.

Disclosure requires full replayed input, as used by Codex. Requests with
`previous_response_id` or `conversation` return `400`, because provider-managed
history cannot safely represent the router's hidden loading steps. Input-token
usage reports the last upstream pass's context size, rather than summing input
across loading passes and falsely triggering compaction. Output-token usage
includes every pass, and an explicit `max_output_tokens` budget is reduced
before each follow-up. Missing usage under that explicit budget stops discovery.

Adapted responses request identity encoding. JSON responses and individual SSE
events are bounded by `max_request_bytes`; the complete SSE stream is not
buffered. An invalid or oversized response fails with `502` before headers are
sent, or aborts an already-started stream. Initial upstream error responses pass
through; a failed internal follow-up produces `502` or aborts an active stream.

## `routes[].input`

### Item policies

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

The installed configuration and printed example set `catalog.output_file` to
`"catalog.json"`. `catalog generate --out <path>` overrides the configured output.
If both the flag and the configuration value are empty, generation writes JSON
to stdout; it does not choose another filename.

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
[the README](../README.md#setup). `route` must name one of `routes[]`,
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
  `codex-model-router validate` prints them absolute. Use `catalog.json` in the
  router configuration and its resolved absolute installation path for Codex's
  `model_catalog_json` setting.

## After editing

`codex-model-router validate` checks syntax, unknown keys, loopback safety, model
ID uniqueness, adapter names, policy values, and path existence, then prints the
compiled routing table. It is the fastest way to see what your configuration
actually means, and it never contacts an upstream:

```sh
codex-model-router validate
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

## Text checkpoints for remote compaction v2

A route can implement Codex remote compaction v2 using the same upstream model
for a text summary:

```json
"compaction": {
  "adapter": "text_summary",
  "max_output_tokens": 4096
}
```

The adapter is disabled when omitted. `max_output_tokens` defaults to 4096 and
accepts 256 through 32768; a smaller explicit request output limit still wins.
Setting a limit without the adapter is rejected. This is an output budget,
including upstream reasoning tokens, not a target summary length. The upstream
must complete with nonempty assistant text; refusals, tool calls, truncated
outputs, and incomplete streams fail without installing a checkpoint. Requests
that exceed the upstream context window also fail; the router does not silently
trim history to make a summary fit. Set the model's auto-compaction threshold
with enough headroom for the summarization prompt and output budget.

The adapter recognizes a single terminal `compaction_trigger` on the ordinary
`/responses` endpoint before input normalization. It asks the selected model for
a checkpoint with tools disabled, then returns one `compaction` output item.
The complete upstream summary is validated before any successful compaction
response is emitted. Codex owns replacing and persisting history, retaining user
messages, reinjecting its initial context, and recalculating active context usage.
The summary inference's usage is preserved; it is not fabricated as the smaller
post-compaction context usage.

The item's `encrypted_content` field contains a versioned, base64url-encoded JSON
text checkpoint (`cmr.compaction.v1:`). Despite the protocol field name, this is
**not encryption** and is not OpenAI-native compaction state. The payload is
self-contained: restart, resume, and fork do not require router memory or a
separate checkpoint database. On replay, the router expands its checkpoints into
ordinary user-context messages at the same history position, before generic
input policies can drop them. They never become system instructions. Payloads
are bounded to 256 KiB of decoded JSON; malformed or unsupported router payloads
fail explicitly. Repeated compaction summarizes the prior checkpoint text.

Expansion applies to recognized router checkpoints on all routes, including
native GPT routes, so switching from a summarized third-party conversation does
not forward a foreign opaque carrier to OpenAI. Native requests without router
checkpoints retain their existing behavior. Routes with `text_summary` enabled
reject foreign compaction items even when `input.compaction_items` is `drop`:
they cannot recover OpenAI's encrypted history and must not silently lose it.
Resume from an uncompacted history when moving such a conversation to that route.

This adapter requires full replayed input. `previous_response_id`, `conversation`,
and `context_management` are rejected on its compaction requests. Legacy
`/responses/compact` and server-side automatic `context_management` are not
implemented by this adapter. A v2 trigger on a route without the adapter returns
an explicit configuration error. Namespace schema disclosure continues normally
on subsequent task requests and is bypassed during summary generation.

For a Mac integration check using an actual Codex app-server and mock inference:

```sh
python3 scripts/compaction_smoke.py --codex /path/to/codex --router bin/codex-model-router
```

Use the executable bundled with the desktop version being validated. This check
uses an isolated temporary configuration and verifies manual compaction,
automatic compaction, restart/resume, fork, and replacement of old tool history.
It does not use a live model or modify the running installation.
