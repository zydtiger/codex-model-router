# Codex setup

The router generates model catalogs and forwards requests. Edit the active Codex
configuration yourself or through your agent; the program does not edit TOML,
create configuration backups, or restore them.

## Configure

1. Generate `~/.local/lib/codex-model-router/catalog.json` using the setup steps
   in the README. Keep it beside the installed binary and `config.json`.
2. Start the router and verify it with `codex-model-router healthcheck`.
3. Locate the active Codex configuration, normally `~/.codex/config.toml` or the
   configuration in your custom `CODEX_HOME`. Back it up before editing.
4. Set these keys at the root, before any TOML table headers:

```toml
openai_base_url = "http://127.0.0.1:4317/v1"
model_catalog_json = "/absolute/home/.local/lib/codex-model-router/catalog.json"
```

Use the router's actual loopback address, fixed port, and base path. Use an
absolute catalog path, replacing `/absolute/home` with your home directory.
For this native-authentication setup, remove any root
custom `model_provider` override; preserve unrelated settings and review the diff.
Do not modify authentication files. Restart Codex to reload the catalog and select
the desired model. Keep the router running for native models as well.

You can check catalog acceptance before editing:

```sh
codex -c "model_catalog_json=\"$HOME/.local/lib/codex-model-router/catalog.json\"" debug models
```

## Rollback

Restore the previous values of `openai_base_url`, `model_catalog_json`, and any
`model_provider` changed during setup, or remove keys that setup added. Preserve
later unrelated edits rather than blindly replacing the whole file with a backup.
Restart Codex before stopping or uninstalling the router.

## Troubleshooting and verification

If every model fails, check the router listener and `healthcheck`. If the custom
model is absent, verify the catalog path and display name, then restart Codex.
Use `codex-model-router validate` to inspect exact model routing. The upstream
must support the Responses API; this router does not bridge Chat Completions.

Codex CLI 0.153.4 loaded a generated catalog and completed a read-only shell tool
call through a real SGLang upstream in an isolated configuration. WebSocket 426
triggered HTTP fallback. The desktop picker UI and real GPT upstream requests
remain unverified; recheck integration after Codex updates.
