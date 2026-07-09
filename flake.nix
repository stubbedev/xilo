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
          ];
          shellHook = ''
            echo "xilo dev shell — run 'just' to list recipes"
          '';
        };
      });
}
