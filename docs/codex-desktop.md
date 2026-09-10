# Desktop setup

This project is an unofficial integration. It is not affiliated with or endorsed
by OpenAI, and it changes a configuration file that Codex owns.

The change itself is small: two root keys in `~/.codex/config.toml`. Everything
else the router does happens at request time, so the change is easy to review and
easy to undo.

## Before you start

- **Back up first.** The file usually holds credentials and provider settings,
  even though this router does not read or write them. `codex-config apply` keeps
  its own backup and `codex-config restore` puts it back; a copy of your own costs
  nothing.
- **Have an escape hatch.** You can undo everything later with
  `codex-model-router codex-config restore`, or by hand by deleting the
  `openai_base_url` and `model_catalog_json` lines. Codex does not need to be
  reinstalled.
- **Know your endpoint.** A self-hosted server has to speak OpenAI's Responses
  API at `<base_url>/responses`. Check that before you touch Codex:

  ```sh
  curl -sS http://127.0.0.1:30000/v1/responses \
    -H 'Content-Type: application/json' \
    -d '{"model":"Qwen3-32B","input":"hi","stream":false}'
  ```

  If your server only speaks `/v1/chat/completions`, this router will not
  translate it. Use it as a check on what the server actually supports.

## Recommended path

1. **Generate the catalog** with the router (see the README).
2. **Preview the change**:

   ```sh
   codex-model-router codex-config plan
   ```

   It prints the current value and the new value for each key it touches, plus a
   note about anything it refuses to touch. It does not modify anything, and it
   prints the absolute path it resolved. Pass `--codex-config` if your file is
   elsewhere.

   If the file cannot be parsed, or if it holds a form the line editor cannot
   handle safely — a multi-line basic string on a root-level key, for example —
   plan fails with an explanation. It then suggests the snippet path rather than
   guessing at an edit.

3. **Apply it** only after reading the plan:

   ```sh
   codex-model-router codex-config apply --confirm
   ```

   Only root-level `openai_base_url` and `model_catalog_json` are written. A
   custom `model_provider` is removed with a note, because it overrides the URL
   pointer. Everything else, including the `[model_providers.*]` tables,
   `[projects.*]`, comments, spacing, and unknown keys, is left
   byte-for-byte intact.

   **If you did not run `codex-model-router service install`, start the router
   yourself.** Apply only writes pointers into Codex's config; it does not start
   anything, and a pointer with nothing listening behind it makes every model
   fail to connect, native ones included.

4. **Restart Codex** to reload the combined catalog, then select the model.
   Catalog registration and model display names are separate from profile selection.

## Manual path

Prefer this if you would rather see the change with your own editor than run an
apply step:

```sh
codex-model-router codex-config snippet
```

```toml
openai_base_url = "http://127.0.0.1:4317/v1"
model_catalog_json = "/absolute/path/to/model-catalog.json"
```

Paste them at the root of `~/.codex/config.toml`. If the file sets
`model_provider` at the root, that key takes precedence over the URL: delete it,
or point that provider's own `base_url` at the router. Restart Codex afterwards;
the model list is read at start-up.

## Troubleshooting

| Symptom                                       | Likely cause                                                 |
| --------------------------------------------- | ------------------------------------------------------------- |
| No models appear at all                       | Codex has not been restarted; or the catalog path is wrong     |
| `debug models` reports a missing key            | The catalog is from another Codex version; regenerate it      |
| Every model fails to connect                  | The router is not running, or `apply` ran without `service install` |
| A self-hosted model answers with a native reply | The ID is in `native.models` as well; `validate` shows the clash |
| `unknown model id`                            | The model is not in `routes[].models`; check for a suffix      |
| Reasoning model answers without thinking      | The adapter or its keys do not match your server               |
| Reasoning summary is missing                  | A custom route drops encrypted reasoning state by design       |
| Native login stops working                    | `native.preserve_client_auth` is false without a native key    |

`codex-model-router healthcheck` confirms the router is up and
`codex-model-router validate` prints exactly which model ID goes to which
upstream. Both exit non-zero on a problem, so they work in a shell script too.

## Validation gaps

Codex CLI 0.153.4 loaded a generated catalog and completed a read-only shell
tool call through a real SGLang upstream in an isolated configuration. `codex -c model_catalog_json="<catalog>" debug models` renders the native
entries and the generated one, with the picker fields intact, and generation itself
stops with a named key when a required field is missing.

What is not checked: the desktop window. No run against a real ChatGPT session or the
model picker UI was performed, so treat the session-facing steps above as untested
until you confirm them on your own machine, and re-check after a Codex update, since
the catalog entries and the endpoint behaviour come from the build you run.

If you do validate it, capture what the desktop asks for — the request path, and
which catalog keys it refuses — because that is the feedback that turns these
instructions into something checked.
