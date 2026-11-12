# Home Manager module: installs the router, builds its configuration and model
# catalog, and runs it as a launchd agent on macOS or a systemd user service on
# Linux. The service uses the label `service install` uses, so `service status`
# reports it.
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.codex-model-router;
  json = pkgs.formats.json { };
  serviceName = "com.github.zydtiger.codex-model-router";
  home = config.home.homeDirectory;
  settingsFile = json.generate "router-settings.json" cfg.settings;
  # The catalog is generated and the configuration validated at build time, so a
  # bad configuration fails the build instead of the running service.
  bundle =
    pkgs.runCommand "codex-model-router-config"
      {
        nativeBuildInputs = [
          cfg.package
          pkgs.jq
        ];
      }
      ''
        export HOME="$TMPDIR/home"
        mkdir -p "$HOME" "$out"
        jq --arg output "$out/catalog.json" \
          '.catalog.native_catalog_file = "" | .catalog.output_file = $output' \
          ${settingsFile} > "$out/config.json"
        codex-model-router catalog generate --config "$out/config.json" \
          --codex ${cfg.codexPackage}/bin/codex
        codex-model-router validate --config "$out/config.json"
      '';
  arguments = [
    "${cfg.package}/bin/codex-model-router"
    "serve"
    "--config"
    "${bundle}/config.json"
  ];
in
{
  options.services.codex-model-router = {
    enable = lib.mkEnableOption "the Codex model router";

    package = lib.mkOption {
      type = lib.types.package;
      description = "codex-model-router package to install and run.";
    };

    codexPackage = lib.mkOption {
      type = lib.types.package;
      description = "Codex package whose bundled model catalog the generated catalog extends.";
    };

    settings = lib.mkOption {
      inherit (json) type;
      description = ''
        Router configuration, as in docs/configuration.md. The module sets
        `catalog.output_file` and clears `catalog.native_catalog_file`: the
        catalog is generated from `codexPackage` into the Nix store.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];
    xdg.configFile."codex-model-router/config.json".source = "${bundle}/config.json";
    xdg.dataFile."codex-model-router/catalog.json".source = "${bundle}/catalog.json";

    launchd.agents.codex-model-router = lib.mkIf pkgs.stdenv.hostPlatform.isDarwin {
      enable = true;
      config = {
        Label = serviceName;
        ProgramArguments = arguments;
        WorkingDirectory = home;
        RunAtLoad = true;
        KeepAlive = {
          Crashed = true;
          SuccessfulExit = false;
        };
        ThrottleInterval = 10;
        StandardOutPath = "${home}/Library/Logs/${serviceName}/${serviceName}.out.log";
        StandardErrorPath = "${home}/Library/Logs/${serviceName}/${serviceName}.err.log";
      };
    };

    home.activation.routerLogDirectory = lib.mkIf pkgs.stdenv.hostPlatform.isDarwin (
      lib.hm.dag.entryBetween [ "setupLaunchAgents" ] [ "writeBoundary" ] ''
        run mkdir -p ${lib.escapeShellArg "${home}/Library/Logs/${serviceName}"}
      ''
    );

    systemd.user = lib.mkIf pkgs.stdenv.hostPlatform.isLinux {
      services.${serviceName} = {
        Unit.Description = "Codex model router";
        Service = {
          Type = "exec";
          ExecStart = lib.escapeShellArgs arguments;
          WorkingDirectory = home;
          Restart = "on-failure";
          RestartSec = 10;
        };
        Install.WantedBy = [ "default.target" ];
      };
    };
  };
}
