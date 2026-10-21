# Features

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
  transfers, and a published [JSON schema](../schemas/xilo.schema.json) for the
  config file.

### Deployment

- One static, cgo-free binary in a distroless image, published for
  `linux/amd64` and `linux/arm64`. Its size sits beside the alternatives' in
  the [comparison](benchmarks.md#against-other-nix-caches).
- Prebuilt binaries for `linux` amd64/arm64/riscv64 and `darwin` arm64.
- A flake with a NixOS module, a home-manager module, and a server-less client
  build for platforms without a Tailwind toolchain.

## Feature comparison

The same implementations the [benchmarks](benchmarks.md) measure, on what they do
rather than how fast they do it. The two object stores share a column because
`nix copy` into a bucket is the same arrangement either way; where they differ
is the last row, and it decides whether a bucket can be a public cache at all.

|  | xilo | attic | harmonia | nix-serve-ng | MinIO / Garage + `nix copy` |
|---|---|---|---|---|---|
| what it serves | its own chunk store | its own chunk store | the host `/nix/store` | the host `/nix/store` | objects a client uploaded |
| accepts a push from another machine | over its own API | over its own API | no | no | `nix copy --to s3://…` |
| chunked dedup (FastCDC) | yes | yes | no, the store is the state | no, the store is the state | no, one NAR per path |
| compression at rest | zstd per chunk | zstd per chunk | none, serves the store | none, serves the store | xz, by `nix copy` |
| metadata store | SQLite or PostgreSQL | SQLite or PostgreSQL | none | none | none |
| storage backends | local or S3, several named, per cache | local or S3, one | n/a | n/a | itself |
| multi-tenancy | accounts, organizations, users, roles | per-cache only | no | no | bucket policies, not caches |
| private caches | yes, per-cache tokens | yes, JWT | no, all or nothing | no, all or nothing | bucket credentials |
| token revocation | immediate (database-backed) | no (stateless JWT) | n/a | n/a | key deletion |
| token scope | one `account/cache`, plus `manage`/`admin` perms | JWT cache patterns | n/a | n/a | whole bucket |
| server-managed signing keys | yes, with rotation | yes | reads a key file | reads a key file | signed by the pushing client |
| retention and GC | time and size caps per cache, plus a global ceiling | time only | the host's own GC | the host's own GC | bucket lifecycle rules |
| incomplete data | fails closed | can serve truncated `200`s | n/a, the store is complete | n/a, the store is complete | serves whatever landed |
| web dashboard | yes (caches, users, accounts, tokens, status, activity) | no | no | no | the store's own console |
| Prometheus metrics | yes | no | yes | no | yes |
| store-watch auto-push | yes | yes | n/a | n/a | no |
| integrity fsck and repair | yes | no | n/a | n/a | no |
| anonymous public reads | yes, per cache | yes, per cache | yes | yes | MinIO: a download policy. Garage: only from its web endpoint, which routes by `Host`; its S3 API answers unauthenticated reads with `Garage does not support anonymous access yet` |
