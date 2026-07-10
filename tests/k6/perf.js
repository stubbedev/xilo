// Performance suite — the numbers to watch release over release.
// Staggered scenarios cover everything that matters for a fast substituter:
//
//   narinfo_hit    nix blocks on these during eval — the hottest read
//   narinfo_miss   mass-query for absent paths (negative lookups, 404 path)
//   pull_identity  NAR streaming, uncompressed wire
//   pull_zstd      NAR streaming, stored-frame passthrough wire
//   pull_big       single large NAR (64 MiB) streaming throughput
//   push_dedup     re-push of already-cached content — the CI hot path
//                  (get-missing-chunks + put-path verify, zero uploads)
//   push_fresh     full pipeline: chunk PUTs + verified registration
//   push_saturate  ramping pushers 4→32 VUs — ingest ceiling + degradation
//   mixed_*        pushers + narinfo storm + pulls simultaneously; reads must
//                  stay fast while ingest is saturated (single-writer claim)
//
//   docker compose -f tests/k6/compose.yaml run --rm k6
//
// Thresholds are loose sanity floors (CI runners vary); the real signal is
// the exported summary tracked per release.
import http from "k6/http";
import { check } from "k6";
import exec from "k6/execution";
import { BASE, CACHE, pushPath, storePathFor, waitHealthy, ensureCache } from "./lib.js";

const SEED_PATHS = 12; // pull corpus: 12 paths x 4 x 256 KiB
const CHUNKS = 4;
const CHUNK_SIZE = 256 * 1024;
const BIG_CHUNKS = 256; // 256 x 256 KiB = 64 MiB
const BIG_SEED = 777;

export const options = {
  setupTimeout: "300s",
  scenarios: {
    narinfo_hit: {
      executor: "constant-vus", vus: 16, duration: "30s",
      exec: "narinfoHit",
    },
    narinfo_miss: {
      executor: "constant-vus", vus: 8, duration: "30s",
      exec: "narinfoMiss",
    },
    pull_identity: {
      executor: "constant-vus", vus: 8, duration: "30s", startTime: "35s",
      exec: "pullIdentity",
    },
    pull_zstd: {
      executor: "constant-vus", vus: 8, duration: "30s", startTime: "70s",
      exec: "pullZstd",
    },
    pull_big: {
      executor: "constant-vus", vus: 4, duration: "20s", startTime: "105s",
      exec: "pullBig",
    },
    push_dedup: {
      executor: "constant-vus", vus: 8, duration: "30s", startTime: "130s",
      exec: "pushDedup",
    },
    push_fresh: {
      executor: "constant-vus", vus: 8, duration: "30s", startTime: "165s",
      exec: "pushFresh",
    },
    push_saturate: {
      executor: "ramping-vus", startVUs: 4, startTime: "200s",
      stages: [
        { duration: "30s", target: 16 },
        { duration: "30s", target: 32 },
      ],
      exec: "pushSaturate",
    },
    // mixed window: all three at once
    mixed_push: {
      executor: "constant-vus", vus: 6, duration: "45s", startTime: "265s",
      exec: "mixedPush",
    },
    mixed_narinfo: {
      executor: "constant-vus", vus: 16, duration: "45s", startTime: "265s",
      exec: "narinfoHit",
    },
    mixed_pull: {
      executor: "constant-vus", vus: 8, duration: "45s", startTime: "265s",
      exec: "pullZstd",
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    "http_req_duration{scenario:narinfo_hit}": ["p(95)<100"],
    "http_req_duration{scenario:narinfo_miss}": ["p(95)<100"],
    "http_req_duration{scenario:pull_identity}": ["p(95)<2000"],
    "http_req_duration{scenario:pull_zstd}": ["p(95)<2000"],
    "http_req_duration{scenario:pull_big}": ["p(95)<10000"],
    "iteration_duration{scenario:push_dedup}": ["p(95)<1000"],
    "iteration_duration{scenario:push_fresh}": ["p(95)<5000"],
    // The substituter SLO: reads stay fast while pushers hammer the server.
    "http_req_duration{scenario:mixed_narinfo}": ["p(95)<300"],
  },
};

export function setup() {
  waitHealthy(60);
  ensureCache();
  const hashes = [];
  for (let i = 0; i < SEED_PATHS; i++) {
    const r = pushPath(1000 + i, CHUNKS, CHUNK_SIZE, { phase: "seed" });
    if (!r.ok) throw new Error(`seeding path ${i} failed`);
    hashes.push(r.storeHash);
  }
  const big = pushPath(BIG_SEED, BIG_CHUNKS, CHUNK_SIZE, { phase: "seed" });
  if (!big.ok) throw new Error("seeding big path failed");
  return { hashes, bigHash: big.storeHash };
}

function pick(data) {
  return data.hashes[exec.scenario.iterationInTest % data.hashes.length];
}

export function narinfoHit(data) {
  const res = http.get(`${BASE}/${CACHE}/${pick(data)}.narinfo`, {
    tags: { name: "narinfo" },
  });
  check(res, { "narinfo 200": (r) => r.status === 200 });
}

const expect404 = http.expectedStatuses(404);

export function narinfoMiss() {
  // Deterministic absent store hashes — exercises the 404/negative-cache path
  // nix hits for every path the cache doesn't have during mass query. The 404
  // is the correct answer, so it must not count into http_req_failed.
  const absent = storePathFor(0x7fff0000 + exec.scenario.iterationInTest).slice(11, 43);
  const res = http.get(`${BASE}/${CACHE}/${absent}.narinfo`, {
    responseCallback: expect404,
    tags: { name: "narinfo-miss" },
  });
  check(res, { "narinfo 404": (r) => r.status === 404 });
}

export function pullIdentity(data) {
  const res = http.get(`${BASE}/${CACHE}/nar/${pick(data)}.nar`, {
    headers: { "Accept-Encoding": "identity" },
    responseType: "none",
    tags: { name: "nar-identity" },
  });
  check(res, { "nar 200": (r) => r.status === 200 });
}

export function pullZstd(data) {
  // k6 doesn't decode zstd; we measure the server serving stored frames.
  const res = http.get(`${BASE}/${CACHE}/nar/${pick(data)}.nar`, {
    headers: { "Accept-Encoding": "zstd" },
    responseType: "none",
    tags: { name: "nar-zstd" },
  });
  check(res, { "nar 200": (r) => r.status === 200 });
}

export function pullBig(data) {
  const res = http.get(`${BASE}/${CACHE}/nar/${data.bigHash}.nar`, {
    headers: { "Accept-Encoding": "identity" },
    responseType: "none",
    tags: { name: "nar-big" },
  });
  check(res, { "big nar 200": (r) => r.status === 200 });
}

export function pushDedup() {
  // Same seeds as the pull corpus: every chunk already exists, so this is
  // get-missing-chunks + put-path (with server-side reassembly verify) only.
  pushPath(1000 + (exec.scenario.iterationInTest % SEED_PATHS), CHUNKS, CHUNK_SIZE, {
    phase: "dedup",
  });
}

export function pushFresh() {
  pushPath(1_000_000 + exec.scenario.iterationInTest, CHUNKS, CHUNK_SIZE, {
    phase: "fresh",
  });
}

export function pushSaturate() {
  pushPath(2_000_000 + exec.scenario.iterationInTest, CHUNKS, CHUNK_SIZE, {
    phase: "saturate",
  });
}

export function mixedPush() {
  pushPath(3_000_000 + exec.scenario.iterationInTest, CHUNKS, CHUNK_SIZE, {
    phase: "mixed",
  });
}
