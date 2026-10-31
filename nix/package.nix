{self, ...}: {
  perSystem = {
    goFor,
    hasWebToolchain,
    lib,
    pkgs,
    ...
  }: let
    # client = drop `xilo serve` (build tag `noserver`), which takes
    # internal/server out of the import graph and with it the need for
    # templ/Tailwind at build time.
    # `go` has to be overridden on buildGoModule itself, not passed as a
    # derivation attr, or the goModules (vendor) derivation keeps using
    # the default.
    mkXilo = {
      client,
      pkgs,
    }:
      (pkgs.buildGoModule.override {go = goFor pkgs;}) {
        pname =
          if client
          then "xilo-cli"
          else "xilo";
        version = "0-unstable-${self.shortRev or "dirty"}";
        src = lib.fileset.toSource {
          root = ../.;
          fileset = lib.fileset.difference (lib.fileset.gitTracked ../.) (
            lib.fileset.unions [
              ../flake.nix
              ../flake.lock
              ../nix
            ]
          );
        };
        vendorHash = "sha256-n6lm4ZLHkyvpX7RKwUl0/3cbV/hWN7wI1JLC1o00vw4=";
        # Hash the module cache (go mod download), not a vendor tree: `go mod
        # vendor` walks the import graph, so its output would depend on the
        # generated _templ.go (templui is imported only from codegen) and on
        # `tags` below — i.e. one vendorHash per package. This way both
        # packages share the single hash `just sync-vendor-hash` maintains.
        proxyVendor = true;
        subPackages = ["cmd/xilo"];
        tags = lib.optional client "noserver";
        nativeBuildInputs = lib.optionals (!client) [pkgs.tailwindcss_4];
        # Build the admin CSS (embedded via go:embed) then generate views.
        overrideModAttrs = _: _: {preBuild = "";};
        preBuild = lib.optionalString (!client) ''
          sh scripts/build-css.sh
          GOOS= GOARCH= go build -ldflags=-linkmode=internal -o "$TMPDIR/templ" github.com/a-h/templ/cmd/templ
          "$TMPDIR/templ" generate
        '';
        env.CGO_ENABLED = 0; # sqlite via modernc.org, pure Go
        ldflags =
          ["-s" "-w" "-X main.version=${self.shortRev or "dev"}"]
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
    xilo = mkXilo {
      client = false;
      inherit pkgs;
    };
    # Client CLI only (push/watch/login/use/…), no `xilo serve`, no admin
    # dashboard. Builds anywhere Go builds.
    xilo-cli = mkXilo {
      client = true;
      inherit pkgs;
    };
    # Full binary for riscv64, cross-compiled. tailwindcss is a
    # nativeBuildInputs and templ is a go tool built for the host (GOOS=
    # GOARCH= in preBuild), so both run on this (x86_64/aarch64) builder and
    # only Go targets riscv64 — which is how riscv64 gets the server
    # despite nixpkgs having no riscv64 tailwindcss_4. Needs a non-riscv64
    # builder (local, or a remote builder / substituter).
    xilo-riscv64 = mkXilo {
      client = false;
      pkgs = pkgs.pkgsCross.riscv64;
    };
  in {
    packages =
      {
        inherit xilo-cli;
        # `default` is the full binary wherever it can be built, so the NixOS
        # and home-manager modules keep working unchanged.
        default =
          if hasWebToolchain
          then xilo
          else xilo-cli;
      }
      // lib.optionalAttrs hasWebToolchain {inherit xilo;}
      # Cross to riscv64-linux only from a Linux builder: a darwin -> linux
      # cross set needs a whole foreign toolchain for no gain here.
      // lib.optionalAttrs (hasWebToolchain && pkgs.stdenv.hostPlatform.isLinux) {
        inherit xilo-riscv64;
      };
  };
}
