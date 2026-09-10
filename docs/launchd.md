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

```sh
go build -o bin/codex-model-router ./cmd/codex-model-router
mkdir -p ~/.local/lib/codex-model-router
cp bin/codex-model-router ~/.local/lib/codex-model-router/

codex-model-router service preview          # read the plist before installing
codex-model-router service install
```

Install runs `launchctl bootout gui/<uid>/<label>` first, to clear a previously
loaded copy of the same label, then writes the plist, then runs
`launchctl bootstrap gui/<uid> <plist>` and `launchctl kickstart -k gui/<uid>/<label>`.
Those are the argument shapes launchctl itself accepts: `bootout`, `kickstart`, and
`print` take one service target, `gui/<uid>/<label>`, while `bootstrap` takes a domain
target plus a plist path. A `bootout` that fails because nothing was loaded is a note;
any other failure stops the install before the plist is written. If `bootstrap` fails
the plist is left in place so you can read it and retry, and the command exits
non-zero.

Flags: `--label`, `--bin`, `--config`, `--plist-dir`, `--log-dir`,
`--working-directory`, `--throttle-interval`, `--shutdown-timeout`,
`--env KEY=VALUE` (repeatable), `--force`, and `--dry-run`.

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
left alone by uninstall. If you also want Codex to stop using the router, run
`codex-model-router codex-config restore` and restart Codex.

## Other platforms

macOS is the only platform with an installer here. On Linux, run `serve` under
your own supervisor and edit `~/.codex/config.toml` yourself;
`codex-model-router codex-config plan` and `codex-config snippet` tell you exactly
what to change, and `service preview` on macOS is the reference for the
`serve --config <path>` command line to reproduce:

```ini
[Service]
ExecStart=/usr/local/bin/codex-model-router serve --config /etc/codex-model-router/config.json
Restart=always
```

Keep it bound to loopback. The router refuses a non-loopback `listen.host` at
load time and again after `bind()`, and it checks every peer address, so a
systemd unit cannot accidentally expose it. If you need it reachable from
elsewhere, put it behind a proxy you control and authenticate there.
