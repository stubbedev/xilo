{
  description = "xilo — self-hosted Nix binary cache";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    # Explicit list, not flake-utils.lib.defaultSystems: that one still has
    # x86_64-darwin, which nixpkgs 26.11 dropped (it throws on eval). riscv64
    # gets the client only, since nixpkgs has no tailwindcss_4 there and the
    # admin CSS is a build-time input of the server.
    flake-utils.lib.eachSystem [
      "x86_64-linux"
      "aarch64-linux"
      "aarch64-darwin"
      "riscv64-linux"
    ] (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        lib = nixpkgs.lib;

        # Can we build the admin UI assets here? tailwindcss_4 throws on
        # platforms it has no binary for (riscv64-linux), so probe it.
        hasWebToolchain = (builtins.tryEval pkgs.tailwindcss_4.drvPath).success;

        # client = drop `xilo serve` (build tag `noserver`), which takes
        # internal/server out of the import graph and with it the need for
        # templ/Tailwind at build time.
        mkXilo = { client, pkgs }: pkgs.buildGoModule {
          pname = if client then "xilo-cli" else "xilo";
          version = "0-unstable-${self.shortRev or "dirty"}";
          src = self;
          vendorHash = "sha256-wBflrxxGT6PgB+krJAsrk+uxlRztlI6U5B7Fo0flRzY=";
          # Hash the module cache (go mod download), not a vendor tree: `go mod
          # vendor` walks the import graph, so its output would depend on the
          # generated _templ.go (templui is imported only from codegen) and on
          # `tags` below — i.e. one vendorHash per package. This way both
          # packages share the single hash `just sync-vendor-hash` maintains.
          proxyVendor = true;
          subPackages = [ "cmd/xilo" ];
          tags = lib.optional client "noserver";
          nativeBuildInputs = lib.optionals (!client) [ pkgs.templ pkgs.tailwindcss_4 ];
          # Build the admin CSS (embedded via go:embed) then generate views.
          preBuild = lib.optionalString (!client) ''
            sh scripts/build-css.sh
            templ generate
          '';
          env.CGO_ENABLED = 0; # sqlite via modernc.org, pure Go
          ldflags = [ "-s" "-w" "-X main.version=${self.shortRev or "dev"}" ]
            # Cross builds otherwise link externally through the target cc and
            # come out needing libc.so.6, unlike every native build here. Both
            # are pure Go (CGO off above), so ask for the internal linker and
            # get the same static binary on both paths.
            ++ lib.optional (pkgs.stdenv.hostPlatform != pkgs.stdenv.buildPlatform)
              "-linkmode=internal";
          meta = {
            description =
              if client
              then "Self-hosted Nix binary cache (client CLI only)"
              else "Self-hosted Nix binary cache";
            homepage = "https://github.com/stubbedev/xilo";
            mainProgram = "xilo";
          };
        };
        # The xilo binary: client CLI and server (`xilo serve`) in one.
        xilo = mkXilo { client = false; inherit pkgs; };
        # Client CLI only (push/watch/login/use/…), no `xilo serve`, no admin
        # dashboard. Builds anywhere Go builds.
        xilo-cli = mkXilo { client = true; inherit pkgs; };
        # Full binary for riscv64, cross-compiled. templ and tailwindcss are
        # nativeBuildInputs, so they run on this (x86_64/aarch64) builder and
        # only Go targets riscv64 — which is how riscv64 gets the server
        # despite nixpkgs having no riscv64 tailwindcss_4. Needs a non-riscv64
        # builder (local, or a remote builder / substituter).
        xilo-riscv64 = mkXilo { client = false; pkgs = pkgs.pkgsCross.riscv64; };
      in {
        packages = {
          inherit xilo-cli;
          # `default` is the full binary wherever it can be built, so the NixOS
          # and home-manager modules keep working unchanged.
          default = if hasWebToolchain then xilo else xilo-cli;
        } // lib.optionalAttrs hasWebToolchain { inherit xilo; }
          # Cross to riscv64-linux only from a Linux builder: a darwin -> linux
          # cross set needs a whole foreign toolchain for no gain here.
          // lib.optionalAttrs (hasWebToolchain && pkgs.stdenv.hostPlatform.isLinux) {
            inherit xilo-riscv64;
          };

        # Dev shell: everything `just` recipes need.
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go # matches go.mod (toolchain auto-downloads if newer)
            gopls
            gotools # goimports
            golangci-lint
            templ # regenerate views: `just generate`
            air # live reload: `just dev`
            just
            sqlite # inspect the metadata db
            curl
          ] ++ lib.optional hasWebToolchain tailwindcss_4; # admin CSS: `just css`
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
