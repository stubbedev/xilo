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
| `perf.js` | staggered scenarios over everything a fast substituter needs: narinfo hit + miss (mass query), NAR pull (identity, zstd stored-frame wire, 64 MiB big NAR), dedup re-push (the CI hot path), fresh push, a 4→32 VU push **saturation ramp**, and a **mixed window** (pushers + narinfo storm + pulls at once — reads must stay <300 ms p95 under full ingest) | error rate ≥ 1 %, per-scenario latency floors (loose; the tracked summary is the signal) |
| `churn.js` | integrity under hostile GC (5 s sweeps, 30 s retention, 15 s grace): recurring dedup pushes, an orphan flood keeping the sweeper busy, and a lapsing path set that gets evicted+swept then re-pushed — every pushed NAR is pulled back and byte-hash-verified | any dropped or corrupt NAR (`nar_broken > 0`) |

Per-scenario throughput is derivable from the summary: iteration counts ×
known payload (seed paths are 4 × 256 KiB; the big NAR is 64 MiB).

## Run

```sh
just k6-perf     # throughput/latency numbers
just k6-churn    # integrity soak (3m; DURATION=10m just k6-churn for longer)
just k6-race     # churn against a `go run -race` server build
```

Or raw compose — see the header of [`compose.yaml`](./compose.yaml).

Summary JSON lands in the `k6-out` volume (`--summary-export`); CI uploads it
as `k6-summary-<ref>`.
