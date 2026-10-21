{
  perSystem = {pkgs, ...}: {
    _module.args = {
      # The one Go pin, shared by the packages and the dev shell; `just
      # sync-toolchain` rewrites it from go.mod. go.mod asks for 1.27 and
      # nixpkgs' default `go` is still 1.26. The sandbox has no network, so
      # the auto-toolchain fetch that papers over that outside nix fails
      # here. A function of the package set so the riscv64 cross build picks
      # its own. Drop it once nixpkgs' default catches up.
      goFor = p: p.go_1_27;

      # Can we build the admin UI assets here? tailwindcss_4 throws on
      # platforms it has no binary for (riscv64-linux), so probe it.
      hasWebToolchain = (builtins.tryEval pkgs.tailwindcss_4.drvPath).success;
    };
  };
}
