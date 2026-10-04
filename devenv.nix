{ pkgs, lib, ... }:
let
  # Same probe as flake.nix's hasWebToolchain: tailwindcss_4 throws on
  # platforms nixpkgs ships no binary for (riscv64-linux).
  hasWebToolchain = (builtins.tryEval pkgs.tailwindcss_4.outPath).success;
in {
  packages = with pkgs;
    [
      # Plain `go` in nixpkgs lags go.mod's directive; `just sync-toolchain`
      # rewrites this pin from go.mod.
      go_1_27
      gopls
      gotools # goimports
      golangci-lint
      templ # regenerate views: `just generate`
      air # live reload: `just dev`
      just
      sqlite # inspect the metadata db
      curl
    ] ++ lib.optionals hasWebToolchain [ tailwindcss_4 ]; # admin CSS: `just css`

  enterShell = ''
    # A GOROOT inherited from an older shell points the toolchain at a go
    # that is not the one on PATH ("compile: version ... does not match").
    # The nix go finds its own root; never carry one over.
    unset GOROOT
  '';
}
