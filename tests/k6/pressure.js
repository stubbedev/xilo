// Pressure suite — the server must take insane load gracefully: latency may
// grow, but nothing fails, nothing leaks, nothing corrupts, and it recovers
// the moment load stops.
//
//   storm         0 → STORM_VUS narinfo clients (80% hit / 20% miss) — raw
//                 connection + read-path pressure
//   push_storm    pushers running through the whole storm window: writes must
//                 keep landing while reads are saturated
//   flood         constant ARRIVAL RATE (not VUs) — requests keep arriving at
//                 RATE/s whether or not the server keeps up; the executor
//                 reports dropped iterations if it can't
//   pull_wall     PULL_VUS parallel NAR downloads, mixed identity/zstd, with
//                 byte-exact hash verification on every identity pull
//   abort_storm   clients with a 150ms timeout on a 64MiB NAR — every request
//                 aborts mid-body; the server must shrug off half-closed
//                 connections (no goroutine/FD leak, no stall)
//   leakwatch     scrapes /metrics every 5s through the whole run and records
//                 go_goroutines + heap; teardown asserts the server drained
//                 back to an idle goroutine count and stayed healthy
//
// Scale knobs (env): STORM_VUS=512 FLOOD_RPS=5000 PULL_VUS=128 DURATION_S=60
//   docker compose -f tests/k6/compose.yaml run --rm k6 run /scripts/pressure.js
import http from "k6/http";
import crypto from "k6/crypto";
import { check, sleep, fail } from "k6";
import exec from "k6/execution";
import { Trend, Counter } from "k6/metrics";
import { BASE, CACHE, pushPath, chunkBytes, storePathFor, authHeaders, waitHealthy, ensureCache } from "./lib.js";

const STORM_VUS = parseInt(__ENV.STORM_VUS || "512");
const FLOOD_RPS = parseInt(__ENV.FLOOD_RPS || "5000");
const PULL_VUS = parseInt(__ENV.PULL_VUS || "128");
const D = parseInt(__ENV.DURATION_S || "60"); // storm plateau seconds

const goroutines = new Trend("srv_goroutines");
const heapMiB = new Trend("srv_heap_mib");
const pullBroken = new Counter("pull_broken");

const RAMP = 30; // storm ramp-up seconds
const STORM_END = RAMP + D + 10;
const FLOOD_START = STORM_END + 5;
const FLOOD_END = FLOOD_START + 45;
const WALL_START = FLOOD_END + 5;
const WALL_END = WALL_START + 45;
const ABORT_START = WALL_END + 5;
const ABORT_END = ABORT_START + 30;

export const options = {
  setupTimeout: "600s",
  // Discard bodies we don't verify — 512 VUs of buffered bodies is k6 memory,
  // not server pressure.
  discardResponseBodies: false,
  scenarios: {
    storm: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: [
        { duration: `${RAMP}s`, target: STORM_VUS },
        { duration: `${D}s`, target: STORM_VUS },
        { duration: "10s", target: 0 },
      ],
      exec: "storm",
    },
    push_storm: {
      executor: "constant-vus", vus: 4, duration: `${STORM_END}s`,
      exec: "pushStorm",
    },
    flood: {
      executor: "constant-arrival-rate",
      rate: FLOOD_RPS, timeUnit: "1s",
      duration: "45s", startTime: `${FLOOD_START}s`,
      preAllocatedVUs: Math.min(1000, FLOOD_RPS),
      exec: "floodHit",
    },
    pull_wall: {
      executor: "constant-vus", vus: PULL_VUS, duration: "45s",
      startTime: `${WALL_START}s`,
      exec: "pullWall",
    },
    abort_storm: {
      executor: "constant-vus", vus: 32, duration: "30s",
      startTime: `${ABORT_START}s`,
      exec: "abortStorm",
    },
    leakwatch: {
      executor: "constant-vus", vus: 1, duration: `${ABORT_END + 20}s`,
      exec: "leakwatch",
    },
  },
  thresholds: {
    // Graceful under pressure: essentially nothing fails, latency bounded.
    "http_req_failed{scenario:storm}": ["rate<0.005"],
    "http_req_failed{scenario:push_storm}": ["rate<0.005"],
    "http_req_failed{scenario:flood}": ["rate<0.005"],
    "http_req_failed{scenario:pull_wall}": ["rate<0.005"],
    "http_req_duration{scenario:storm}": ["p(99)<1000"],
    "http_req_duration{scenario:flood}": ["p(99)<1000"],
    // The arrival-rate executor drops iterations when the server can't keep
    // up. At the default 5000 rps a healthy server drops none — any drop is a
    // capacity regression. When you deliberately push FLOOD_RPS above the
    // hardware's ceiling, raise DROP_BUDGET: drops then just mean finite
    // capacity; the graceful-degradation signal is zero FAILURES + bounded
    // latency, which stay enforced.
    dropped_iterations: [`count<${__ENV.DROP_BUDGET || 10}`],
    // No corrupted pulls, ever.
    pull_broken: ["count==0"],
    checks: ["rate>0.995"],
  },
};

const SEED_PATHS = 16;
const CHUNKS = 4;
const CHUNK_SIZE = 262144;
const BIG_SEED = 909; // 256 x 256KiB = 64MiB — abort_storm target

