# k6 performance & integrity suite

Repeatable load tests against a containerized xilo — the same numbers, release
after release, instead of one-off benchmarks. CI runs these on every push to
master and on tags ([`perf.yml`](../../.github/workflows/perf.yml)) and
uploads the k6 summary as an artifact per ref.

The push scenarios speak the real wire protocol (chunk `PUT` + `put-path` with
server-side reassembly verification) with deterministic generated content —
no Nix store needed, fully hermetic.

## Scripts

| script | what it measures | what fails it |
|---|---|---|
| `ops.js` | **every operation, once, with correctness assertions**: health/metrics/statics, full push wire protocol incl. all rejection paths (wrong hash, wrong order, wrong size, bad JSON, unknown chunk), narinfo contract (signature, headers, negative cache), byte-exact NAR on identity/gzip/zstd, the full private-cache auth matrix (anonymous/pull/push/revoked/wrong-scope tokens), and the whole admin surface — cache CRUD + rotate + GC + delete, token create/edit/revoke, search/sort/paging, status ranges, password change, complete TOTP 2FA cycle (enroll → enable → 2FA login → disable, codes computed in-script) | any single failed check (`rate==1`) |
| `perf.js` | staggered scenarios over everything a fast substituter needs: narinfo hit + miss (mass query), NAR pull (identity, zstd stored-frame wire, 64 MiB big NAR), dedup re-push (the CI hot path), fresh push, a 4→32 VU push **saturation ramp**, and a **mixed window** (pushers + narinfo storm + pulls at once — reads must stay <300 ms p95 under full ingest) | error rate ≥ 1 %, per-scenario latency floors (loose; the tracked summary is the signal) |
| `pressure.js` | graceful behavior at the edge: a 512-VU narinfo **storm** (with pushers running through it), a 5000 rps **constant-arrival flood** (arrival rate keeps coming whether or not the server keeps up — dropped iterations are the collapse signal), a 128-VU **pull wall** with byte-exact verification on every identity pull, an **abort storm** (32 clients killing 64 MiB downloads after 150 ms, all run long), and a **leak watch** scraping `go_goroutines`/heap every 5 s; teardown asserts goroutines drain back to idle, the corpus is still byte-exact, and healthz is green. Scale: `STORM_VUS=1024 FLOOD_RPS=10000 just k6-pressure` | any scenario failing >0.5 %, ≥10 dropped arrivals, any broken pull, goroutine leak after load |
| `churn.js` | integrity under hostile GC (5 s sweeps, 30 s retention, 15 s grace): recurring dedup pushes, an orphan flood keeping the sweeper busy, and a lapsing path set that gets evicted+swept then re-pushed — every pushed NAR is pulled back and byte-hash-verified | any dropped or corrupt NAR (`nar_broken > 0`) |

Per-scenario throughput is derivable from the summary: iteration counts ×
known payload (seed paths are 4 × 256 KiB; the big NAR is 64 MiB).

## Run

```sh
just k6-ops      # operations conformance (fast, every endpoint)
just k6-perf     # throughput/latency numbers
just k6-churn    # integrity soak (3m; DURATION=10m just k6-churn for longer)
just k6-race     # churn against a `go run -race` server build
just e2e         # CLI end-to-end (tests/e2e/cli.sh — real nix + container)
```

Or raw compose — see the header of [`compose.yaml`](./compose.yaml).

Summary JSON lands in the `k6-out` volume (`--summary-export`); CI uploads it
as `k6-summary-<ref>`.
