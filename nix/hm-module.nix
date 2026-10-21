# Home-manager module: installs the CLI and (optionally) writes
# ~/.config/xilo/xilo.yaml, which `xilo serve` picks up via XDG.
# `xilo login` state (~/.config/xilo/config.yaml) is left unmanaged.
{self, ...}: {
  flake = {
    homeModules.default = {
      config,
      lib,
      pkgs,
      ...
    }: let
      cfg = config.programs.xilo;
      settingsFormat = pkgs.formats.yaml {};
    in {
      options.programs.xilo = {
        enable = lib.mkEnableOption "xilo, a self-hosted Nix binary cache";

        package = lib.mkOption {
          type = lib.types.package;
          default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
          defaultText = lib.literalExpression "xilo.packages.\${system}.default";
          description = "The xilo package to use.";
        };

        settings = lib.mkOption {
          inherit (settingsFormat) type;
          default = {};
          description = ''
            Server configuration written to
            {file}`$XDG_CONFIG_HOME/xilo/xilo.yaml` (used by `xilo serve`).
            Leave empty if this machine is client-only.
          '';
        };
      };

      config = lib.mkIf cfg.enable {
        home.packages = [cfg.package];
        xdg.configFile."xilo/xilo.yaml" = lib.mkIf (cfg.settings != {}) {
          source = settingsFormat.generate "xilo.yaml" cfg.settings;
        };
      };
    };
    homeManagerModules.default = self.homeModules.default;
  };
}
