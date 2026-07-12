// Deep-end stress — edge dimensions perf.js doesn't reach:
//
//   huge_path      one NAR of 1000 chunks: crosses the 900-variable SQLite
//                  IN-batch boundary in get-missing-chunks / put-path /
//                  ChunkKeys, and the verify look-ahead window — over the wire
//   big_chunks     pushes at max chunk size (1 MiB bodies)
//   narinfo_storm  mass query across 10k DISTINCT paths (read-pool + page
//                  cache, no single-row hot spot)
//   pull_gzip      gzip wire encoding under sustained load (zstd and identity
//                  are covered in perf.js; gzip is the odd one out)
//
//   docker compose -f tests/k6/compose.yaml run --rm k6 run /scripts/deep.js
import http from "k6/http";
import crypto from "k6/crypto";
import { check } from "k6";
import exec from "k6/execution";
import { cachePrefix, pushPath, chunkBytes, storePathFor, authHeaders, waitHealthy, provisionTenants } from "./lib.js";

const STORM_PATHS = 10_000; // registered cheaply: 1 shared chunk, distinct paths

export const options = {
  setupTimeout: "600s",
  scenarios: {
    huge_path: {
      executor: "shared-iterations", vus: 1, iterations: 1, maxDuration: "300s",
      exec: "hugePath",
    },
    big_chunks: {
      executor: "constant-vus", vus: 4, duration: "30s", startTime: "0s",
      exec: "bigChunks",
    },
    narinfo_storm: {
      executor: "constant-vus", vus: 24, duration: "30s", startTime: "35s",
      exec: "narinfoStorm",
    },
    pull_gzip: {
      executor: "constant-vus", vus: 8, duration: "30s", startTime: "70s",
      exec: "pullGzip",
    },
  },
  thresholds: {
    checks: ["rate==1"],
    http_req_failed: ["rate<0.01"],
    "http_req_duration{scenario:narinfo_storm}": ["p(95)<150"],
  },
};

export function setup() {
  waitHealthy(60);
  // Single-target edge suite (the interesting dimensions are batch boundaries
  // and encodings, not tenant fan-out); tenancy-agnostic via ACCOUNT/XILO_TOKEN.
  const t = provisionTenants(0)[0];
  const prefix = cachePrefix(t);

  // Storm corpus: 10k distinct paths all referencing one tiny shared chunk —
  // cheap to create, forces distinct-row reads on every narinfo.
  const shared = chunkBytes(1, 0, 4096);
  const sharedHex = crypto.sha256(shared, "hex");
  http.put(`${prefix}/api/chunk/${sharedHex}`, shared, { headers: authHeaders({}, t) });
  const stormHashes = [];
  const batchTags = { phase: "storm-seed" };
  for (let i = 0; i < STORM_PATHS; i++) {
    const res = http.put(
      `${prefix}/api/path`,
      JSON.stringify({
        storePath: storePathFor(500_000 + i),
        narHash: `sha256:${sharedHex}`,
        narSize: 4096,
        deriver: "",
        references: [],
        chunks: [sharedHex],
      }),
      { headers: Object.assign({ "Content-Type": "application/json" }, authHeaders({}, t)), tags: batchTags },
    );
    if (res.status !== 200) throw new Error(`storm seed ${i}: ${res.status} ${res.body}`);
    stormHashes.push(storePathFor(500_000 + i).slice(11, 43));
  }

  // gzip pull corpus
  const gz = pushPath(600_000, 4, 262144, { phase: "seed" }, t);
  if (!gz.ok) throw new Error("gzip corpus seed failed");
  return { target: t, stormHashes, gzHash: gz.storeHash, gzNarHex: gz.narHex };
}

export function hugePath(data) {
  const t = data.target;
  const prefix = cachePrefix(t);
  // 1000 x 64 KiB = 64 MiB NAR; chunk list crosses the 900-var batch boundary
  // in every server-side lookup it touches.
  const seed = 700_001;
  const hexes = [];
  const hasher = crypto.createHash("sha256");
  let size = 0;
  for (let i = 0; i < 1000; i++) {
    const data = chunkBytes(seed, i, 65536);
    const hex = crypto.sha256(data, "hex");
    hexes.push(hex);
    hasher.update(data);
    size += data.byteLength;
    const res = http.put(`${prefix}/api/chunk/${hex}`, data, {
      headers: authHeaders({}, t), tags: { name: "huge-chunk" },
    });
    check(res, { "huge chunk 200": (r) => r.status === 200 });
  }
  const narHex = hasher.digest("hex");

  // get-missing-chunks with all 1000 hashes: batching must report none missing
  let res = http.post(`${prefix}/api/get-missing-chunks`, JSON.stringify({ hashes: hexes }),
    { headers: Object.assign({ "Content-Type": "application/json" }, authHeaders({}, t)) });
  check(res, { "huge missing-chunks empty": (r) => r.status === 200 && (JSON.parse(r.body).missing || []).length === 0 });

  res = http.put(`${prefix}/api/path`,
    JSON.stringify({ storePath: storePathFor(seed), narHash: `sha256:${narHex}`, narSize: size, deriver: "", references: [], chunks: hexes }),
    { headers: Object.assign({ "Content-Type": "application/json" }, authHeaders({}, t)) });
  check(res, { "huge put-path 200": (r) => r.status === 200 });

  // byte-exact read-back of all 64 MiB through the 1000-chunk reassembly
  const storeHash = storePathFor(seed).slice(11, 43);
  res = http.get(`${prefix}/nar/${storeHash}.nar`, {
    headers: { "Accept-Encoding": "identity" }, responseType: "binary", tags: { name: "huge-nar" },
  });
  check(res, {
    "huge nar byte-exact": (r) => r.status === 200 && r.body.byteLength === size && crypto.sha256(r.body, "hex") === narHex,
  });
}

export function bigChunks(data) {
  // max-size (1 MiB) chunk bodies — the largest single PUT the server accepts.
  const it = exec.scenario.iterationInTest;
  const bytes = chunkBytes(800_000 + it, 0, 1 << 20);
  const hex = crypto.sha256(bytes, "hex");
  const res = http.put(`${cachePrefix(data.target)}/api/chunk/${hex}`, bytes, {
    headers: authHeaders({}, data.target), tags: { name: "big-chunk" },
  });
  check(res, { "1MiB chunk 200": (r) => r.status === 200 });
}

export function narinfoStorm(data) {
  const h = data.stormHashes[(exec.scenario.iterationInTest * 7919) % data.stormHashes.length];
  const res = http.get(`${cachePrefix(data.target)}/${h}.narinfo`, { tags: { name: "storm" } });
  check(res, { "storm narinfo 200": (r) => r.status === 200 });
}

export function pullGzip(data) {
  // k6 decodes gzip — byte-exact hash under sustained load.
  const res = http.get(`${cachePrefix(data.target)}/nar/${data.gzHash}.nar`, {
    headers: { "Accept-Encoding": "gzip" }, responseType: "binary", tags: { name: "nar-gzip" },
  });
  check(res, { "gzip nar byte-exact": (r) => r.status === 200 && crypto.sha256(r.body, "hex") === data.gzNarHex });
}
