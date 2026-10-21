# Install

## Docker

```sh
docker run -d -p 8080:8080 -v xilo-data:/data \
  -e XILO_ADMIN_PASSWORD=change-me ghcr.io/stubbedev/xilo:latest
```

[`examples/docker-compose.yml`](../examples/docker-compose.yml) is the same
deployment with a restart policy and a commented S3 block;
[`examples/Caddyfile`](../examples/Caddyfile) adds TLS.

## Prebuilt binary

Every release publishes static binaries for `linux` (amd64, arm64, riscv64) and
`darwin` (arm64):

```sh
curl -fsSL https://github.com/stubbedev/xilo/releases/latest/download/xilo-linux-amd64.tar.gz \
  | tar -xz -C /usr/local/bin
```

## Nix

The flake ships the binary (client CLI and `xilo serve` in one), a NixOS module
and a home-manager module:

```sh
nix run github:stubbedev/xilo -- --help    # try it
nix profile install github:stubbedev/xilo  # the full binary
```

Client-only machines can install `packages.xilo-cli`: the same CLI without
`xilo serve`, and without templ and Tailwind as build inputs. That is the only
option on platforms nixpkgs has no `tailwindcss_4` for (`riscv64-linux`, where
`packages.default` *is* `xilo-cli`):

```sh
nix profile install github:stubbedev/xilo#xilo-cli
```

riscv64 machines that need the server take `xilo-riscv64`, the full binary
cross-compiled from an x86_64 or aarch64 builder (templ and Tailwind run on the
builder, only Go targets riscv64). Build it there and `nix copy` it over, or
point a riscv64 host at it with a remote builder configured:

```nix
{ inputs, ... }: {
  imports = [ inputs.xilo.nixosModules.default ];
  services.xilo.package = inputs.xilo.packages.x86_64-linux.xilo-riscv64;
  services.xilo.enable = true;
}
```

Module usage is documented under [NixOS module](configuration.md#nixos-module) and
[home-manager module](configuration.md#home-manager-module).

## First run

`XILO_ADMIN_PASSWORD` (or `admin.password`) seeds the first user on an empty
database: username `admin`, instance role superadmin. Once any user exists the
value is ignored, so a stale environment variable cannot reset a password. On
an instance with `self_service` off, boot also creates a workspace named
`admin` and makes that user its owner.

Open <http://localhost:8080/admin>, sign in, and create a cache. The same is
available from the CLI. Administrative commands run against the server's API,
so point the CLI at it once:

```sh
xilo login http://localhost:8080 --token <admin-token>
xilo cache create myteam/mycache   # prints the public key and nix.conf snippet
```

On the server itself, where `xilo.yaml` and the database live, the same
commands work directly against the local database with no login. A cache
reference always names its account, and the account is created if it does not
exist yet. A bare name is resolved against the token's account (remote) or the
instance's single account (local).
