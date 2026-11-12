# Home Manager module

The flake's `homeManagerModules.default` installs the router, builds its
configuration and model catalog, and runs it as a LaunchAgent on macOS or a
systemd user service on Linux. Use it instead of `setup` and `service install`
when Home Manager owns the machine's configuration.

```nix
{
  imports = [ inputs.codex-model-router.homeManagerModules.default ];
  services.codex-model-router = {
    enable = true;
    # The Codex whose bundled models the generated catalog extends.
    codexPackage = pkgs.codex;
    settings = builtins.fromJSON (builtins.readFile ./router.json);
  };
}
```

| Option         | Purpose                                                         |
| -------------- | --------------------------------------------------------------- |
| `enable`       | Install the package, the configuration, and the service         |
| `package`      | Router package; defaults to the one this flake builds           |
| `codexPackage` | Codex package that exports the bundled model catalog            |
| `settings`     | Router configuration, as in [configuration](configuration.md)   |

The build generates the catalog with `catalog generate` and checks the result
with `validate`, so an invalid configuration fails the build rather than the
running service. The module sets `catalog.output_file` and clears
`catalog.native_catalog_file` itself. The configuration is linked at
`$XDG_CONFIG_HOME/codex-model-router/config.json` and the catalog at
`$XDG_DATA_HOME/codex-model-router/catalog.json`; both are read-only store
files, so change `settings` and switch instead of editing them.

The service uses the default label, `com.github.zydtiger.codex-model-router`,
so `codex-model-router service status` reports it. Do not also run
`service install` for the same label. Settings are stored in the world-readable
Nix store: keep keys out of them and name environment variables instead.
