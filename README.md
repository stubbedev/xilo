# xilo

[![CI](https://github.com/stubbedev/xilo/actions/workflows/ci.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/ci.yml)
[![Perf](https://github.com/stubbedev/xilo/actions/workflows/perf.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/perf.yml)
[![Docker](https://github.com/stubbedev/xilo/actions/workflows/docker.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/docker.yml)
[![coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Fstubbedev%2Fxilo%2Fmaster%2F.github%2Fbadges%2Fcoverage.json)](https://github.com/stubbedev/xilo/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/stubbedev/xilo)](https://github.com/stubbedev/xilo/releases/latest)

Self-hosted [Nix binary cache](https://nix.dev/manual/nix/latest/store/types/http-binary-cache-store) — a single Go binary, no external services. An alternative to [attic](https://github.com/zhaofengli/attic) that:

- **never stalls on concurrent pushes** — pure-Go SQLite (WAL) with a single writer goroutine; chunk bytes live in the storage backend, never the DB, so pushes take no long-held lock
- ships a **cachix-style admin dashboard** to manage caches and tokens
- can **revoke push/pull tokens** instantly
- does **content-addressed chunked dedup** (FastCDC) across all caches
- stores chunks on **local disk or any S3-compatible bucket** (AWS, [Garage](https://garagehq.deuxfleurs.fr/), R2, …)
- ships a **9 MB distroless Docker image** and serves zstd pulls straight from
  stored frames (zero compression CPU on the hot path)

Head-to-head against attic (same machine, same 91-path/1 GB closure, same
chunking, sqlite + local storage both sides — reproduce with
[`tests/bench/bench.sh`](./tests/bench/bench.sh)):

| metric | xilo | attic |
|---|---|---|
| cold push (1 GB closure) | **5.2 s** | 5.7 s |
| dedup re-push | **0.1 s** | 0.3 s |
| narinfo QPS (16 conns) | **5 981** | 1 887 |
| NAR pull throughput | **695 MB/s** | 216 MB/s |
| server max RSS under that load | **108 MiB** | 197 MiB |
| CPU per MB/s served | **0.7 %** | 1.1 % |
| Docker image | **9 MB** | 87 MB |

(attic's per-NAR p95 was ~50 ms better in this run — it was serving a third
of the bytes. Numbers from 2026-07; rerun the script for your hardware.)

## Quick start

```sh
docker run -d -p 8080:8080 -v xilo-data:/data \
  -e XILO_ADMIN_PASSWORD=change-me ghcr.io/stubbedev/xilo:latest
```

For a VPS, [`examples/docker-compose.yml`](./examples/docker-compose.yml) is the
same thing with a restart policy and an optional S3 block; add
[`examples/Caddyfile`](./examples/Caddyfile) for TLS.

Open <http://localhost:8080/admin>, log in, create a cache. Or from the CLI:

```sh
xilo cache create mycache          # prints the public key + nix.conf snippet
```

### Nix / NixOS

The flake ships the binary (client CLI + `xilo serve` in one), a NixOS
module, and a home-manager module:

```sh
nix run github:stubbedev/xilo -- --help    # try it
nix profile install github:stubbedev/xilo  # just the CLI
```

NixOS — server as a systemd unit plus the CLI in `systemPackages`:

```nix
{
  inputs.xilo.url = "github:stubbedev/xilo";
}
```

```nix
{ inputs, ... }: {
  imports = [ inputs.xilo.nixosModules.default ];

  services.xilo = {
    enable = true;
    settings = {
      # rendered to xilo.yaml; see xilo.example.yaml for all keys.
      # listen defaults to ":8080", data_dir to /var/lib/xilo.
      base_url = "https://cache.example.com";
    };
    # Secrets stay out of the Nix store:
    environmentFile = "/run/secrets/xilo.env"; # XILO_ADMIN_PASSWORD=…
  };
}
```

home-manager — user-level CLI, optionally with a user config:

```nix
{ inputs, ... }: {
  imports = [ inputs.xilo.homeModules.default ];

  programs.xilo = {
    enable = true;
    # Optional; writes ~/.config/xilo/xilo.yaml (picked up by `xilo serve`).
    settings.listen = ":8090";
  };
}
```

`xilo serve` resolves its config as: `--config` / `XILO_CONFIG` →
`./xilo.yaml` → `$XDG_CONFIG_HOME/xilo/xilo.yaml` → `/etc/xilo/xilo.yaml`.
Dependency bumps and the flake's `vendorHash` are managed by `just update`
(never edit by hand).

### Pull (substitute)

Add to `nix.conf` (the cache page in the dashboard shows this filled in):

```
extra-substituters = http://localhost:8080/mycache
extra-trusted-public-keys = mycache:<public-key>
```

### Push

Nix can't upload to an HTTP cache, so xilo ships its own client:

```sh
xilo cache create mycache
# in the dashboard: create a token with "push" → copy the secret
XILO_URL=http://localhost:8080 XILO_TOKEN=<secret> xilo push mycache ./result
```

Parallelism is automatic (the server advertises its capacity; override with `--jobs`).
Paths already signed by a configured `upstream_keys` entry (e.g. `cache.nixos.org-1`) are skipped.
Add `--dry-run` to preview, `--quiet` for hooks.

### Automatic push

Point Nix's `post-build-hook` at [`examples/post-build-hook.sh`](./examples/post-build-hook.sh)
(pushes each built path via `xilo push <cache> - --quiet`), or on Linux run the
inotify watcher:

```sh
xilo watch mycache   # auto-pushes newly-built store paths
```

### GitHub Actions

The repo doubles as a composite action: it installs the CLI, saves the login,
and adds the cache as a substituter, so the build pulls what previous runs
cached and `xilo push` needs no env:

```yaml
- uses: DeterminateSystems/nix-installer-action@main
- uses: stubbedev/xilo@v0
  with:
    url: https://cache.example.com
    cache: mycache
    token: ${{ secrets.XILO_TOKEN }} # a push token from the dashboard
- run: nix build
- run: xilo push mycache ./result
```

The `v0` tag floats to the newest release automatically on every tag push.

Full workflow in [`examples/github-actions.yml`](./examples/github-actions.yml).

### Convenience

```sh
xilo login https://cache.example.com --token <secret>   # save URL+token
xilo use mycache                                         # write nix.conf (+ netrc if private)
xilo use mycache --remove                                # undo
```

## Behind a reverse proxy (TLS)

xilo speaks plain HTTP; terminate TLS with Caddy/nginx and set
`base_url: "https://…"` so session cookies are `Secure`. See
[`examples/Caddyfile`](./examples/Caddyfile). `narinfo`/`nar` responses are
`immutable` with `ETag`, so a CDN in front caches them hard.

## Observability

- `GET /healthz` — readiness probe (does a DB read).
- `GET /metrics` — Prometheus counters (narinfo hit/miss, NAR bytes, chunk dedup, pushes, auth failures) plus Go runtime gauges (goroutines, heap). A ready-made Grafana dashboard is in [`examples/grafana-dashboard.json`](./examples/grafana-dashboard.json).
- Request logging + graceful shutdown (drains in-flight transfers on SIGTERM) are built in. Set `logging: quiet` to log only errors and slow requests on busy instances.

## Backups

All state is one SQLite file plus the chunk directory under `data_dir`
(local backend). Back up **the database first, then the chunks** — the server
writes a chunk's blob before its DB row, so a snapshot ordered DB→chunks can
only contain extra unreferenced blobs (harmless; the next GC sweeps them),
never a row pointing at a missing blob:

```sh
sqlite3 /data/xilo.db ".backup /backup/xilo.db"   # consistent WAL-aware copy
rsync -a /data/storage/ /backup/storage/
```

For continuous replication, [Litestream](https://litestream.io/) on
`xilo.db` plus any object-storage sync for `storage/` works well. With the
S3 backend only the DB needs backing up.

## Tokens & private caches

- Tokens are opaque secrets, stored hashed, scoped to caches + `push`/`pull`, **revocable** from the dashboard or `xilo token revoke <id>`.
- Public caches are open to pull. Private caches need a `pull` token, supplied by Nix via `~/.netrc`:

  ```
  machine cache.example.com login xilo password <token>
  ```

- Until the first token exists, push is open (bootstrap mode); creating any token locks it down.

## Storage

Default is local disk under `data_dir`. For S3-compatible object storage (example uses Garage):

```yaml
storage:
  backend: s3
  s3:
    endpoint: "localhost:3900"
    bucket: "xilo"
    region: "garage"
    access_key: "" # or XILO_S3_ACCESS_KEY
    secret_key: "" # or XILO_S3_SECRET_KEY
    insecure: true # plain HTTP for a local Garage
```

Or entirely from env — setting `XILO_S3_BUCKET` selects the s3 backend, so a
Docker deployment needs no config file (`XILO_S3_ENDPOINT`, `XILO_S3_BUCKET`,
`XILO_S3_REGION`, `XILO_S3_ACCESS_KEY`, `XILO_S3_SECRET_KEY`,
`XILO_S3_INSECURE`). The SQLite database stays in `data_dir`.

## Garbage collection

Chunks are content-addressed and shared; GC is a mark-sweep over unreferenced chunks. Dashboard button, or:

```sh
xilo gc                     # sweep unreferenced chunks
xilo gc --older-than 720h   # also evict paths not pulled in 30 days, then sweep
```

## Integrity checking

`xilo fsck` verifies every chunk row against its stored blob and every path
against its chunk list — the states a crash or disk damage could leave that
normal operation can't heal (dedup trusts a chunk row forever):

```sh
xilo fsck             # existence check (fast)
xilo fsck --content   # re-hash every blob (reads all data)
xilo fsck --repair    # drop bad rows + broken paths; the next push re-uploads them
```

## Configuration

YAML (see [`xilo.example.yaml`](./xilo.example.yaml)). A JSON schema is published at
[`schemas/xilo.schema.json`](./schemas/xilo.schema.json) and referenced by a
`yaml-language-server` modeline for editor autocompletion. Secrets can come from env
(`XILO_ADMIN_PASSWORD`, `XILO_S3_ACCESS_KEY`, `XILO_S3_SECRET_KEY`).

## Development

```sh
nix develop          # go, templ, air, just, golangci-lint …
just                 # list recipes
just dev             # live-reload server (air)
just generate        # regenerate templ views
just check           # lint + test + templ/schema in sync
```

The admin UI is [templ](https://templ.guide/) components styled with [Pico CSS](https://picocss.com/) (vendored, no CDN). Generated `*_templ.go` and `schemas/xilo.schema.json` are committed and verified in sync by CI.

## How it works

A push runs `nix path-info` for the closure, chunks each NAR client-side (FastCDC),
uploads only the chunks the server lacks, then registers the path metadata. Serving
speaks the standard Nix binary-cache protocol: `/{cache}/nix-cache-info`,
`/{cache}/{hash}.narinfo` (signed on the fly with the cache's ed25519 key so pushers
never hold the signing key), and `/{cache}/nar/{hash}.nar` (reassembled from chunks).
