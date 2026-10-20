# Operations

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

## Observability

- `GET /healthz`: readiness probe, performs a database read.
- `GET /metrics`: Prometheus counters (`xilo_narinfo_hits_total`,
  `xilo_narinfo_misses_total`, `xilo_nar_bytes_total`,
  `xilo_chunks_received_total`, `xilo_chunks_deduped_total`,
  `xilo_paths_pushed_total`, `xilo_paths_adopted_total`,
  `xilo_auth_failures_total`, with pull-serving and push-upload latency counted
  separately) plus Go runtime gauges and `xilo_http_connections_open`. Counters
  are persisted alongside the metadata, so totals and the dashboard figures
  survive restarts. A Grafana dashboard is in
  [`examples/grafana-dashboard.json`](../examples/grafana-dashboard.json).
- Activity log: every successful admin or API mutation is recorded with actor,
  method, path, source IP, user agent, latency and status, and is browsable at
  `/admin/audit`. A background job trims entries past `gc.audit_retention`.
- Request logging and graceful shutdown are built in. `logging: quiet` logs
  only errors and slow requests, which is measurable at tens of thousands of
  requests per second.
