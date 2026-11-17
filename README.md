<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/banner-dark.svg">
    <img alt="xilo" src="docs/brand/banner-light.svg" width="640">
  </picture>
</h1>

[![CI](https://github.com/stubbedev/xilo/actions/workflows/ci.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Fstubbedev%2Fxilo%2Fmaster%2F.github%2Fbadges%2Fcoverage.json)](https://github.com/stubbedev/xilo/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/stubbedev/xilo)](https://github.com/stubbedev/xilo/releases/latest)

A self-hosted [Nix binary cache](https://nix.dev/manual/nix/latest/store/types/http-binary-cache-store)
in a single Go binary. One static executable is the server, the admin dashboard and
the push client. It needs no other services.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/dashboard-dark.png">
  <img alt="The xilo dashboard: dedup ratio, storage, caches and tokens" src="docs/screenshots/dashboard-light.png">
</picture>

## Why xilo

- **One binary, no dependencies.** SQLite or PostgreSQL for metadata, local disk or any
  S3-compatible bucket for data.
- **Deduplicated storage.** Content-defined chunking (FastCDC) shares bytes across every
  path and every cache, and compresses them with zstd at rest.
- **Fast pushes.** The client uploads only the chunks the server is missing, and can skip
  paths an upstream cache such as `cache.nixos.org` already signed.
- **Safe by default.** Tokens are scoped to a single cache and revoked immediately.
  Signing keys never leave the server, and incomplete data fails with an error instead
  of a truncated download.
- **Multi-tenant.** Workspaces, organizations, roles, quotas and optional self-service
  signup.
- **A real dashboard.** Caches, tokens, users, live status and an activity log, in six
  languages, light and dark.

## Quick start

Run the server:

```sh
docker run -d -p 8080:8080 -v xilo-data:/data \
  -e XILO_ADMIN_PASSWORD=change-me ghcr.io/stubbedev/xilo:latest
```

Open <http://localhost:8080/admin>, sign in as `admin`, then create a cache and a push
token. On the cache's page you can copy the setup snippets.

Push to it:

```sh
xilo login http://localhost:8080 --token <token>
xilo push myteam/mycache ./result
```

Pull from it by adding the cache to `nix.conf`. `xilo use myteam/mycache` adds it for you:

```
extra-substituters = http://localhost:8080/c/myteam/mycache
extra-trusted-public-keys = localhost/myteam/mycache-1:<public-key>
```

Prebuilt binaries, the Nix flake, and the NixOS and home-manager modules are covered in
[Install](docs/install.md).

## Screenshots

<table>
  <tr>
    <td width="50%">
      <picture>
        <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/cache-dark.png">
        <img alt="A cache page with its dedup ratio and nix.conf, flake and CLI setup snippets" src="docs/screenshots/cache-light.png">
      </picture>
      <p align="center"><b>Cache</b>: dedup and setup snippets</p>
    </td>
    <td width="50%">
      <picture>
        <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/status-dark.png">
        <img alt="The status page: health, uptime, hit rate and live request, latency, throughput and storage charts" src="docs/screenshots/status-light.png">
      </picture>
      <p align="center"><b>Status</b>: live health and traffic</p>
    </td>
  </tr>
  <tr>
    <td width="50%">
      <picture>
        <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/activities-dark.png">
        <img alt="The activity log of every admin and API change, filterable by method and status" src="docs/screenshots/activities-light.png">
      </picture>
      <p align="center"><b>Activities</b>: every admin and API change</p>
    </td>
    <td width="50%">
      <picture>
        <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/console-dark.png">
        <img alt="The instance console listing every workspace with its storage and lifecycle controls" src="docs/screenshots/console-light.png">
      </picture>
      <p align="center"><b>Console</b>: every workspace on the instance</p>
    </td>
  </tr>
</table>

## Performance

<!-- perfchart:compare -->
Same machine, same closure (93 paths, 646 MB of NAR), same load script. GitHub ubuntu-latest runner, 2 cores, 2026-11-17.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/perf/compare-dark.svg">
  <img alt="Bar charts comparing xilo with other self-hostable Nix caches on 8 measures, xilo highlighted. The table below holds every number." src="docs/perf/compare-light.svg">
</picture>

Each chart has its own scale. A missing bar is a target that does not do that thing, not a zero.

Stored on disk: a host store keeps the closure unpacked, a cache stores it compressed.

<details>
<summary>The same numbers as a table</summary>

| Measure | xilo | attic | harmonia | nix-serve-ng | Garage + nix copy |
|---|--:|--:|--:|--:|--:|
| narinfo throughput (requests/s, higher is better) | 3,257/s | 1,116/s | **3,638/s** | 1,716/s | 2,074/s |
| NAR pull throughput (MB/s, higher is better) | 633 MB/s | 68 MB/s | **688 MB/s** | 14 MB/s | — |
| Memory under pull load (peak RAM in use, lower is better) | 100 MiB | 192 MiB | **22 MiB** | 31 MiB | **22 MiB** |
| CPU per MB/s served (% of a core per MB/s, lower is better) | **0.28** | 2.38 | **0.28** | 8.14 | — |
| Cold push of the closure (seconds, lower is better) | **3.9 s** | 9.8 s | — | — | 84.1 s |
| Repeat push of the closure (seconds, lower is better) | **0.1 s** | 0.8 s | — | — | **0.1 s** |
| Stored on disk (bytes for this closure) | 225 MB | 212 MB | 654 MB | 654 MB | 157 MB |
| Image or closure to deploy (bytes, lower is better) | **11 MB** | 87 MB | 78 MB | 220 MB | 28 MB |

Bold is the best value of a measure that has a better direction; — is a target that does not do that thing.

</details>
<!-- /perfchart:compare -->

CI measures these numbers and commits them; none are typed in by hand.
[Benchmarks](docs/benchmarks.md) explains the method, and
[Features](docs/features.md) compares xilo with the alternatives feature by feature.

## Documentation

| | |
|---|---|
| [Install](docs/install.md) | Docker, binaries, Nix flake, first run |
| [Usage](docs/usage.md) | pulling, pushing, GitHub Actions, CLI, tokens, organizations |
| [Configuration](docs/configuration.md) | config file, environment, S3, PostgreSQL, NixOS and home-manager modules, TLS |
| [Operations](docs/operations.md) | garbage collection, quotas, fsck, backups, upgrades, metrics |
| [Features](docs/features.md) | the full feature list and a comparison with other caches |
| [Benchmarks](docs/benchmarks.md) | how xilo compares with other caches, and the regression gate |
| [How it works](docs/architecture.md) | request path and what a push avoids doing |
| [Development](docs/development.md) | dev shell, recipes, UI stack, test suites |

## Development

```sh
nix develop    # go, templ, air, just, golangci-lint, ...
just dev       # live-reload server with demo data on :8090 (admin / demo)
just check     # everything CI runs
```

Run `just` to list every recipe. [Development](docs/development.md) has the rest, including
the UI stack and the test suites. [RELEASING.md](./RELEASING.md) covers releases and
[SECURITY.md](./SECURITY.md) covers vulnerability reports.

## License

[MIT](./LICENSE)
