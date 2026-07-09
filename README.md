# xilo

Self-hosted [Nix binary cache](https://nix.dev/manual/nix/latest/store/types/http-binary-cache-store) — a single Go binary, no external services. An alternative to [attic](https://github.com/zhaofengli/attic) that:

- **never stalls on concurrent pushes** — pure-Go SQLite (WAL) with a single writer goroutine; chunk bytes live in the storage backend, never the DB, so pushes take no long-held lock
- ships a **cachix-style admin dashboard** to manage caches and tokens
- can **revoke push/pull tokens** instantly
- does **content-addressed chunked dedup** (FastCDC) across all caches
- stores chunks on **local disk or any S3-compatible bucket** (AWS, [Garage](https://garagehq.deuxfleurs.fr/), R2, …)
- publishes a **27 MB distroless Docker image**

## Quick start

```sh
docker run -d -p 8080:8080 -v xilo-data:/data \
  -e XILO_ADMIN_PASSWORD=change-me ghcr.io/stubbedev/xilo:master
```

Open <http://localhost:8080/admin>, log in, create a cache. Or from the CLI:

```sh
xilo cache create mycache          # prints the public key + nix.conf snippet
```

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
- `GET /metrics` — Prometheus counters (narinfo hit/miss, NAR bytes, chunk dedup, pushes, auth failures).
- Request logging + graceful shutdown (drains in-flight transfers on SIGTERM) are built in.

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

## Garbage collection

Chunks are content-addressed and shared; GC is a mark-sweep over unreferenced chunks. Dashboard button, or:

```sh
xilo gc                     # sweep unreferenced chunks
xilo gc --older-than 720h   # also evict paths not pulled in 30 days, then sweep
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
