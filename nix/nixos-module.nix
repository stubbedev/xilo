# NixOS module: `services.xilo.enable = true;` runs the server under
# systemd and puts the client CLI in systemPackages. Config lives in
# `settings` (rendered to YAML); secrets go in `environmentFile`
# (XILO_ADMIN_PASSWORD, XILO_S3_ACCESS_KEY, XILO_S3_SECRET_KEY) so they
# stay out of the world-readable Nix store.
{self, ...}: {
  flake.nixosModules.default = {
    config,
    lib,
    pkgs,
    ...
  }: let
    cfg = config.services.xilo;
    settingsFormat = pkgs.formats.yaml {};
    configFile = settingsFormat.generate "xilo.yaml" cfg.settings;
  in {
    options.services.xilo = {
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
        example = lib.literalExpression ''
          {
            listen = ":8080";
            base_url = "https://cache.example.com";
            gc.interval = "12h";
          }
        '';
        description = ''
          Server configuration, rendered to xilo.yaml.
          See xilo.example.yaml for all keys. Do not put secrets here —
          use {option}`services.xilo.environmentFile`.
        '';
      };

      environmentFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        example = "/run/secrets/xilo.env";
        description = ''
          systemd EnvironmentFile with secrets, e.g.
          XILO_ADMIN_PASSWORD=... (also XILO_S3_ACCESS_KEY /
          XILO_S3_SECRET_KEY for S3 storage).
        '';
      };
    };

    config = lib.mkIf cfg.enable {
      services.xilo.settings = {
        listen = lib.mkDefault ":8080";
        data_dir = lib.mkDefault "/var/lib/xilo";
      };

      # Client CLI (xilo push/watch/login/…) for everyone on the box.
      environment.systemPackages = [cfg.package];

      systemd.services.xilo = {
        description = "xilo Nix binary cache";
        wantedBy = ["multi-user.target"];
        after = ["network.target"];
        serviceConfig =
          {
            ExecStart = "${lib.getExe cfg.package} serve --config ${configFile}";
            DynamicUser = true;
            StateDirectory = "xilo";
            Restart = "on-failure";
            RestartSec = 5;
          }
          // lib.optionalAttrs (cfg.environmentFile != null) {
            EnvironmentFile = cfg.environmentFile;
          };
      };
    };
  };
}
