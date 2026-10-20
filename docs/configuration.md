# Configuration

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
[`xilo.example.yaml`](../xilo.example.yaml) is the annotated reference with all
defaults, and [`schemas/xilo.schema.json`](../schemas/xilo.schema.json) is
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
[`xilo.example.yaml`](../xilo.example.yaml) belongs here, but never secrets: the
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
cross-built `xilo-riscv64` as shown under [Nix](install.md#nix).

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

## Reverse proxy and TLS

xilo speaks plain HTTP; terminate TLS with Caddy, nginx or equivalent and set
`base_url: "https://..."` so session cookies are `Secure`. See
[`examples/Caddyfile`](../examples/Caddyfile).

Every push request is small (one chunk, at most `chunking.max_size` or
`chunking.nar_threshold`, whichever is larger), so proxy body limits such as
Cloudflare's 100 MB never apply. xilo keeps idle connections open for 16
minutes, longer than any common proxy reuses one (Cloudflare: 900 s), so a
proxy never sends a request down a connection xilo has just closed; one with a
longer reuse window would answer those requests with a 502 or 520.

The real client IP, used for login rate limiting and the activity log, is read
from `X-Forwarded-For` or `X-Real-IP` when the proxy sits on a loopback or
private address, which needs no configuration in the usual colocated setup. For
chained proxies set `trusted_proxy_hops` to the number of hops in front of xilo
(each must append to `X-Forwarded-For` rather than overwrite it); `-1` disables
proxy trust and keys on the socket peer instead.
