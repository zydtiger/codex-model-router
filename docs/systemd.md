# systemd user service

On Linux, `service preview`, `install`, `status`, and `uninstall` manage a
systemd **user** service. No root access is required. A running user manager
and its session bus must be available; run commands from your normal login
session. The installer does not enable lingering or alter system-wide services.

## Installation

Install the binary and your configuration into `~/.local/lib/codex-model-router/`.
Keep the generated catalog there as `config.catalog.json`. Preserve personal
configuration when replacing the binary.

```sh
~/.local/lib/codex-model-router/codex-model-router service preview \
  --config ~/.local/lib/codex-model-router/config.json
~/.local/lib/codex-model-router/codex-model-router service install \
  --config ~/.local/lib/codex-model-router/config.json
```

Units go in `$XDG_CONFIG_HOME/systemd/user/`, or `~/.config/systemd/user/`.
Use `--unit-dir` to override the directory for previews or testing. For normal
installation choose a directory in the user manager's unit search path.
The default unit is `com.github.zydtiger.codex-model-router.service`.

Installation writes the unit, reloads the manager, enables the unit, and restarts
it. Existing unrelated units require `--force` before replacement. A failed
systemctl operation is reported and the unit is retained for inspection/retry.
`--dry-run` performs no writes or manager operations.

The unit uses `Restart=on-failure` and a ten-second restart delay, configurable
with `--throttle-interval`. It starts with the user manager. Starting at boot
without a login requires separately configured user lingering; the installer
never changes that policy.

## Logs and status

```sh
journalctl --user -u com.github.zydtiger.codex-model-router.service
~/.local/lib/codex-model-router/codex-model-router service status \
  --config ~/.local/lib/codex-model-router/config.json
```

Both stdout and stderr go to the journal; retention and rotation follow the
host's journald configuration. `--log-dir` and `--plist-dir` are macOS-only.
Status reports manager state separately from the HTTP health probe.

Repeated `--env KEY=VALUE` flags add environment variables to the unit. Units
containing these variables are written with mode `0600`, but systemd environment
variables are not a secret store. Do not publish unit files containing credentials.

## Uninstall

```sh
~/.local/lib/codex-model-router/codex-model-router service uninstall \
  --config ~/.local/lib/codex-model-router/config.json
```

Uninstall disables and stops the unit, removes its definition, and reloads the
manager. The binary, configuration, generated catalog, and journal remain.
A stop/disable failure leaves the unit file available for recovery.
