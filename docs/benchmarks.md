# Benchmarks

Two sets of numbers, both measured and drawn by CI and committed to the
repository: a comparison against the other self-hostable caches, and a gate
against xilo's own past. Nothing here is typed in by hand. The comparison is
re-measured weekly, whenever the harness itself changes, and on every release
([`bench.yml`](../.github/workflows/bench.yml)); both charts are redrawn from
the committed numbers on every push ([`ci.yml`](../.github/workflows/ci.yml)),
so a figure and its chart cannot drift apart. Every chart's caption names the
machine and the date that produced it, because a throughput number without
those is not a measurement.

## Against other Nix caches

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

[`tests/bench/bench.sh`](../tests/bench/bench.sh) starts each implementation in
turn, pushes the same closure into it (where it takes a push), runs the same
pull load against it, and samples the container's memory and CPU while that
load runs. [`bench.yml`](../.github/workflows/bench.yml) runs it on the schedule
above and commits what the runner measured, so
[`tests/bench/results.json`](../tests/bench/results.json) is the raw data behind
the charts, which `just perf-charts` (or CI) redraws from it.

| target | what it is | accepts a push | dedup |
|---|---|---|---|
| xilo | this project: SQLite metadata, local chunk storage | over its own API | FastCDC chunks, shared across caches |
| [attic](https://github.com/zhaofengli/attic) | latest published image: SQLite, local storage | over its own API | FastCDC chunks |
| [harmonia](https://github.com/nix-community/harmonia) | serves the host `/nix/store` over HTTP | no, the store is the state | none, the store is the state |
| [nix-serve-ng](https://github.com/aristanetworks/nix-serve-ng) | serves the host `/nix/store` over HTTP | no, the store is the state | none, the store is the state |
| MinIO + `nix copy` | a plain S3 bucket used as a cache | `nix copy --to s3://…` | none, one compressed NAR per path |
| [Garage](https://garagehq.deuxfleurs.fr/) + `nix copy` | the same, on the other self-hostable object store; read over its web endpoint | `nix copy --to s3://…` | none, one compressed NAR per path |

What keeps the comparison honest:

- One target at a time, sequentially, so nothing shares the machine.
- Every server runs in a container, and every memory and CPU figure comes from
  `docker stats` sampled once a second on that container, so a single method
  measures all of them. The memory bar is peak resident memory (RSS: the
  physical RAM the server actually occupied, not what it reserved) during the
  pull phase.
- The pull load is [one k6 script](../tests/bench/pull.js) speaking the plain
  binary-cache protocol, so no implementation gets a client of its own.
- xilo and attic get matching chunk sizes and both compress with zstd at rest.
- CPU is charted per MB/s served rather than raw, because the server that
  serves the least spends the least, which is not the same as costing less.
- `nix copy` writes each NAR compressed and a bucket serves those bytes
  verbatim, so neither bucket's byte rate is the quantity a server
  reassembling identity NARs reports. Both are flagged in the JSON and left
  out of the throughput panels.
- harmonia and nix-serve-ng serve paths that are already in the host store, so
  neither has a push phase. Both answer `Compression: none`, so their byte
  rates are the quantity the other servers report, and their bar in the
  storage panel is the closure's own footprint in `/nix/store`: nothing
  compressed, nothing deduplicated. That is free when the cache is the machine
  that built the paths and the full amount when it is not, which is the trade
  they make rather than a number worth leaving blank.
- Storage is measured as the whole data directory, so a cache's metadata
  counts against it too, not just its chunks.
- Absolute numbers belong to the machine that produced them, named with the
  date in the chart's caption. Read the bars against each other rather than
  against your own hardware.

Reading the panels: xilo and attic are the two that accept a push, chunk it,
deduplicate it and reassemble NARs on the way out, so that is the pairing to
read against each other. A server that hands out the host store has nothing to
reassemble, which is why it can beat a chunk store on raw pull throughput and
run in a fraction of the memory; what it cannot do is take a push from another
machine or compress and dedup what it serves, so its storage bar is the
unpacked closure (the two of them also differ sharply from each other, and the
NAR panels are where that shows). A plain bucket does no work per request and
keeps the fewest bytes, because `nix copy` compresses each NAR once with xz and
the bucket never reassembles anything; it pays for that in push time and in
having no dedup between paths.

To measure your own hardware:

```sh
just bench              # every target, around 20 minutes, needs docker + nix
just bench xilo,attic   # a subset
```

## Against its own past

<!-- perfchart:k6-gate -->
Committed baselines from tests/k6/baselines, measured on 2-core GitHub runners and re-checked on every push. Worst of 12 sampled runs, plus each metric's tolerance.

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 400, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "Read latency, p95"
    x-axis ["narinfo hit  8.4 ms", "narinfo miss  8.3 ms", "NAR pull  7.3 ms", "NAR pull, zstd  6.6 ms", "NAR pull, 64 MiB  97 ms", "narinfo under push load  38 ms", "narinfo, 512 clients  59 ms", "narinfo, 5000 rps arrival  0.8 ms"]
    y-axis "ms, lower is better" 0 --> 100
    bar [8.411690899999995, 8.3399478, 7.273910649999999, 6.570586999999999, 97.01801439999998, 37.6803545, 58.670966, 0.765489]
```

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 160, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "Push round trip, p95"
    x-axis ["repeat push (deduped)  1.52 s", "fresh push  1.55 s"]
    y-axis "ms, lower is better" 0 --> 2000
    bar [1521.0977961, 1549.921537]
```

```mermaid
%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "#7c7f93", "xAxisLabelColor": "#7c7f93", "xAxisTitleColor": "#7c7f93", "xAxisLineColor": "#9ca0b0", "yAxisTitleColor": "#7c7f93", "yAxisLineColor": "#9ca0b0", "plotColorPalette": "#8839ef"}}, "xyChart": {"height": 160, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%
xychart-beta horizontal
    title "Requests served per run"
    x-axis ["throughput suite  357,331", "pressure suite  390,705"]
    y-axis "requests, higher is better" 0 --> 500000
    bar [357331, 390705]
```

Each chart has its own scale; every bar carries the measured value.
<!-- /perfchart:k6-gate -->

Every push to `master` runs the k6 suites in single-tenant and multi-tenant
mode and compares them against the committed baselines in
[`tests/k6/baselines`](../tests/k6/baselines), per-metric tolerances included,
failing the build on a regression
([`perf.yml`](../.github/workflows/perf.yml)). Moving a baseline is therefore a
deliberate commit (`just re-baseline`), which is what makes the chart above a
standard the project holds itself to rather than one good run.

The suites, in [`tests/k6`](../tests/k6/README.md):

- `perf.js`: narinfo hit and miss, NAR pull (identity, zstd stored frames, 64
  MiB NAR), dedup and fresh push, a 4 to 32 VU push saturation ramp, and a
  mixed read/write window.
- `pressure.js`: a 512-VU narinfo storm, a 5000 rps constant-arrival flood, a
  128-VU pull wall with byte-exact verification, an abort storm, and a
  goroutine and heap leak watch.
- `churn.js`: integrity under hostile GC (5 s sweeps, 30 s retention), with
  every pushed NAR pulled back and hash-verified.
- `ops.js`, `mt.js`, `deep.js`: wire-contract and correctness conformance for
  the cache protocol, the tenancy surface and the edge dimensions.