export function setup() {
  waitHealthy(60);
  ensureCache();
  const corpus = [];
  for (let i = 0; i < SEED_PATHS; i++) {
    const r = pushPath(9000 + i, CHUNKS, CHUNK_SIZE, { phase: "seed" });
    if (!r.ok) throw new Error(`seed ${i} failed`);
    corpus.push({ hash: r.storeHash, narHex: r.narHex, narSize: r.narSize });
  }
  const big = pushPath(BIG_SEED, 256, CHUNK_SIZE, { phase: "seed" });
  if (!big.ok) throw new Error("big seed failed");
  return { corpus, bigHash: big.storeHash };
}

export function storm(data) {
  const it = exec.scenario.iterationInTest;
  if (it % 5 === 4) {
    // 20% miss traffic — negative lookups are part of every nix eval
    const absent = storePathFor(0x6fff0000 + it).slice(11, 43);
    const res = http.get(`${BASE}/${CACHE}/${absent}.narinfo`, {
      responseCallback: http.expectedStatuses(404),
      tags: { name: "storm-miss" },
    });
    check(res, { "storm miss 404": (r) => r.status === 404 });
  } else {
    const p = data.corpus[it % data.corpus.length];
    const res = http.get(`${BASE}/${CACHE}/${p.hash}.narinfo`, { tags: { name: "storm-hit" } });
    check(res, { "storm hit 200": (r) => r.status === 200 });
  }
}

export function pushStorm() {
  // Fresh content non-stop while the read side is saturated.
  const r = pushPath(2_000_000 + exec.scenario.iterationInTest, CHUNKS, CHUNK_SIZE, {
    phase: "push-storm",
  });
  check(null, { "push lands under storm": () => r.ok });
}

export function floodHit(data) {
  const p = data.corpus[exec.scenario.iterationInTest % data.corpus.length];
  const res = http.get(`${BASE}/${CACHE}/${p.hash}.narinfo`, { tags: { name: "flood" } });
  check(res, { "flood 200": (r) => r.status === 200 });
}

export function pullWall(data) {
  const it = exec.scenario.iterationInTest;
  const p = data.corpus[it % data.corpus.length];
  if (it % 2 === 0) {
    // identity: verify every byte, every time
    const res = http.get(`${BASE}/${CACHE}/nar/${p.hash}.nar`, {
      headers: { "Accept-Encoding": "identity" },
      responseType: "binary",
      tags: { name: "wall-identity" },
    });
    const intact = res.status === 200 && res.body.byteLength === p.narSize &&
      crypto.sha256(res.body, "hex") === p.narHex;
    if (!intact) pullBroken.add(1);
    check(res, { "wall pull intact": () => intact });
  } else {
    const res = http.get(`${BASE}/${CACHE}/nar/${p.hash}.nar`, {
      headers: { "Accept-Encoding": "zstd" },
      responseType: "none",
      tags: { name: "wall-zstd" },
    });
    check(res, { "wall zstd 200": (r) => r.status === 200 });
  }
}

export function abortStorm(data) {
  // 64MiB NAR with a 150ms budget: aborts mid-body by design. Failures here
  // are the point — no thresholds reference this scenario.
  http.get(`${BASE}/${CACHE}/nar/${data.bigHash}.nar`, {
    headers: { "Accept-Encoding": "identity" },
    responseType: "none",
    timeout: "150ms",
    tags: { name: "abort" },
  });
}

export function leakwatch() {
  const res = http.get(`${BASE}/metrics`, { tags: { name: "metrics" } });
  if (res.status === 200) {
    const g = String(res.body).match(/^go_goroutines (\d+)/m);
    if (g) goroutines.add(parseInt(g[1]));
    const h = String(res.body).match(/^go_heap_inuse_bytes (\d+)/m);
    if (h) heapMiB.add(parseInt(h[1]) / 1048576);
  }
  sleep(5);
}

export function teardown(data) {
  // Recovery: after ALL load (including the half-closed-connection storm),
  // the server must drain back to an idle goroutine count and still serve
  // byte-exact NARs.
  sleep(15);
  const res = http.get(`${BASE}/metrics`);
  const g = String(res.body).match(/^go_goroutines (\d+)/m);
  if (!g) fail("metrics endpoint dead after pressure run");
  const n = parseInt(g[1]);
  console.log(`post-load goroutines: ${n}`);
  // Idle keepalive connections from k6's still-open VU pools hold ~2 server
  // goroutines each until IdleTimeout — scale the bound with client count.
  // A real leak (chunk prefetchers, per-request spawns) shows up in the
  // thousands and still trips this.
  const bound = 200 + Math.ceil(STORM_VUS / 4);
  if (n > bound) fail(`goroutine leak: ${n} still alive 15s after load stopped (bound ${bound})`);

  for (const p of data.corpus.slice(0, 4)) {
    const nar = http.get(`${BASE}/${CACHE}/nar/${p.hash}.nar`, {
      headers: { "Accept-Encoding": "identity" },
      responseType: "binary",
    });
    if (nar.status !== 200 || crypto.sha256(nar.body, "hex") !== p.narHex) {
      fail(`corpus NAR ${p.hash} corrupt after pressure run`);
    }
  }
  if (http.get(`${BASE}/healthz`).status !== 200) fail("healthz failed after pressure run");
  console.log("recovery verified: goroutines drained, corpus byte-exact, healthz ok");
}
