# xilo

[![CI](https://github.com/stubbedev/xilo/actions/workflows/ci.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/ci.yml)
[![Perf](https://github.com/stubbedev/xilo/actions/workflows/perf.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/perf.yml)
[![Docker](https://github.com/stubbedev/xilo/actions/workflows/docker.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/docker.yml)
[![Bench](https://github.com/stubbedev/xilo/actions/workflows/bench.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/bench.yml)
[![coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Fstubbedev%2Fxilo%2Fmaster%2F.github%2Fbadges%2Fcoverage.json)](https://github.com/stubbedev/xilo/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/stubbedev/xilo)](https://github.com/stubbedev/xilo/releases/latest)

A self-hosted [Nix binary cache](https://nix.dev/manual/nix/latest/store/types/http-binary-cache-store)
in a single Go binary: server, admin dashboard and client CLI in one static
executable, with no external services required. Metadata lives in SQLite (or
PostgreSQL), chunk bytes on local disk (or any S3-compatible bucket), and
content-addressed FastCDC chunking deduplicates across every cache on the same
backend.

xilo is MIT-licensed and self-hosted. It has no billing surface: plans exist
only as quota profiles.

## Contents

- [Features](#features)
- [Benchmarks](#benchmarks)
- [Feature comparison with attic](#feature-comparison-with-attic)
- [Install](#install)
- [Using a cache](#using-a-cache)
- [Tokens and private caches](#tokens-and-private-caches)
- [Accounts, organizations and self-service](#accounts-organizations-and-self-service)
- [Storage backends](#storage-backends)
- [PostgreSQL](#postgresql)
- [Retention, quotas and garbage collection](#retention-quotas-and-garbage-collection)
- [Integrity checking](#integrity-checking)
- [Backups](#backups)
- [Upgrading](#upgrading)
- [Reverse proxy and TLS](#reverse-proxy-and-tls)
- [Observability](#observability)
- [Configuration](#configuration)
- [How it works](#how-it-works)
- [Development](#development)

## Features

### Nix protocol

- The standard HTTP binary-cache protocol under `/c/{account}/{cache}`:
  `nix-cache-info`, `{hash}.narinfo`, `nar/{hash}.nar` reassembled from chunks.
- `narinfo` responses are signed per request with the cache's rotatable ed25519
  key, so pushers never hold a signing key.
- `zstd`, `gzip` and identity are negotiated; zstd pulls come straight from the
  stored frames, so no compression runs on the pull path.
- Responses carry `ETag` and `immutable`, so a CDN or proxy in front can cache
  them indefinitely.
- A path whose chunks are incomplete fails with an error rather than a
  truncated `200`.
- Per-cache substituter priority and public/private visibility.

### Push client

Nix cannot upload to an HTTP cache, so xilo ships its own client in the same
binary:

- FastCDC chunking client-side with server-dictated parameters, so every client
  chunks identically and dedup stays global.
- NARs are serialized in-process rather than by one `nix-store --dump`
  subprocess per path, and every archive is checked against the NarHash Nix
  recorded.
- Uploads run in parallel at the capacity the server advertises.
- Paths signed by a configured upstream key (`cache.nixos.org-1`, for example)
  are skipped.
- Automatic push through a `post-build-hook`, a detached background push, an
  inotify store watcher, or the composite GitHub Action.

### Storage and deduplication

- Local filesystem or any S3-compatible bucket (AWS, MinIO,
  [Garage](https://garagehq.deuxfleurs.fr/), R2), with several named backends
  at once and one backend pinned per cache.
- Content-addressed chunk dedup per backend, zstd-compressed at rest.
- Chunk bytes never pass through the database, so a push holds no long write
  lock.
- SQLite by default (pure-Go driver, WAL, one writer goroutine, so
  `SQLITE_BUSY` cannot occur), PostgreSQL for larger deployments.
- Migrations are additive and run at boot: a newer binary opens an older
  database with no export or replay step.

### Multi-tenancy and access control

- Caches live in accounts (a user's workspace, or an organization) and are
  addressed `account/cache`. Instance roles are separate from membership roles
  inside an organization.
- Tokens are opaque secrets, stored hashed, revocable immediately, optionally
  expiring, and each is valid for exactly one cache.
- Accounts have a lifecycle: `active`, `readonly` (pulls only) and `suspended`,
  ahead of deletion.
- Optional self-service signup with approval, and plans capping caches,
  members, storage, retention and organization creation.
- Sign-in is rate-limited per IP and supports TOTP 2FA and passkeys.
- Optionally, signing keys and TOTP secrets are encrypted at rest and token
  hashes keyed with `database.salt`.

### Admin dashboard

- Caches, tokens, users, accounts, organizations, plans and instance settings,
  with `nix.conf` and token snippets rendered per cache.
- Deduplication (logical versus physical bytes) and storage usage on the
  dashboard and per cache.
- Live status page, and an activity log of every admin and API mutation:
  searchable, sortable and paginated.
- Six UI languages and fifteen per-user colour palettes, light and dark.
- Server-rendered templ components with a build-time stylesheet: no CDN and no
  runtime JS framework.

### Operations

- `GET /healthz` readiness probe and Prometheus metrics at `GET /metrics`, with
  counters persisted so totals survive restarts.
- Garbage collection as a mark-sweep over unreferenced chunks, with retention
  windows, per-cache size caps and a global storage ceiling.
- `xilo fsck` verifies chunks against blobs and paths against their chunk
  lists, and repairs what a crash or disk damage left behind.
- A JSON API at `/api/v1/...` mirrors the administrative operations, so the CLI
  can manage a remote instance.
- Request logging with a quiet mode, graceful shutdown that drains in-flight
  transfers, and a published [JSON schema](./schemas/xilo.schema.json) for the
  config file.

### Deployment

- One static, cgo-free binary in a distroless image, published for
  `linux/amd64` and `linux/arm64`. Its size sits beside the alternatives' in
  the [comparison](#against-other-nix-caches).
- Prebuilt binaries for `linux` amd64/arm64/riscv64 and `darwin` amd64/arm64.
- A flake with a NixOS module, a home-manager module, and a server-less client
  build for platforms without a Tailwind toolchain.

## Benchmarks

Two sets of numbers, both measured and drawn by CI and committed to the
repository: a comparison against the other self-hostable caches, and a gate
against xilo's own past. Nothing here is typed in by hand. The comparison is
re-measured weekly, whenever the harness itself changes, and on every release
([`bench.yml`](./.github/workflows/bench.yml)); both charts are redrawn from
the committed numbers on every push ([`ci.yml`](./.github/workflows/ci.yml)),
so a figure and its chart cannot drift apart. Every chart's caption names the
machine and the date that produced it, because a throughput number without
those is not a measurement.

### Against other Nix caches

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/perf/compare-dark.svg">
  <img alt="Bar charts comparing xilo, attic, harmonia, nix-serve-ng and a MinIO bucket on narinfo throughput, NAR throughput, memory, CPU per MB/s, push time, bytes stored and deployment size" src="docs/perf/compare-light.svg">
</picture>

[`tests/bench/bench.sh`](./tests/bench/bench.sh) starts each implementation in
turn, pushes the same closure into it (where it takes a push), runs the same
pull load against it, and samples the container's memory and CPU while that
load runs. [`bench.yml`](./.github/workflows/bench.yml) runs it on the schedule
above and commits what the runner measured, so
[`tests/bench/results.json`](./tests/bench/results.json) is the raw data behind
the chart, which `just perf-charts` (or CI) redraws from it.

| target | what it is | accepts a push | dedup |
|---|---|---|---|
| xilo | this project: SQLite metadata, local chunk storage | over its own API | FastCDC chunks, shared across caches |
| [attic](https://github.com/zhaofengli/attic) | latest published image: SQLite, local storage | over its own API | FastCDC chunks |
| [harmonia](https://github.com/nix-community/harmonia) | serves the host `/nix/store` over HTTP | no, the store is the state | none, the store is the state |
| [nix-serve-ng](https://github.com/aristanetworks/nix-serve-ng) | serves the host `/nix/store` over HTTP | no, the store is the state | none, the store is the state |
| MinIO + `nix copy` | a plain S3 bucket used as a cache | `nix copy --to s3://…` | none, one compressed NAR per path |

What keeps the comparison honest:

- One target at a time, sequentially, so nothing shares the machine.
- Every server runs in a container, and every memory and CPU figure comes from
  `docker stats` sampled once a second on that container, so a single method
  measures all of them. The memory bar is peak resident memory (RSS: the
  physical RAM the server actually occupied, not what it reserved) during the
  pull phase.
- The pull load is [one k6 script](./tests/bench/pull.js) speaking the plain
  binary-cache protocol, so no implementation gets a client of its own.
- xilo and attic get matching chunk sizes and both compress with zstd at rest.
- CPU is charted per MB/s served rather than raw, because the server that
  serves the least spends the least, which is not the same as costing less.
- `nix copy` writes each NAR compressed and the bucket serves those bytes
  verbatim, so that target's byte rate is not the quantity a server
  reassembling identity NARs reports. It is flagged in the JSON and left out
  of the throughput panels.
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

### Against its own past

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/perf/k6-gate-dark.svg">
  <img alt="Bar charts of xilo's committed k6 baselines: read latency p95 per scenario, push round trip p95, and requests served per run" src="docs/perf/k6-gate-light.svg">
</picture>

Every push to `master` runs the k6 suites in single-tenant and multi-tenant
mode and compares them against the committed baselines in
[`tests/k6/baselines`](./tests/k6/baselines), per-metric tolerances included,
failing the build on a regression
([`perf.yml`](./.github/workflows/perf.yml)). Moving a baseline is therefore a
deliberate commit (`just re-baseline`), which is what makes the chart above a
standard the project holds itself to rather than one good run.

The suites, in [`tests/k6`](./tests/k6/README.md):

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

## Feature comparison with attic

attic is the closest peer, so this is a feature-by-feature table rather than a
measurement; the numbers are in [Benchmarks](#benchmarks).

|  | xilo | attic |
|---|---|---|
| chunked dedup (FastCDC) | yes | yes |
| SQLite / PostgreSQL | yes / yes | yes / yes |
| local / S3 storage | yes, several named backends, per cache | yes, one |
| multi-tenancy | accounts, organizations, users, roles | per-cache only |
| server-managed signing keys | yes, with rotation | yes |
| token revocation | immediate (database-backed) | no (stateless JWT) |
| token scope | exactly one `account/cache`, plus `manage`/`admin` perms | JWT cache patterns |
| retention and GC | time and size caps (per cache, plus a global ceiling) | time only |
| incomplete data | fails closed | can serve truncated `200`s |
| web dashboard | yes (caches, users, accounts, tokens, status, activity) | no |
| Prometheus metrics | yes | no |
| store-watch auto-push | yes | yes |
| integrity fsck and repair | yes | no |
| tagged releases | yes | none yet |

## Install

### Docker

```sh
docker run -d -p 8080:8080 -v xilo-data:/data \
  -e XILO_ADMIN_PASSWORD=change-me ghcr.io/stubbedev/xilo:latest
```

[`examples/docker-compose.yml`](./examples/docker-compose.yml) is the same
deployment with a restart policy and a commented S3 block;
[`examples/Caddyfile`](./examples/Caddyfile) adds TLS.

### Prebuilt binary

Every release publishes static binaries for `linux` (amd64, arm64, riscv64) and
`darwin` (amd64, arm64):

```sh
curl -fsSL https://github.com/stubbedev/xilo/releases/latest/download/xilo-linux-amd64.tar.gz \
  | tar -xz -C /usr/local/bin
```

### Nix

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

Module usage is documented under [NixOS module](#nixos-module) and
[home-manager module](#home-manager-module).

### First run

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

## Using a cache

### Pull

Add the cache to `nix.conf` (the cache's page in the dashboard shows this
filled in):

```
extra-substituters = http://localhost:8080/c/myteam/mycache
extra-trusted-public-keys = mycache:<public-key>
```

Or let the CLI write it:

```sh
xilo use myteam/mycache            # writes nix.conf, plus netrc for a private cache
xilo use myteam/mycache --remove   # undo
```

Cache URLs are always `/c/{account}/{cache}`. The `/c/` mount means account
names can never collide with application routes.

### Push

```sh
xilo cache create myteam/mycache
# on the cache's page in the dashboard: "Create push token", copy the snippet
xilo login http://localhost:8080 --token <secret>
xilo push myteam/mycache ./result
xilo status                        # which profile, token, scope and expiry are in use
```

Parallelism follows the capacity the server advertises unless `--jobs` says
otherwise. `--dry-run` previews without uploading, `--quiet` suits hooks, and
`-` reads store paths from stdin. With a default target saved
(`xilo use myteam/mycache --default`) the cache argument is optional:
`xilo push ./result`.

### Automatic push

Point Nix's `post-build-hook` at
[`examples/post-build-hook.sh`](./examples/post-build-hook.sh), which pushes
each built path with `xilo push <cache> - --quiet`, or run the inotify watcher
on Linux:

```sh
xilo watch myteam/mycache   # pushes newly built store paths
```

Nix runs a `post-build-hook` synchronously, so the build waits for the push.
Two ways to keep the push off the build's critical path: `xilo push --detach`
hands the work to a background process and returns immediately (progress and
errors go to `~/.cache/xilo/push.log`), and the watcher pushes out of band
entirely. The example hook uses `--detach`; drop the flag to let a failed push
fail the build.

### GitHub Actions

The repository doubles as a composite action: it installs the CLI, saves the
login, and adds the cache as a substituter, so a build pulls what previous runs
cached and `xilo push` needs no environment of its own:

```yaml
- uses: DeterminateSystems/nix-installer-action@main
- uses: stubbedev/xilo@v1
  with:
    url: https://cache.example.com
    cache: mycache
    token: ${{ secrets.XILO_TOKEN }} # a push token from the dashboard
- run: nix build
- run: xilo push mycache ./result
```

Inputs are `url`, `cache`, `token` (optional for a public cache) and `version`
(defaults to the latest release). The `v1` tag follows the newest release on
every tag push; pin a full `v1.2.3` tag to opt into upgrades explicitly. Full
workflow in [`examples/github-actions.yml`](./examples/github-actions.yml).

### CLI reference

Client commands:

| command | purpose |
|---|---|
| `xilo login <url>` | save a server profile (`--token`, `--name`, `--cache`, `--default`) |
| `xilo use <account>/<cache>` | write `nix.conf` and netrc (`--default`, `--remove`) |
| `xilo push [account/cache] <path>...` | push paths or a closure (`--jobs`, `--dry-run`, `--quiet`, `--detach`) |
| `xilo watch <account>/<cache>` | push newly built store paths (Linux; `--store`) |
| `xilo status` | profile, token, scope, perms, expiry, `nix.conf` and netrc state |

Administrative commands, run against the local database or against a remote
instance with `--server <url> --token <admin-token>`:

| command | purpose |
|---|---|
| `xilo serve` | run the cache server (`--config`) |
| `xilo cache create <name>` | create a cache (`--private`, `--priority`, `--storage`) |
| `xilo cache list` / `info <name>` | list caches, show one cache's key and usage |
| `xilo cache configure <name>` | visibility, `--priority`, `--retention`, `--max-size` |
| `xilo cache rotate <name>` | generate a new signing key |
| `xilo cache destroy <name>` | delete a cache (`--yes`) |
| `xilo token create <name>` | mint a token (`--cache`, `--pull`, `--push`, `--manage`, `--admin`, `--ttl`) |
| `xilo token list` / `revoke <id>` | list tokens, revoke one |
| `xilo gc` | sweep unreferenced chunks (`--older-than`) |
| `xilo fsck` | verify integrity (`--content`, `--repair`) |

## Tokens and private caches

- Tokens are opaque secrets, stored hashed, and revocable from the dashboard or
  with `xilo token revoke <id>`. Revocation takes effect immediately, because
  authorization is a database lookup rather than a signature check.
- A token is valid for exactly one cache, written `account/cache`, and belongs
  to that cache's account. There are no wildcard scopes, so a leaked token
  covers one cache.
- The dashboard mints them from the cache's own page ("Create push token",
  "Create pull token"): the scope and owning account come from the page, and
  the secret is rendered into the setup snippets once.
- Cache perms are `pull`, `push` and `manage` (create, configure and destroy
  that one cache). Separately, `admin` is instance-wide management, covering
  every cache, token and account, and it is the credential behind
  `xilo cache|token|gc --server ...`. It is not a cache scope and grants
  neither pull nor push.
- `xilo status` prints what the saved token actually is (account, scope, perms,
  expiry), so a `401` is diagnosable without guessing.
- Caches are public by default and open to pull. `--private` (or the dashboard)
  requires a `pull` token, which Nix supplies from `~/.netrc`:

  ```
  machine cache.example.com login xilo password <token>
  ```

- Push always requires a token. `security.allow_open_bootstrap: true` (default
  `false`) permits anonymous pushes, and only until the first token exists.

## Accounts, organizations and self-service

Caches live in accounts. Every user gets a workspace of their own (slug =
username), and organizations group teams. Usernames and organization names
share one global pool, so `/c/{account}/{cache}` is unambiguous and sign-in
works with either username or email.

- Membership is the only way to reach an account's caches and tokens. Members
  join as owner (the creator), admin (manages the organization's caches, tokens
  and members) or user (visibility).
- A superadmin runs the instance (policy, plans, storage, every organization)
  and owns nothing in it. To work inside an account they add themselves to its
  member list and switch to it.
- A tenant's dashboard shows only their own accounts, foreign caches return
  `404`, and a token cannot cross an account boundary.
- Accounts degrade before they disappear: `readonly` still serves pulls and
  refuses pushes, `suspended` serves nothing, and only an explicit delete
  removes data.

All of that is always on. `self_service` adds the signup surface on top of it:

|  | `self_service: false` (default) | `self_service: true` |
|---|---|---|
| accounts, organizations, per-cache token scopes | yes | yes |
| a workspace per user | yes | yes |
| who creates users and organizations | the superadmin | anyone who registers |
| self-registration at `/register` | no | behind an instance toggle |
| plans and quotas | no | yes |

(`multi_tenant` was the previous name for this key and still works as an
alias.)

With `self_service: true` the extra surface is governed from Settings:

- Instance toggles: allow registrations (off by default) and require approval
  for new accounts (on by default, so no email infrastructure is needed).
- Plans are quota profiles offered at `/register`: maximum caches,
  organization members, storage, retention ceiling, and whether organizations
  may be created. No plan means no limits. An account over its storage quota
  goes read-only for pushes while pulls keep working, and nothing is deleted
  automatically. Retention ceilings clamp per-cache retention during the GC
  sweep. Per-account egress is metered and displayed, not enforced.
- Registration can create an organization on the spot when the chosen plan
  allows it, and entitled users can create more later from Settings.

Instance rules (the storage ceiling, the retention a new cache inherits, the
longest a token may live, and the plan a signup lands on) live in settings and
are read where they apply, so changing one needs no restart.

Transactional email (registration, approval and membership notices) needs an
SMTP relay under `smtp:`. With no host configured, mail is disabled and
everything else keeps working.

## Storage backends

The default backend is local disk under `data_dir`. For S3-compatible object
storage:

```yaml
storage:
  backend: s3
  s3:
    endpoint: "localhost:3900"
    bucket: "xilo"
    region: "garage"
    access_key: "" # or XILO_S3_ACCESS_KEY
    secret_key: "" # or XILO_S3_SECRET_KEY
    insecure: true # plain HTTP, for a local Garage or MinIO
```

The same is configurable entirely from the environment. Setting
`XILO_S3_BUCKET` also selects the s3 backend, so a container deployment needs
no config file (`XILO_S3_ENDPOINT`, `XILO_S3_BUCKET`, `XILO_S3_REGION`,
`XILO_S3_ACCESS_KEY`, `XILO_S3_SECRET_KEY`, `XILO_S3_INSECURE`). The database
stays in `data_dir`.

Additional named backends sit beside the primary one, which is named
`default`, and each cache is pinned to one at creation
(`xilo cache create foo --storage fast`, or the select in the dashboard):

```yaml
storages:
  fast:
    backend: local
    local: { root: /nvme/xilo }
default_storage: fast   # backend for new caches when none is chosen
```

Chunk dedup is per backend, and GC and fsck sweep each backend independently.

## PostgreSQL

SQLite is the default and needs no configuration. For larger deployments point
xilo at PostgreSQL:

```yaml
database:
  url: "postgres://xilo:secret@db.internal/xilo"   # or XILO_DATABASE_URL
```

The schema is created and migrated automatically, and blob storage is
unaffected. An administrative CLI on another machine cannot open the database
directly: use `--server https://cache.example.com --token <admin-token>` and
the same commands run over the HTTP API.

## Retention, quotas and garbage collection

Chunks are content-addressed and shared between paths and caches, so GC is a
mark-sweep over unreferenced chunks. It runs on a schedule (`gc.interval`),
from the dashboard, or from the CLI:

```sh
xilo gc                     # sweep unreferenced chunks
xilo gc --older-than 720h   # evict paths not pulled in 30 days, then sweep
```

Four limits apply, all optional:

| limit | where it is set | effect |
|---|---|---|
| `gc.retention` | config | evicts paths not pulled within the window |
| per-cache retention | `xilo cache configure --retention`, dashboard | overrides the global window for that cache |
| per-cache size cap | `xilo cache configure --max-size`, dashboard | caps one cache's stored bytes |
| `limits.total` | config, or the instance storage ceiling in Settings | evicts least-recently-pulled paths across all caches |

`gc.grace` (default `1h`) protects chunks newer than the window, so a chunk
uploaded by an in-flight push is never swept before its path is registered; it
must exceed the longest single push. `gc.audit_retention` (default one year)
trims the activity log on each sweep.

## Integrity checking

`xilo fsck` verifies every chunk row against its stored blob and every path
against its chunk list. Those are the states a crash or disk damage can leave
and that normal operation does not heal, because dedup trusts a chunk row
indefinitely:

```sh
xilo fsck             # existence check (fast)
xilo fsck --content   # re-hash every blob (reads all data)
xilo fsck --repair    # drop bad rows and broken paths; the next push re-uploads them
```

## Backups

All state is one SQLite file plus the chunk directory under `data_dir` (local
backend). Back up the database first, then the chunks: the server writes a
chunk's blob before its database row, so a snapshot ordered database-then-chunks
can only contain extra unreferenced blobs, which the next GC sweeps, and never
a row pointing at a missing blob.

```sh
sqlite3 /data/xilo.db ".backup /backup/xilo.db"   # consistent, WAL-aware copy
rsync -a /data/storage/ /backup/storage/
```

For continuous replication, [Litestream](https://litestream.io/) on `xilo.db`
plus any object-storage sync for `storage/` covers both halves. With the S3
backend only the database needs backing up.

## Upgrading

Schema migrations run at boot and are additive: a newer binary opens an older
database, and nothing has to be exported or replayed.

**v1.1 to v1.2** renames two vocabularies in place: the instance role `owner`
becomes `superadmin`, and an account frozen as `past_due` becomes `readonly`
(the old name described a subscription this project does not have). Both are
renames of existing rows, so the upgrade is safe to repeat. A downgrade to v1.1
leaves a role that version does not recognise, which would cost the instance
its administrator, so back the database up first if you want that option.

## Reverse proxy and TLS

xilo speaks plain HTTP; terminate TLS with Caddy, nginx or equivalent and set
`base_url: "https://..."` so session cookies are `Secure`. See
[`examples/Caddyfile`](./examples/Caddyfile).

The real client IP, used for login rate limiting and the activity log, is read
from `X-Forwarded-For` or `X-Real-IP` when the proxy sits on a loopback or
private address, which needs no configuration in the usual colocated setup. For
chained proxies set `trusted_proxy_hops` to the number of hops in front of xilo
(each must append to `X-Forwarded-For` rather than overwrite it); `-1` disables
proxy trust and keys on the socket peer instead.

## Observability

- `GET /healthz`: readiness probe, performs a database read.
- `GET /metrics`: Prometheus counters (`xilo_narinfo_hits_total`,
  `xilo_narinfo_misses_total`, `xilo_nar_bytes_total`,
  `xilo_chunks_received_total`, `xilo_chunks_deduped_total`,
  `xilo_paths_pushed_total`, `xilo_paths_adopted_total`,
  `xilo_auth_failures_total`, with pull-serving and push-upload latency counted
  separately) plus Go runtime gauges. Counters are persisted alongside the
  metadata, so totals and the dashboard figures survive restarts. A Grafana
  dashboard is in
  [`examples/grafana-dashboard.json`](./examples/grafana-dashboard.json).
- Activity log: every successful admin or API mutation is recorded with actor,
  method, path, source IP, user agent, latency and status, and is browsable at
  `/admin/audit`. A background job trims entries past `gc.audit_retention`.
- Request logging and graceful shutdown are built in. `logging: quiet` logs
  only errors and slow requests, which is measurable at tens of thousands of
  requests per second.

## Configuration

Configuration comes from three layers, each overriding the one below it:

1. Environment variables (for secrets and container deployments)
2. The YAML config file (`xilo.yaml`)
3. Built-in defaults (a bare `xilo serve` with no config file works)

The Nix modules are frontends to these layers: `settings` renders the YAML
file, `environmentFile` feeds the environment.

### Config file

`xilo serve` uses the first of these that exists:

1. `--config <path>`, or `XILO_CONFIG=<path>`
2. `./xilo.yaml`
3. `$XDG_CONFIG_HOME/xilo/xilo.yaml`, usually `~/.config/xilo/xilo.yaml` (what
   the home-manager module writes)
4. `/etc/xilo/xilo.yaml`

A missing file is not an error. Every key is optional;
[`xilo.example.yaml`](./xilo.example.yaml) is the annotated reference with all
defaults, and [`schemas/xilo.schema.json`](./schemas/xilo.schema.json) is
referenced by a `yaml-language-server` modeline for editor completion. A
production file typically holds:

```yaml
listen: ":8080"
base_url: "https://cache.example.com" # https so session cookies are Secure
data_dir: "/var/lib/xilo"

self_service: false

gc:
  interval: "12h"    # background sweep
  retention: "720h"  # evict paths not pulled in 30 days

upstream_keys: ["cache.nixos.org-1"] # don't re-cache nixpkgs

# Secrets (admin.password, database.salt, storage.s3.*_key, smtp.password)
# are better supplied via the environment, see below.
```

Keys worth knowing beyond that:

| key | default | effect |
|---|---|---|
| `chunking.min_size` / `avg_size` / `max_size` | 64 KiB / 256 KiB / 1 MiB | FastCDC parameters, dictated to clients |
| `chunking.nar_threshold` | `min_size` | NARs below this are stored as a single chunk |
| `compression.level` | `default` | zstd level at rest (`fastest` to `best`) |
| `parallelism` | 4x CPU count | upload concurrency advertised to clients |
| `durability` | `normal` | `full` fsyncs every commit |
| `logging` | `full` | `quiet` logs errors and slow requests only |
| `security.skip_upload_verify` | `false` | skips reassembly verification on push |
| `security.allow_open_bootstrap` | `false` | anonymous push until the first token exists |
| `trusted_proxy_hops` | `0` | proxy hops trusted for the client IP |
| `database.salt` | empty | encrypts signing keys and TOTP secrets at rest; must never change once set |

### Environment variables

Environment values override the corresponding YAML key. They are intended for
secrets, keeping them out of config files and the Nix store, and for file-less
container deployments:

| variable | overrides |
|---|---|
| `XILO_CONFIG` | config file path (same as `--config`) |
| `XILO_LISTEN` | `listen` |
| `XILO_BASE_URL` | `base_url` |
| `XILO_DATA_DIR` | `data_dir` |
| `XILO_ADMIN_PASSWORD` | `admin.password` |
| `XILO_DATABASE_URL` | `database.url` |
| `XILO_SALT` | `database.salt` |
| `XILO_SMTP_PASSWORD` | `smtp.password` |
| `XILO_S3_ENDPOINT` | `storage.s3.endpoint` |
| `XILO_S3_BUCKET` | `storage.s3.bucket` (also selects the s3 backend, unless the YAML set `storage.backend`) |
| `XILO_S3_REGION` | `storage.s3.region` |
| `XILO_S3_ACCESS_KEY` | `storage.s3.access_key` |
| `XILO_S3_SECRET_KEY` | `storage.s3.secret_key` |
| `XILO_S3_INSECURE` | `storage.s3.insecure` (`true`/`1` enables; cannot turn a YAML `true` off) |

The client CLI reads its own set: `XILO_URL`, `XILO_TOKEN`, `XILO_CACHE`,
`XILO_CACHE_DIR`, `XILO_EXTERNAL_DUMP`.

### Environment file

For systemd (`services.xilo.environmentFile`) or Docker (`--env-file`, compose
`env_file:`) the variables go in a plain env file: one `KEY=value` per line, no
`export`, no quotes, `#` starts a comment.

```ini
# /run/secrets/xilo.env
XILO_ADMIN_PASSWORD=change-me
XILO_SALT=a-long-random-string-never-changed-once-set

# Only for the S3 backend:
XILO_S3_ACCESS_KEY=GK31c2f218a2e44f485b94239e
XILO_S3_SECRET_KEY=b892c0665f0ada8a4755dae98baa3b13

# Only for PostgreSQL:
XILO_DATABASE_URL=postgres://xilo:secret@db.internal/xilo
```

### NixOS module

`settings` is rendered to `xilo.yaml` (any key from
[`xilo.example.yaml`](./xilo.example.yaml) belongs here, but never secrets: the
Nix store is world-readable) and passed to the service with `--config`.
`environmentFile` becomes the unit's systemd `EnvironmentFile` and holds the
secrets, in the format above.

```nix
{
  inputs.xilo.url = "github:stubbedev/xilo";
}
```

```nix
{ inputs, ... }: {
  imports = [ inputs.xilo.nixosModules.default ];

  services.xilo = {
    enable = true; # systemd unit plus the client CLI in systemPackages

    settings = {
      # listen defaults to ":8080", data_dir to /var/lib/xilo.
      base_url = "https://cache.example.com";
      gc = {
        interval = "12h";
        retention = "720h";
      };
      upstream_keys = [ "cache.nixos.org-1" ];
      # S3 storage; the keys come from environmentFile:
      storage = {
        backend = "s3";
        s3 = {
          endpoint = "s3.amazonaws.com";
          bucket = "xilo";
          region = "us-east-1";
        };
      };
    };

    # An env file that exists on the target machine, outside the Nix store,
    # deployed by hand or by a secrets tool:
    environmentFile = "/run/secrets/xilo.env";
    # sops-nix:  environmentFile = config.sops.secrets."xilo.env".path;
    # agenix:    environmentFile = config.age.secrets."xilo.env".path;
  };
}
```

`package` defaults to `packages.default` for the host's system. On riscv64 that
is the client-only build, so a riscv64 server needs `package` pointed at the
cross-built `xilo-riscv64` as shown under [Nix](#nix).

### home-manager module

Installs the CLI. `settings` optionally writes `~/.config/xilo/xilo.yaml`,
which `xilo serve` picks up via the lookup order above:

```nix
{ inputs, ... }: {
  imports = [ inputs.xilo.homeModules.default ];

  programs.xilo = {
    enable = true; # the CLI, which is all a client-only machine needs

    # Only for running a user-level server (`xilo serve`):
    settings = {
      listen = ":8090";
      base_url = "http://localhost:8090";
      data_dir = "/home/me/.local/share/xilo";
    };
  };
}
```

### Client configuration

`xilo login` saves server profiles to `~/.config/xilo/config.yaml`
(`$XDG_CONFIG_HOME` respected), a different file from the server's `xilo.yaml`
and managed entirely by the CLI. `XILO_URL`, `XILO_TOKEN` and per-command flags
override it per invocation.

Pushes also keep a disposable cache in `~/.cache/xilo`: chunk manifests, the
server's chunk presence filter, and `push.log` from `--detach`.
`XILO_CACHE_DIR` relocates it, and deleting it is always safe, costing only
work. With `XILO_EXTERNAL_DUMP=1` the client serializes NARs by running
`nix-store --dump` instead of doing it in-process.

## How it works

### Request path

`cmd/xilo` dispatches to `internal/server` (one `http.ServeMux`), which reads
metadata through `internal/store` and chunk bytes through `internal/storage`.
The server exposes three surfaces:

- The Nix binary-cache protocol at `/c/{account}/{cache}/...`:
  `nix-cache-info`, `{hash}.narinfo` (signed on the fly with the cache's
  ed25519 key) and `nar/{hash}.nar` (reassembled from chunks).
- The push protocol beside it: `api/config`, `api/get-missing-paths`,
  `api/get-missing-chunks`, `api/chunk-filter`, `api/chunk/{hash}`,
  `api/path`.
- The token-authed JSON API at `/api/v1/...` and the session-authed dashboard
  at `/admin/...`.

A push asks `nix path-info` for the closure, chunks each NAR client-side with
FastCDC, uploads only the chunks the server lacks, then registers the path
metadata.

### What a push avoids doing

Determining what the server already has is most of a push, so four mechanisms
cut that down, cheapest first:

- **Path adoption.** The client sends each path's NAR hash with the "what is
  missing" question. If an identical path already exists in another cache on
  the same storage backend, the server copies the chunk list over and reports
  the path present: no dump, no chunking, no upload. Only caches the caller may
  already read are eligible (public, pullable by the token, or an admin token),
  so adoption grants no access the caller did not have.
- **Cached chunk manifests.** `~/.cache/xilo/manifests` remembers each pushed
  path's chunk list, keyed by store hash, NAR hash and the server's chunking
  parameters. Pushing the same path again (to a second cache, after a failed
  run, after a server GC) registers it without reading the NAR.
- **The chunk presence filter.** `GET /c/{account}/{cache}/api/chunk-filter`
  serves a bloom filter of the backend's chunk hashes (memoized for about ten
  minutes, `ETag`-revalidated, cached on disk). With it the client uploads new
  chunks immediately and skips known ones without a negotiation round trip. It
  is fetched only when the push is large enough to be worth the download.
- **Pipelined negotiation.** Without a filter, chunk windows are resolved
  concurrently with the dump instead of blocking it, so a round trip no longer
  stalls the stream every 32 chunks.

Server-side, `put-path`'s reassembly check, which streams every referenced
chunk back out of the blob store to confirm the claimed NarHash, is remembered
per chunk list, so a re-push, a push to a second cache, or a retry only
confirms that the blobs still exist.

Every one of those shortcuts is optimistic, and one check makes that safe:
`put-path` re-stamps and re-checks every referenced chunk, answering `409` with
the hashes that are absent. On a `409` the client redoes that path with all
shortcuts disabled, so a stale manifest, a filter false positive or a chunk
swept mid-push costs one extra dump and never a corrupt or dangling path.

## Development

```sh
nix develop          # go, templ, air, just, golangci-lint, ...
just                 # list recipes
just dev             # live-reload server (air) with seeded demo data
just generate        # regenerate templ views
just check           # everything CI runs: lint, test, schema and nix build in sync
just update          # bump deps and the flake vendorHash together
just release-patch   # tag and push a release (RELEASING.md); -minor and -major too
```

The admin UI is [templ](https://templ.guide/) components (the
[templUI](https://templui.io/) library) styled with
[Tailwind CSS v4](https://tailwindcss.com/), compiled into a single embedded
stylesheet at build time. The generated `*_templ.go` and CSS are rebuilt by
`just` and git-ignored; `schemas/xilo.schema.json` is committed and verified in
sync by CI.

Tests beyond `go test ./...`: `tests/e2e/cli.sh` (real Nix and a container),
the k6 suites in [`tests/k6`](./tests/k6/README.md) (`just k6-ops`,
`just k6-perf`, `just k6-churn`, `just k6-pressure`, and their `-mt`
multi-tenant variants), and the cross-implementation comparison in
[`tests/bench/bench.sh`](./tests/bench/bench.sh) (`just bench`). `just
perf-charts` redraws the README's performance graphics from the committed
numbers.

Release process: [RELEASING.md](./RELEASING.md). Vulnerability reporting and
deployment scope notes: [SECURITY.md](./SECURITY.md). Licensed under
[MIT](./LICENSE).
