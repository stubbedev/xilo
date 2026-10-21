{
  description = "xilo — self-hosted Nix binary cache";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };
  };

  outputs = inputs @ {flake-parts, ...}:
    flake-parts.lib.mkFlake {inherit inputs;} {
      # Explicit list, not nixpkgs' flakeExposed: x86_64-darwin is gone from
      # nixpkgs 26.11 (it throws on eval). riscv64 gets the client only, since
      # nixpkgs has no tailwindcss_4 there and the admin CSS is a build-time
      # input of the server.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
        "riscv64-linux"
      ];
      imports = [
        ./nix/toolchain.nix
        ./nix/package.nix
        ./nix/shell.nix
        ./nix/hm-module.nix
        ./nix/nixos-module.nix
      ];
    };
}
