# k6 performance & integrity suite

Repeatable load tests against a containerized xilo — the same numbers, release
after release, instead of one-off benchmarks. CI runs these on every push to
master and on tags ([`perf.yml`](../../.github/workflows/perf.yml)) and
uploads the k6 summary as an artifact per ref.

The push scenarios speak the real wire protocol (chunk `PUT` + `put-path` with
server-side reassembly verification) with deterministic generated content —
no Nix store needed, fully hermetic.

## Tenancy

Every cache is mounted at `/c/{account}/{cache}/…`. The suites run in two modes:

- **Single-tenant** (default, `server-perf.yaml` / `server-churn.yaml`): one
  `default` account, open bootstrap on, pushes unauthenticated.
- **Multi-tenant** (`server-mt.yaml` / `server-mt-churn.yaml`, `multi_tenant:
  true`): `mt.js` proves the tenancy surface end to end; the load scripts take
  `TENANTS=N` to spread work across `N` org accounts, each with its own cache
  and a scoped push token (minted by `provisionTenants` in `lib.js`), so
  per-account isolation is exercised under pressure. `ACCOUNT=<slug>` runs a
  whole suite against a single named account instead.

## Scripts

| script | what it measures | what fails it |
|---|---|---|
| `ops.js` | **every operation, once, with correctness assertions** (single-tenant): health/metrics/statics, full push wire protocol incl. all rejection paths (wrong hash, wrong order, wrong size, bad JSON, unknown chunk), narinfo contract (signature, headers, negative cache), byte-exact NAR on identity/gzip/zstd, the full private-cache auth matrix (anonymous/pull/push/revoked/wrong-scope tokens), and the whole admin surface — cache CRUD + rotate + GC + delete, token create/edit/revoke, search/sort/paging, status ranges, account email, password change, complete TOTP 2FA cycle (enroll → enable → 2FA login → disable, codes computed in-script) | any single failed check (`rate==1`) |
| `mt.js` | **the multi-tenant surface, once, with correctness assertions**: `/register` closed until an owner opens it, self-registration with plan selection, the pending → approve → sign-in flow, the instant-registration toggle, plan CRUD, all three plan quotas (cache count, storage bytes → push 403, org member count), plan-gated org creation (`/admin/neworg`), cross-account isolation of both scoped tokens (401 across accounts) and dashboard cache pages (404), and the registration rate limit (429 after burst) | any single failed check (`rate==1`) |
| `perf.js` | staggered scenarios over everything a fast substituter needs: narinfo hit + miss (mass query), NAR pull (identity, zstd stored-frame wire, 64 MiB big NAR), dedup re-push (the CI hot path), fresh push, a 4→32 VU push **saturation ramp**, and a **mixed window** (pushers + narinfo storm + pulls at once — reads must stay fast under full ingest). `TENANTS=N` spreads it across N accounts | error rate ≥ 1 %, per-scenario latency floors (loose; the tracked summary is the signal) |
| `pressure.js` | graceful behavior at the edge: a 512-VU narinfo **storm** (with pushers running through it), a 5000 rps **constant-arrival flood** (arrival rate keeps coming whether or not the server keeps up — dropped iterations are the collapse signal), a 128-VU **pull wall** with byte-exact verification on every identity pull, an **abort storm** (32 clients killing 64 MiB downloads after 150 ms, all run long), and a **leak watch** scraping `go_goroutines`/heap every 5 s; teardown asserts goroutines drain back to idle, the corpus is still byte-exact, and healthz is green. `TENANTS=N` shards every scenario across N accounts. Scale: `STORM_VUS=1024 FLOOD_RPS=10000 just k6-pressure` | any scenario failing >0.5 %, ≥10 dropped arrivals, any broken pull, goroutine leak after load |
| `churn.js` | integrity under hostile GC (5 s sweeps, 30 s retention, 15 s grace): recurring dedup pushes, an orphan flood keeping the sweeper busy, and a lapsing path set that gets evicted+swept then re-pushed — every pushed NAR is pulled back and byte-hash-verified. `TENANTS=N` runs the churn across N accounts at once | any dropped or corrupt NAR (`nar_broken > 0`) |
| `deep.js` | edge dimensions: a 1000-chunk (64 MiB) NAR crossing the 900-var SQLite IN-batch boundary, 1 MiB max-size chunk PUTs, a 10 000-distinct-path narinfo storm, gzip wire byte-exact under load | any failed check (`rate==1`), error rate ≥ 1 % |

Per-scenario throughput is derivable from the summary: iteration counts ×
known payload (seed paths are 4 × 256 KiB; the big NAR is 64 MiB).

## Run

```sh
just k6-ops         # single-tenant operations conformance (fast, every endpoint)
just k6-mt          # multi-tenant conformance (registration, plans, quotas, isolation)
just k6-deep        # edge-dimension stress
just k6-perf        # throughput/latency numbers (single-tenant)
just k6-perf-mt     # same, load spread across 4 accounts
just k6-churn       # integrity soak (3m; DURATION=10m just k6-churn for longer)
just k6-churn-mt    # integrity soak across 4 accounts
just k6-pressure    # graceful-degradation (single-tenant)
just k6-pressure-mt # graceful-degradation across 4 accounts
just k6-race        # churn against a `go run -race` server build
just e2e            # CLI end-to-end (tests/e2e/cli.sh — real nix + container)
```

Or raw compose — see the header of [`compose.yaml`](./compose.yaml).

Summary JSON lands in the `k6-out` volume (`--summary-export`); CI uploads it
as `k6-summary-<ref>`.
