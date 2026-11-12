{inputs, ...}: {
  perSystem = {pkgs, ...}: {
    _module.args = {
      goFor = p: (inputs.devtools.overlays.default p.buildPackages p.buildPackages).go;

      # Can we build the admin UI assets here? tailwindcss_4 throws on
      # platforms it has no binary for (riscv64-linux), so probe it.
      hasWebToolchain = (builtins.tryEval pkgs.tailwindcss_4.drvPath).success;
    };
  };
}
