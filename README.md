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
extra-trusted-public-keys = mycache:<public-key>
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
Same machine, same closure (93 paths, 646 MB of NAR), same load script. GitHub ubuntu-latest runner, 2 cores, 2026-11-16.

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 280, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "narinfo throughput"
    x-axis ["xilo  3,247/s", "attic  1,102/s", "harmonia  3,572/s", "nix-serve-ng  1,708/s", "Garage + nix copy  1,946/s"]
    y-axis "requests/s, higher is better" 0 --> 5000
    bar [3247.103, 1102.366, 3572.193, 1708.453, 1946.158]
```

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 240, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "NAR pull throughput"
    x-axis ["xilo  647 MB/s", "attic  68 MB/s", "harmonia  684 MB/s", "nix-serve-ng  14 MB/s"]
    y-axis "MB/s, higher is better" 0 --> 1000
    bar [647.342, 67.566, 683.671, 13.891]
```

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 280, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "Memory under pull load"
    x-axis ["xilo  99 MiB", "attic  108 MiB", "harmonia  24 MiB", "nix-serve-ng  32 MiB", "Garage + nix copy  24 MiB"]
    y-axis "peak RAM in use, lower is better" 0 --> 200
    bar [98.88, 107.7, 23.67, 32.45, 23.62]
```

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 240, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "CPU per MB/s served"
    x-axis ["xilo  0.28", "attic  2.38", "harmonia  0.28", "nix-serve-ng  8.20"]
    y-axis "% of a core per MB/s, lower is better" 0 --> 10
    bar [0.27887577200305247, 2.3804872273036732, 0.2836232632362642, 8.195450291555684]
```

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 200, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "Cold push of the closure"
    x-axis ["xilo  3.9 s", "attic  9.8 s", "Garage + nix copy  84.8 s"]
    y-axis "seconds, lower is better" 0 --> 100
    bar [3.9, 9.8, 84.8]
```

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 200, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "Repeat push of the closure"
    x-axis ["xilo  0.1 s", "attic  0.8 s", "Garage + nix copy  0.1 s"]
    y-axis "seconds, lower is better" 0 --> 1
    bar [0.1, 0.8, 0.1]
```

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 280, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "Stored on disk"
    x-axis ["xilo  225 MB", "attic  212 MB", "harmonia  654 MB", "nix-serve-ng  654 MB", "Garage + nix copy  157 MB"]
    y-axis "bytes for this closure; a host store keeps it unpacked" 0 --> 1000000000
    bar [225354440, 212363843, 654291571, 654291571, 156640678]
```

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 280, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "Image or closure to deploy"
    x-axis ["xilo  11 MB", "attic  87 MB", "harmonia  78 MB", "nix-serve-ng  220 MB", "Garage + nix copy  28 MB"]
    y-axis "bytes, lower is better" 0 --> 500000000
    bar [11249623, 86969956, 78256816, 220114536, 28103547]
```

Each chart has its own scale. A missing bar is a target that does not do that thing, not a zero.
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
