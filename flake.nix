{
  description = "Local Responses API model router for Codex";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      # Git tags are the version source, and a flake cannot read them. The
      # release commit a tag points at always adds its section to CHANGELOG.md,
      # so the newest released section there names the same version.
      version =
        let
          # Bracket expressions rather than escapes: `[[]` and `[]]` are the
          # portable way to match literal square brackets.
          released = "## [[]([0-9]+[.][0-9]+[.][0-9]+)[]].*";
          versions = lib.concatMap (
            line:
            let
              found = builtins.match released line;
            in
            lib.optionals (found != null) found
          ) (lib.splitString "\n" (builtins.readFile ./CHANGELOG.md));
        in
        if versions == [ ] then throw "CHANGELOG.md has no released section" else builtins.head versions;
      mkPackage =
        pkgs:
        pkgs.buildGo127Module {
          pname = "codex-model-router";
          inherit version;

          src = ./.;
          # Changes whenever go.mod or go.sum does; a build with a stale value
          # fails and prints the right one.
          vendorHash = "sha256-BA/+yNbhMZd+4FCQrVJKprqyD6RR5whh1kY3eH2WtJU=";

          subPackages = [ "cmd/codex-model-router" ];
          # The same static, stamped build scripts/package-release.sh produces.
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
            "-X github.com/zydtiger/codex-model-router/internal/routing.Version=v${version}"
          ];

          # The release-packaging tests drive scripts/package-release.sh, which calls Git.
          nativeCheckInputs = [ pkgs.git ];

          postInstall = ''
            install -Dm644 LICENSE "$out/share/licenses/codex-model-router/LICENSE"
            install -Dm644 README.md CHANGELOG.md -t "$out/share/doc/codex-model-router"
          '';

          doInstallCheck = true;
          installCheckPhase = ''
            runHook preInstallCheck
            test "$("$out/bin/codex-model-router" version)" = "codex-model-router v${version}"
            runHook postInstallCheck
          '';

          meta = {
            description = "Local Responses API model router for Codex";
            homepage = "https://github.com/zydtiger/codex-model-router";
            changelog = "https://github.com/zydtiger/codex-model-router/releases/tag/v${version}";
            license = lib.licenses.mit;
            platforms = lib.platforms.unix;
            mainProgram = "codex-model-router";
          };
        };
    in
    {
      packages = lib.genAttrs systems (
        system:
        let
          codex-model-router = mkPackage nixpkgs.legacyPackages.${system};
        in
        {
          inherit codex-model-router;
          default = codex-model-router;
        }
      );
    };
}
