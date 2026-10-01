# Codex setup

The router generates model catalogs and forwards requests. `setup
--configure-codex` can make its two required root-TOML edits after it has started
a healthy service. It is opt-in; without that flag, the program does not edit
Codex TOML.

## Configure

1. Run `codex-model-router setup --configure-codex`.
2. The final catalog defaults to
   `$XDG_DATA_HOME/codex-model-router/catalog.json`, or
   `~/.local/share/codex-model-router/catalog.json`.
3. The active Codex configuration is `$CODEX_HOME/config.toml` when `CODEX_HOME`
   is absolute, otherwise `~/.codex/config.toml`.
4. Setup preserves existing TOML except these root keys:

```toml
openai_base_url = "http://127.0.0.1:4317/v1"
model_catalog_json = "/absolute/path/to/codex-model-router/catalog.json"
```

Use the router's actual loopback address, fixed port, base path, and an absolute
catalog path. Setup creates `config.toml.codex-model-router.bak` with the exact
pre-change bytes before its first update, and preserves that backup on later
updates. It refuses a root `model_provider` override, including dotted/table
forms, until you explicitly remove or migrate it; this prevents silent routing
around `openai_base_url`. It does not modify authentication files. Restart Codex
to reload the catalog and select the desired model.

You can check catalog acceptance before editing:

```sh
catalog_data_home=${XDG_DATA_HOME:-"$HOME/.local/share"}; case "$catalog_data_home" in /*) ;; *) catalog_data_home="$HOME/.local/share" ;; esac
codex -c "model_catalog_json=\"$catalog_data_home/codex-model-router/catalog.json\"" debug models
```

## Rollback

Restore the two changed values or remove keys that setup added. The exact backup
is available for reference, but preserve later unrelated edits rather than
blindly replacing the whole file with it. Restart Codex before stopping or
uninstalling the router.

## Troubleshooting and verification

If every model fails, check the router listener and `healthcheck`. If the custom
model is absent, verify the catalog path and display name, then restart Codex.
Use `codex-model-router validate` to inspect exact model routing. The upstream
must support the Responses API; this router does not bridge Chat Completions.

Codex CLI 0.153.4 loaded a generated catalog and completed a read-only shell tool
call through a real SGLang upstream in an isolated configuration. WebSocket 426
triggered HTTP fallback. The desktop picker UI and real GPT upstream requests
remain unverified; recheck integration after Codex updates.
