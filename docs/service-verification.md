# Service verification

CI runs the same repository hooks, Go tests, and vet on `ubuntu-latest` and
`macos-latest`. Lifecycle unit tests replace service-manager calls with a fake
runner, so CI does not install a persistent service or require a GUI login.

For a real service-manager check, build the binary and run:

```sh
GOTOOLCHAIN=go1.27.1 go build -o bin/codex-model-router ./cmd/codex-model-router
python3 scripts/service_smoke.py bin/codex-model-router
```

This opt-in test installs a temporary service with a unique label and a temporary
configuration on a free loopback port. It verifies install, reinstall, health,
crash recovery, logs, and uninstall. It preserves the configuration during
uninstall and cleans up its temporary files after success. If service cleanup
fails, it prints and preserves the working directory so cleanup can be retried.
No model request is sent to a real upstream. On Linux it also verifies the unit
with `systemd-analyze` and checks literal environment values in the child process.

Run from a logged-in macOS GUI session or a Linux session with an active systemd
user manager. On Linux, user-manager startup at boot without a login additionally
requires lingering; the script does not configure it. A full machine reboot or
logout/login is a separate manual check, not part of this script.
