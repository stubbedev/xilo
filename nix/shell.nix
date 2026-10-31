{
  perSystem = {
    goFor,
    hasWebToolchain,
    lib,
    pkgs,
    ...
  }: {
    # Loaded by direnv (`use flake` in .envrc).
    devShells.default = pkgs.mkShell {
      packages = with pkgs;
        [
          (goFor pkgs)
          gopls
          gotools # goimports
          golangci-lint
          air # live reload: `just dev`
          just
          sqlite # inspect the metadata db
          curl
        ]
        ++ lib.optionals hasWebToolchain [tailwindcss_4]; # admin CSS: `just css`

      # A GOROOT inherited from an older shell points the toolchain at a go
      # that is not the one on PATH ("compile: version ... does not match").
      # The nix go finds its own root; never carry one over.
      shellHook = ''
        unset GOROOT
      '';
    };

    formatter = pkgs.alejandra;
  };
}
