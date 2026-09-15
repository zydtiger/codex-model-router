# launchd service

On macOS the router runs as a **LaunchAgent**: a per-user job that starts at
login, restarts when the process dies, and never needs root.

The default installation uses your home directory and does not require `sudo`. `service preview` shows exactly what would be written, and
`service install --dry-run` shows the whole sequence without running it.

## Default paths

| Purpose                       | Default                                                            |
| ----------------------------- | ------------------------------------------------------------------ |
| LaunchAgent plist             | `~/Library/LaunchAgents/com.github.zydtiger.codex-model-router.plist` |
| Router logs                   | `~/Library/Logs/com.github.zydtiger.codex-model-router/`           |
| Binary the agent runs         | `~/.local/lib/codex-model-router/codex-model-router`               |
| Router configuration          | `~/.local/lib/codex-model-router/config.json`                      |
| Generated model catalog       | `~/.local/lib/codex-model-router/catalog.json`                     |
| Label                         | `com.github.zydtiger.codex-model-router`                           |
| launchd domain                | `gui/<uid>` of the current user                                    |

The binary path is deliberately outside any checkout: upgrading or deleting a
clone must not break a running agent. `service install` does **not** copy the
executable, so point `--bin` at a file you placed there yourself, or accept the
default only after installing a build at that path. The key names come from your
router configuration (`routes[].auth.api_key_env`, `native.api_key_env`) and are
passed with `--env`; nothing is hardcoded here.

`service status` names the log files and probes the router's health endpoint.

## Install

Follow [setup](../README.md#setup) to install the binary, edit `config.json`,
and generate `catalog.json` in the installation directory. The CLI defaults to
that `config.json`; `catalog.output_file` is `catalog.json`.

```sh
~/.local/lib/codex-model-router/codex-model-router service preview
~/.local/lib/codex-model-router/codex-model-router service install
```

Install runs `launchctl bootout gui/<uid>/<label>` first, to clear a previously
loaded copy of the same label, then writes the plist, then runs
`launchctl bootstrap gui/<uid> <plist>` and `launchctl kickstart -k gui/<uid>/<label>`.
Those are the argument shapes launchctl itself accepts: `bootout`, `kickstart`, and
`print` take one service target, `gui/<uid>/<label>`, while `bootstrap` takes a domain
target plus a plist path. A `bootout` that fails because nothing was loaded is a note;
any other failure stops the install before the plist is written. If `bootstrap` fails
the installer retries the observed transient `Bootstrap failed: 5: Input/output error`
response up to five total attempts with bounded backoff. Other errors fail immediately.
If loading still fails, the plist is left in place for inspection and retry, and
the command exits non-zero.

Flags: `--label`, `--bin`, `--config`, `--plist-dir`, `--log-dir`,
`--working-directory`, `--throttle-interval`, `--shutdown-timeout`,
`--env KEY=VALUE` (repeatable), `--force`, and `--dry-run`.

## Optional ChatGPT proxy settings

The service reads `~/.codex/.env` when it starts, or `$CODEX_HOME/.env` if
`CODEX_HOME` is supplied to the service. No proxy variables need to be copied
into the LaunchAgent when they are provided by this file. A missing file or no
proxy entries preserves existing behavior. File settings apply only to the
native ChatGPT upstream; see the README for supported variables and precedence.
Restart the service to load changes. The file does not supply route API keys.

## Keeping a key available

`routes[].auth.api_key_env` and `native.api_key_env` name environment variables,
and launchd does not inherit your interactive shell's environment, so a key that is
only in your shell is invisible to the service. Pass it at install time as a
`KEY=VALUE` pair — a bare name is rejected as a usage error, because there is nothing
to store:

```sh
codex-model-router service install --env SGLANG_API_KEY="$SGLANG_API_KEY"
```

The **value** is written into the plist, so it is readable by anything running as you
and by anyone with root. `service install` notices a value that looks like a secret and
writes the plist `0600`, and `service status` never prints the value. The same trade-off
applies to a root-level `EnvironmentVariables` dictionary you add by hand; keep that
plist `0600` as well and keep its values out of any bug report. There is no key file or
keychain lookup: the process can only read a key from its own environment.

## Status and logs

```sh
codex-model-router service status
```

It reports three things: the plist on disk, `launchctl print` state (including
PID and last exit code), and a probe against `http://<listen>/<healthz>`. A
router that answers but was not started by launchd is reported as such rather
than assumed to be the service.

Log files are `com.github.zydtiger.codex-model-router.out.log` and `.err.log` in
the log directory. `StandardErrorPath` is what usually has the interesting lines,
because startup failures are logged to stderr. The router never logs bodies or
authentication headers intentionally. Model IDs and route metadata appear in
request logs when enabled. There is no general redaction filter; inspect logs
before attaching them to an issue.

## Restart and uninstall

```sh
codex-model-router service uninstall            # bootout, then remove the plist
codex-model-router service uninstall --dry-run  # just report
```

Uninstall boots the job out and removes only a plist this tool wrote; if the file
on disk does not look like ours, it refuses without `--force`. If `bootout` reports a
service that is not loaded, the plist is still removed, because that is the state an
interrupted install leaves behind. Any other `bootout` failure is reported as a failure
and the plist is kept, so an interrupted uninstall never leaves a loaded job without a
matching file, and the command exits non-zero.

The agent has `RunAtLoad`, and `KeepAlive` set to restart on a crash or non-zero
exit but **not** on a clean exit. `--shutdown-timeout` controls the grace window
the router gives in-flight SSE streams when it is told to stop.

The router's config, the generated catalog, and your `~/.codex/config.toml` are
left alone by uninstall. Before stopping the router, undo its Codex configuration
changes yourself or through your agent and restart Codex; see
[rollback](codex-desktop.md#rollback).

## Other platforms

Linux uses a systemd user service through the same CLI commands. See
[systemd setup](systemd.md). macOS keeps stdout/stderr files under
`~/Library/Logs/<label>/`; Linux uses journald. There is no automatic rotation
for macOS log files.
