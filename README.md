# xilo

[![CI](https://github.com/stubbedev/xilo/actions/workflows/ci.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/ci.yml)
[![Perf](https://github.com/stubbedev/xilo/actions/workflows/perf.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/perf.yml)
[![Docker](https://github.com/stubbedev/xilo/actions/workflows/docker.yml/badge.svg)](https://github.com/stubbedev/xilo/actions/workflows/docker.yml)
[![coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Fstubbedev%2Fxilo%2Fmaster%2F.github%2Fbadges%2Fcoverage.json)](https://github.com/stubbedev/xilo/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/stubbedev/xilo)](https://github.com/stubbedev/xilo/releases/latest)

Self-hosted [Nix binary cache](https://nix.dev/manual/nix/latest/store/types/http-binary-cache-store) — a single Go binary, no external services. An alternative to [attic](https://github.com/zhaofengli/attic) that:

- **never stalls on concurrent pushes** — pure-Go SQLite (WAL) with a single writer goroutine; chunk bytes live in the storage backend, never the DB, so pushes take no long-held lock
- is **multi-tenant**: every user gets a personal account, organizations group
  teams (`/c/{account}/{cache}`), and optional self-registration offers
  super-admin-defined plans with storage/cache/member quotas
- ships a **cachix-style admin dashboard** to manage caches, tokens, users and accounts, with live status and searchable activities
- can **revoke push/pull tokens** instantly
- does **content-addressed chunked dedup** (FastCDC) per storage backend
- stores chunks on **local disk or any S3-compatible bucket** (AWS, [Garage](https://garagehq.deuxfleurs.fr/), R2, …) — several named backends at once, assignable per cache
- scales past a personal cache by pointing `database.url` at **PostgreSQL** (SQLite stays the zero-config default)
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

Feature parity, and where xilo goes further:

| | xilo | attic |
|---|---|---|
| chunked dedup (FastCDC) | ✓ | ✓ |
| SQLite / PostgreSQL | ✓ / ✓ | ✓ / ✓ |
| local / S3 storage | ✓ (+ several named backends, per cache) | ✓ (one) |
| namespaces / multi-tenancy | ✓ first-class, with users + roles | per-cache only |
| server-managed signing keys | ✓ (+ rotation) | ✓ |
| token revocation | ✓ instant (DB-backed) | ✗ (stateless JWT) |
| token scopes | `*`, `ns/*`, `ns/cache` + mgmt perms | JWT cache patterns |
| retention / GC | time **and size caps** (per cache + global LRU) | time only |
| missing data | fails closed (clean error) | can serve truncated 200s |
| web dashboard | ✓ (users, accounts, tokens, live status, activities) | ✗ |
| Prometheus metrics | ✓ | ✗ |
| store-watch auto-push | ✓ | ✓ |
| integrity fsck + repair | ✓ | ✗ |
| tagged releases | ✓ | none yet |

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

Full module examples, the environment-file format and the config lookup
order are in [Configuration](#configuration).

### Pull (substitute)

Add to `nix.conf` (the cache page in the dashboard shows this filled in):

```
extra-substituters = http://localhost:8080/c/default/mycache
extra-trusted-public-keys = mycache:<public-key>
```

Cache URLs are always `/c/{account}/{cache}` — the `/c/` mount means account
names can never collide with application routes. Bare names in the CLI mean
the `default` account.

### Push

Nix can't upload to an HTTP cache, so xilo ships its own client:

```sh
xilo cache create mycache
# in the dashboard: create a token with "push" → copy the secret
XILO_URL=http://localhost:8080 XILO_TOKEN=<secret> xilo push mycache ./result
```

Parallelism is automatic (the server advertises its capacity; override with `--jobs`).
Paths already signed by a configured `upstream_keys` entry (e.g. `cache.nixos.org-1`) are skipped.
Add `--dry-run` to preview, `--quiet` for hooks. With a saved default target
(`xilo use mycache --default`) the cache argument is optional: `xilo push ./result`.

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
- uses: stubbedev/xilo@v1
  with:
    url: https://cache.example.com
    cache: mycache
    token: ${{ secrets.XILO_TOKEN }} # a push token from the dashboard
- run: nix build
- run: xilo push mycache ./result
```

The `v1` tag floats to the newest release automatically on every tag push (pin a full `v1.2.3` tag if you'd rather opt into upgrades).

Full workflow in [`examples/github-actions.yml`](./examples/github-actions.yml).

### Convenience

```sh
xilo login https://cache.example.com --token <secret>   # save a server profile
xilo login https://other.example.com --name work        # more servers: named profiles (-p work)
xilo use mycache --default                               # write nix.conf (+ netrc) and make it the default push target
xilo push ./result                                       # no cache argument needed anymore
xilo use mycache --remove                                # undo nix.conf
```

## Behind a reverse proxy (TLS)

xilo speaks plain HTTP; terminate TLS with Caddy/nginx and set
`base_url: "https://…"` so session cookies are `Secure`. See
[`examples/Caddyfile`](./examples/Caddyfile). When the proxy sits on a
loopback/private address (the usual colocated setup), xilo automatically reads
the real client IP from `X-Forwarded-For`/`X-Real-IP` — used for login
rate-limiting and activities — with no configuration.
`narinfo`/`nar` responses are `immutable` with `ETag`, so a CDN in front caches
them hard.

## Observability

- `GET /healthz` — readiness probe (does a DB read).
- `GET /metrics` — Prometheus counters (narinfo hit/miss, NAR bytes, chunk dedup, pushes, auth failures; pull-serving and push-upload latency counted separately) plus Go runtime gauges (goroutines, heap). Counters are persisted alongside the metadata DB, so totals and the dashboard KPIs survive restarts. A ready-made Grafana dashboard is in [`examples/grafana-dashboard.json`](./examples/grafana-dashboard.json).
- **Activities** — every successful admin/API mutation is recorded (actor, method, path, source IP, user-agent, latency, status) and browsable under `/admin/audit`: searchable, sortable, paginated. A low-priority background job trims entries past `gc.audit_retention` (default 1 year).
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

- Tokens are opaque secrets, stored hashed, **revocable** from the dashboard or `xilo token revoke <id>`.
- Scopes are patterns: `*`, `ns/*` or `ns/cache`. A token minted inside a
  namespace can never reach outside it.
- Perms: `pull`, `push`, plus management bits for the HTTP API —
  `create-cache`, `configure-cache`, `destroy-cache` (scoped like pull/push)
  and `admin` (instance-wide; drives remote `xilo cache|token|gc --server …`).
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
`XILO_S3_INSECURE`). The database stays in `data_dir`.

Additional **named backends** can sit next to the primary (which is named
`default`), and each cache is pinned to one at creation (`xilo cache create
foo --storage fast`, or the select in the dashboard). Chunk dedup is
per-backend, and GC/fsck sweep each backend independently:

```yaml
storages:
  fast:
    backend: local
    local: { root: /nvme/xilo }
default_storage: fast   # backend for new caches when none is chosen
```

## Multi-tenancy

Caches live in **accounts** — either a **personal account** (created
automatically with every user, slug = username) or an **organization**.
Usernames and org names share one global pool, so `/c/{account}/{cache}` is
always unambiguous and you can sign in with username or email. Users join
orgs as **admin** (manages the org's caches, tokens, members) or **member**
(visibility); instance admins manage everything.

Two caches may share a name in different accounts; each has its own signing
key. A tenant's dashboard shows only their accounts, foreign caches 404, and
account tokens cannot cross the boundary.

Setting `multi_tenant: true` unlocks the signup surface, all governed from
Settings by the instance admin:

- **Instance** toggles: allow registrations (off by default), require
  approval for new accounts (on by default — no email infrastructure needed).
- **Plans**: quota bundles (max caches, org members, storage, retention
  ceiling, organizations allowed) offered at `/register`. No plan = no
  limits. Over storage quota an account goes **read-only for pushes** — pulls
  keep working, data is never auto-deleted. Retention ceilings clamp per-cache
  retention in the GC sweep. Per-account egress is metered and shown in
  Settings (not yet enforced).
- Registration can create an organization on the spot when the chosen plan
  allows it; entitled users can also create orgs later from Settings.

Leave `multi_tenant` off (the default) and none of this surface exists — a
single admin manages a private instance exactly as before.

## PostgreSQL

SQLite (the default) needs zero configuration and comfortably serves a
personal or small-team cache. For larger deployments point xilo at Postgres:

```yaml
database:
  url: "postgres://xilo:secret@db.internal/xilo"   # or XILO_DATABASE_URL
```

The schema is created and migrated automatically; blob storage is unaffected.
Note the admin CLI on a different machine can't open the database directly —
use `--server https://cache.example.com --token <admin token>` (an `admin`-perm
token) and the same commands work over the HTTP API.

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

The server takes its configuration from three layers; each overrides the
one below it:

1. **Environment variables** (highest — secrets and container deployments)
2. **YAML config file** (`xilo.yaml`)
3. Built-in defaults (a bare `xilo serve` with no config at all works)

The Nix modules are frontends to these layers: `settings` renders the YAML
file, `environmentFile` feeds the environment.

### Config file (`xilo.yaml`)

`xilo serve` looks for the file in this order and uses the first that exists:

1. `--config <path>` flag, or `XILO_CONFIG=<path>`
2. `./xilo.yaml` (current working directory)
3. `$XDG_CONFIG_HOME/xilo/xilo.yaml` — usually `~/.config/xilo/xilo.yaml`
   (this is what the home-manager module writes)
4. `/etc/xilo/xilo.yaml`

A missing file is not an error — defaults plus env still yield a working
server. Every key is optional; [`xilo.example.yaml`](./xilo.example.yaml) is
the full annotated reference with all defaults. A JSON schema is published at
[`schemas/xilo.schema.json`](./schemas/xilo.schema.json) and referenced by a
`yaml-language-server` modeline for editor autocompletion. A typical
production file:

```yaml
listen: ":8080"
base_url: "https://cache.example.com" # https so session cookies are Secure
data_dir: "/var/lib/xilo"

multi_tenant: false

gc:
  interval: "12h"    # background sweep
  retention: "720h"  # evict paths not pulled in 30 days

upstream_keys: ["cache.nixos.org-1"] # don't re-cache nixpkgs

# Secrets (admin.password, database.salt, storage.s3.*_key, smtp.password)
# are better supplied via environment — see below.
```

### Environment variables

Env overrides the corresponding YAML key. Intended for secrets (keep them
out of config files and the Nix store) and for file-less Docker deployments:

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
| `XILO_S3_BUCKET` | `storage.s3.bucket` (also selects the s3 backend, unless the YAML set `storage.backend` explicitly) |
| `XILO_S3_REGION` | `storage.s3.region` |
| `XILO_S3_ACCESS_KEY` | `storage.s3.access_key` |
| `XILO_S3_SECRET_KEY` | `storage.s3.secret_key` |
| `XILO_S3_INSECURE` | `storage.s3.insecure` (`true`/`1` enables; cannot turn a YAML `true` back off) |

(The client CLI reads its own set: `XILO_URL`, `XILO_TOKEN`, `XILO_CACHE`.)

### Environment file

For systemd (`services.xilo.environmentFile`) or Docker (`--env-file`,
compose `env_file:`) the variables go in a plain env file — one
`KEY=value` per line, no `export`, no quotes, `#` starts a comment:

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
[`xilo.example.yaml`](./xilo.example.yaml) goes here — but never secrets,
the Nix store is world-readable) and passed to the service via `--config`;
`environmentFile` becomes the unit's systemd `EnvironmentFile` and holds
the secrets, in the format above:

```nix
{ inputs, ... }: {
  imports = [ inputs.xilo.nixosModules.default ];

  services.xilo = {
    enable = true; # systemd unit + client CLI in systemPackages

    settings = {
      # listen defaults to ":8080", data_dir to /var/lib/xilo.
      base_url = "https://cache.example.com";
      gc = {
        interval = "12h";
        retention = "720h";
      };
      upstream_keys = [ "cache.nixos.org-1" ];
      # S3 storage — keys come from environmentFile:
      storage = {
        backend = "s3";
        s3 = {
          endpoint = "s3.amazonaws.com";
          bucket = "xilo";
          region = "us-east-1";
        };
      };
    };

    # Path to an env file that exists on the target machine, outside the
    # Nix store — deployed by hand or by a secrets tool:
    environmentFile = "/run/secrets/xilo.env";
    # sops-nix:  environmentFile = config.sops.secrets."xilo.env".path;
    # agenix:    environmentFile = config.age.secrets."xilo.env".path;
  };
}
```

### home-manager module

Installs the CLI; `settings` optionally writes
`~/.config/xilo/xilo.yaml`, which `xilo serve` picks up via the lookup
order above:

```nix
{ inputs, ... }: {
  imports = [ inputs.xilo.homeModules.default ];

  programs.xilo = {
    enable = true; # the CLI — enough for client-only machines

    # Only for running a user-level server (`xilo serve`):
    settings = {
      listen = ":8090";
      base_url = "http://localhost:8090";
      data_dir = "/home/me/.local/share/xilo";
    };
  };
}
```

### Client config

`xilo login` saves server profiles to `~/.config/xilo/config.yaml`
(`$XDG_CONFIG_HOME` respected) — a different file from the server's `xilo.yaml`, and
managed entirely by the CLI. Overridable per-invocation with `XILO_URL` /
`XILO_TOKEN`, or per-command flags.

## Development

```sh
nix develop          # go, templ, air, just, golangci-lint …
just                 # list recipes
just dev             # live-reload server (air)
just generate        # regenerate templ views
just check           # everything CI runs: lint, test, schema + nix build in sync
just update          # bump deps + the flake vendorHash together (never edit hashes by hand)
```

The admin UI is [templ](https://templ.guide/) components (the [templUI](https://templui.io/) library) styled with [Tailwind CSS v4](https://tailwindcss.com/), compiled to a single embedded stylesheet at build time — no CDN, no runtime JS framework. The generated `*_templ.go` and CSS are rebuilt by `just` and git-ignored; `schemas/xilo.schema.json` is committed and verified in sync by CI.

## How it works

A push runs `nix path-info` for the closure, chunks each NAR client-side (FastCDC),
uploads only the chunks the server lacks, then registers the path metadata. Serving
speaks the standard Nix binary-cache protocol under the `/c/{account}/{cache}` mount:
`/c/{account}/{cache}/nix-cache-info`, `/c/{account}/{cache}/{hash}.narinfo` (signed on
the fly with the cache's ed25519 key so pushers never hold the signing key), and
`/c/{account}/{cache}/nar/{hash}.nar` (reassembled from chunks).
