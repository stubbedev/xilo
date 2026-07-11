{
  description = "xilo — self-hosted Nix binary cache";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in {
        # The xilo binary: client CLI and server (`xilo serve`) in one.
        packages.default = pkgs.buildGoModule {
          pname = "xilo";
          version = "0-unstable-${self.shortRev or "dirty"}";
          src = self;
          vendorHash = "sha256-IsVMraNNn2kaFnOEDXIIiQpag9Fr8RUo0em8F09TUHE=";
          subPackages = [ "cmd/xilo" ];
          nativeBuildInputs = [ pkgs.templ pkgs.tailwindcss_4 ];
          # Build the admin CSS (embedded via go:embed) then generate views.
          preBuild = ''
            sh scripts/build-css.sh
            templ generate
          '';
          env.CGO_ENABLED = 0; # sqlite via modernc.org, pure Go
          ldflags = [ "-s" "-w" "-X main.version=${self.shortRev or "dev"}" ];
          meta = {
            description = "Self-hosted Nix binary cache";
            homepage = "https://github.com/stubbedev/xilo";
            mainProgram = "xilo";
          };
        };

        # Dev shell: everything `just` recipes need.
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go # matches go.mod (toolchain auto-downloads if newer)
            gopls
            gotools # goimports
            golangci-lint
            templ # regenerate views: `just generate`
            tailwindcss_4 # admin CSS: `just css`
            air # live reload: `just dev`
            just
            sqlite # inspect the metadata db
            curl
          ];
          shellHook = ''
            echo "xilo dev shell — run 'just' to list recipes"
          '';
        };
      }) // {
      # NixOS module: `services.xilo.enable = true;` runs the server under
      # systemd and puts the client CLI in systemPackages. Config lives in
      # `settings` (rendered to YAML); secrets go in `environmentFile`
      # (XILO_ADMIN_PASSWORD, XILO_S3_ACCESS_KEY, XILO_S3_SECRET_KEY) so they
      # stay out of the world-readable Nix store.
      # Home-manager module: installs the CLI and (optionally) writes
      # ~/.config/xilo/xilo.yaml, which `xilo serve` picks up via XDG.
      # `xilo login` state (~/.config/xilo/config.yaml) is left unmanaged.
      homeModules.default = { config, lib, pkgs, ... }:
        let
          cfg = config.programs.xilo;
          settingsFormat = pkgs.formats.yaml { };
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
              type = settingsFormat.type;
              default = { };
              description = ''
                Server configuration written to
                {file}`$XDG_CONFIG_HOME/xilo/xilo.yaml` (used by `xilo serve`).
                Leave empty if this machine is client-only.
              '';
            };
          };

          config = lib.mkIf cfg.enable {
            home.packages = [ cfg.package ];
            xdg.configFile."xilo/xilo.yaml" = lib.mkIf (cfg.settings != { }) {
              source = settingsFormat.generate "xilo.yaml" cfg.settings;
            };
          };
        };
      homeManagerModules.default = self.homeModules.default;

      nixosModules.default = { config, lib, pkgs, ... }:
        let
          cfg = config.services.xilo;
          settingsFormat = pkgs.formats.yaml { };
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
              type = settingsFormat.type;
              default = { };
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
            environment.systemPackages = [ cfg.package ];

            systemd.services.xilo = {
              description = "xilo Nix binary cache";
              wantedBy = [ "multi-user.target" ];
              after = [ "network.target" ];
              serviceConfig = {
                ExecStart = "${lib.getExe cfg.package} serve --config ${configFile}";
                DynamicUser = true;
                StateDirectory = "xilo";
                Restart = "on-failure";
                RestartSec = 5;
              } // lib.optionalAttrs (cfg.environmentFile != null) {
                EnvironmentFile = cfg.environmentFile;
              };
            };
          };
        };
    };
}
